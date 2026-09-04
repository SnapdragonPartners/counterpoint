package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/counterpoint/internal/appserver"
	"github.com/SnapdragonPartners/counterpoint/internal/gitrepo"
	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

// fakeReviewer records calls and answers from a script.
type fakeReviewer struct {
	mu           sync.Mutex
	started      []string
	resumed      []string
	instructions []string
	closed       int

	resumeErr     error
	reviewErr     error
	blockUntilCtx bool
	// reviewStarted is closed the first time Review is called.
	reviewStarted chan struct{}
	startOnce     sync.Once
	stallSetup    bool // StartThread and ResumeThread block until ctx ends
	warnings      []string
}

func (f *fakeReviewer) StartThread(ctx context.Context, cwd string) (appserver.Thread, error) {
	if f.stallSetup {
		<-ctx.Done()
		return appserver.Thread{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, cwd)
	return appserver.Thread{ID: fmt.Sprintf("thr_%d", len(f.started)), Model: "fake", ReasoningEffort: "xhigh"}, nil
}

func (f *fakeReviewer) ResumeThread(ctx context.Context, id, cwd string) (appserver.Thread, error) {
	if f.stallSetup {
		<-ctx.Done()
		return appserver.Thread{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resumeErr != nil {
		return appserver.Thread{}, f.resumeErr
	}
	f.resumed = append(f.resumed, id+"@"+cwd)
	return appserver.Thread{ID: id, Model: "fake", ReasoningEffort: "xhigh"}, nil
}

func (f *fakeReviewer) Review(ctx context.Context, threadID, instructions string) (*appserver.Review, error) {
	f.startOnce.Do(func() {
		if f.reviewStarted != nil {
			close(f.reviewStarted)
		}
	})
	f.mu.Lock()
	f.instructions = append(f.instructions, instructions)
	block, err, warnings := f.blockUntilCtx, f.reviewErr, f.warnings
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, fmt.Errorf("%w: %w", appserver.ErrTurnInterrupted, ctx.Err())
	}
	if err != nil {
		return nil, err
	}
	first, _, _ := strings.Cut(instructions, "\n")
	return &appserver.Review{TurnID: "turn_1", Text: "REVIEW on " + threadID + ": " + first, Warnings: warnings}, nil
}

func (f *fakeReviewer) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

type harness struct {
	repo     *testRepo
	store    *state.Store
	reviewer *fakeReviewer
	spawns   int
	svc      *Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{repo: newTestRepo(t), reviewer: &fakeReviewer{}}
	h.store = state.NewStore(filepath.Join(t.TempDir(), "state", "state.json"))
	h.svc = New(Options{
		Store:  h.store,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		NewReviewer: func(context.Context) (Reviewer, error) {
			h.spawns++
			return h.reviewer, nil
		},
	})
	return h
}

func (h *harness) request(commit, notes string) Request {
	return Request{Repo: h.repo.dir, Branch: "feature", Commit: commit, BranchNotes: notes}
}

func TestFirstRoundStartsThreadAndPersists(t *testing.T) {
	h := newHarness(t)
	tip := h.repo.git("rev-parse", "HEAD")
	res, err := h.svc.Review(context.Background(), h.request(tip[:8], "round one notes"))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Round != 1 || res.Commit != tip || res.Branch != "refs/heads/feature" || res.Repo != h.repo.dir || res.Replayed {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.Review, "review round 1 of refs/heads/feature") {
		t.Errorf("review text = %q", res.Review)
	}
	if len(h.reviewer.started) != 1 || h.reviewer.started[0] != h.repo.dir || len(h.reviewer.resumed) != 0 {
		t.Errorf("thread calls: started %v resumed %v", h.reviewer.started, h.reviewer.resumed)
	}
	if h.reviewer.closed != 1 {
		t.Errorf("reviewer closed %d times, want 1", h.reviewer.closed)
	}
	if !strings.Contains(h.reviewer.instructions[0], "round one notes") {
		t.Error("branch notes missing from instructions")
	}

	st, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := gitrepo.Open(context.Background(), h.repo.dir)
	wf, ok := st.Get(gitrepo.WorkflowKey(repo.Identity(), "refs/heads/feature"))
	if !ok || wf.ThreadID != "thr_1" || wf.Round != 1 || wf.LastCommit != tip || wf.LastBase != res.Base || wf.LastReview != res.Review {
		t.Errorf("persisted workflow = %+v", wf)
	}
}

func TestIdenticalRequestReplaysWithoutSpawning(t *testing.T) {
	h := newHarness(t)
	tip := h.repo.git("rev-parse", "HEAD")
	first, err := h.svc.Review(context.Background(), h.request(tip, "notes"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.svc.Review(context.Background(), h.request(tip[:7], "notes"))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Review != first.Review || again.Round != 1 {
		t.Errorf("replay = %+v", again)
	}
	if h.spawns != 1 {
		t.Errorf("reviewer spawned %d times, want 1", h.spawns)
	}

	// Changed notes on the same commit is a new round.
	third, err := h.svc.Review(context.Background(), h.request(tip, "revised notes"))
	if err != nil {
		t.Fatal(err)
	}
	if third.Replayed || third.Round != 2 || h.spawns != 2 {
		t.Errorf("revised notes: replayed=%v round=%d spawns=%d", third.Replayed, third.Round, h.spawns)
	}
}

func TestSecondRoundResumesThreadAndNamesPreviousTip(t *testing.T) {
	h := newHarness(t)
	first := h.repo.git("rev-parse", "HEAD")
	if _, err := h.svc.Review(context.Background(), h.request(first, "r1")); err != nil {
		t.Fatal(err)
	}
	second := h.repo.commit("feature-2")

	// A fresh service over the same store, as after a restart.
	restarted := New(Options{Store: state.NewStore(h.store.Path()), NewReviewer: func(context.Context) (Reviewer, error) { return h.reviewer, nil }})
	res, err := restarted.Review(context.Background(), h.request(second, "r2"))
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if res.Round != 2 || !strings.Contains(res.Review, "on thr_1:") {
		t.Errorf("second round result = %+v", res)
	}
	if len(h.reviewer.resumed) != 1 || !strings.HasPrefix(h.reviewer.resumed[0], "thr_1@") {
		t.Errorf("resumed = %v, want thr_1", h.reviewer.resumed)
	}
	instr := h.reviewer.instructions[1]
	if !strings.Contains(instr, "previously reviewed tip was "+first) || !strings.Contains(instr, "git diff "+first+" "+second) {
		t.Errorf("round two instructions lack the previous tip delta:\n%s", instr)
	}
}

func TestRewrittenHistoryIsReportedToReviewer(t *testing.T) {
	h := newHarness(t)
	first := h.repo.git("rev-parse", "HEAD")
	if _, err := h.svc.Review(context.Background(), h.request(first, "r1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.repo.dir, "feature-1.txt"), []byte("amended\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.repo.git("add", "-A")
	h.repo.git("commit", "--quiet", "--amend", "--no-edit")
	amended := h.repo.git("rev-parse", "HEAD")

	if _, err := h.svc.Review(context.Background(), h.request(amended, "r2")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.reviewer.instructions[1], "history was rewritten") {
		t.Error("rewritten history not reported in instructions")
	}
}

func TestResumeFailureFailsClosed(t *testing.T) {
	h := newHarness(t)
	first := h.repo.git("rev-parse", "HEAD")
	if _, err := h.svc.Review(context.Background(), h.request(first, "r1")); err != nil {
		t.Fatal(err)
	}
	h.reviewer.resumeErr = errors.New("thread not found")
	second := h.repo.commit("feature-2")
	_, err := h.svc.Review(context.Background(), h.request(second, "r2"))
	if !errors.Is(err, ErrThreadUnavailable) || !strings.Contains(err.Error(), h.store.Path()) || !strings.Contains(err.Error(), "refs/heads/feature") {
		t.Fatalf("error = %v, want ErrThreadUnavailable naming the workflow and state file", err)
	}
	if len(h.reviewer.started) != 1 {
		t.Errorf("a replacement thread was started: %v", h.reviewer.started)
	}
	st, _ := h.store.Load()
	repo, _ := gitrepo.Open(context.Background(), h.repo.dir)
	if wf, _ := st.Get(gitrepo.WorkflowKey(repo.Identity(), "refs/heads/feature")); wf.Round != 1 || wf.LastCommit != first {
		t.Errorf("state changed by a failed round: %+v", wf)
	}
}

func TestFailedTurnPersistsNothing(t *testing.T) {
	h := newHarness(t)
	tip := h.repo.git("rev-parse", "HEAD")
	h.reviewer.reviewErr = fmt.Errorf("%w: boom", appserver.ErrTurnFailed)
	_, err := h.svc.Review(context.Background(), h.request(tip, "r1"))
	if !errors.Is(err, appserver.ErrTurnFailed) {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(h.store.Path()); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("state written for a failed turn: %v", statErr)
	}
	if h.reviewer.closed != 1 {
		t.Errorf("reviewer not closed after failure")
	}
}

func TestTimeoutInterruptsAndReports(t *testing.T) {
	h := newHarness(t)
	h.svc.timeout = 200 * time.Millisecond
	h.reviewer.blockUntilCtx = true
	tip := h.repo.git("rev-parse", "HEAD")
	start := time.Now()
	_, err := h.svc.Review(context.Background(), h.request(tip, "r1"))
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, appserver.ErrTurnInterrupted) {
		t.Fatalf("error = %v, want ErrTimeout and ErrTurnInterrupted", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not honored")
	}
}

func TestLockContentionFailsClearly(t *testing.T) {
	h := newHarness(t)
	held, err := state.AcquireLock(context.Background(), h.store.LockPath(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	tip := h.repo.git("rev-parse", "HEAD")
	_, err = h.svc.Review(context.Background(), h.request(tip, "r1"))
	if !errors.Is(err, state.ErrLocked) {
		t.Fatalf("error = %v, want ErrLocked", err)
	}
	if h.spawns != 0 {
		t.Error("reviewer spawned while the lock was held elsewhere")
	}
}

func TestValidationErrorsSpawnNothing(t *testing.T) {
	h := newHarness(t)
	tip := h.repo.git("rev-parse", "HEAD")
	cases := map[string]Request{
		"missing notes":  h.request(tip, ""),
		"primary branch": {Repo: h.repo.dir, Branch: "main", Commit: "HEAD", BranchNotes: "n"},
		"not tip":        h.request(h.repo.git("rev-parse", "HEAD~1"), "n"),
		"relative repo":  {Repo: "relative", Branch: "feature", Commit: tip, BranchNotes: "n"},
	}
	for name, req := range cases {
		if _, err := h.svc.Review(context.Background(), req); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if h.spawns != 0 {
		t.Errorf("reviewer spawned %d times for invalid requests", h.spawns)
	}
}

func TestWarningsAreReturnedAndPersisted(t *testing.T) {
	h := newHarness(t)
	h.reviewer.warnings = []string{"declined app-server request x"}
	tip := h.repo.git("rev-parse", "HEAD")
	res, err := h.svc.Review(context.Background(), h.request(tip, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 {
		t.Errorf("warnings = %v", res.Warnings)
	}
	again, _ := h.svc.Review(context.Background(), h.request(tip, "r1"))
	if !again.Replayed || len(again.Warnings) != 1 {
		t.Errorf("replayed warnings = %v", again.Warnings)
	}
}

func TestMovedMergeBaseIsANewRound(t *testing.T) {
	h := newHarness(t)
	tip := h.repo.git("rev-parse", "HEAD")
	if _, err := h.svc.Review(context.Background(), h.request(tip, "notes")); err != nil {
		t.Fatal(err)
	}
	// Advance the primary branch without touching the feature tip, then
	// come back; the reviewed diff changed even though tip and notes did not.
	h.repo.git("checkout", "--quiet", "main")
	h.repo.commit("main-2")
	h.repo.git("merge", "--quiet", "--no-ff", "--no-edit", "feature")
	mainTip := h.repo.git("rev-parse", "HEAD")
	h.repo.git("checkout", "--quiet", "feature")

	res, err := h.svc.Review(context.Background(), h.request(tip, "notes"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || res.Round != 2 || res.Base != tip {
		t.Errorf("after primary moved to %s: replayed=%v round=%d base=%s", mainTip, res.Replayed, res.Round, res.Base)
	}
}

func TestIncompleteStoredWorkflowFailsClosed(t *testing.T) {
	complete := func(tip, hash string) state.Workflow {
		return state.Workflow{ThreadID: "thr_1", LastCommit: tip, LastBase: "b", LastRequestHash: hash, Round: 1, LastReview: "prior review"}
	}
	cases := map[string]func(w *state.Workflow){
		"thread_id":         func(w *state.Workflow) { w.ThreadID = "" },
		"last_commit":       func(w *state.Workflow) { w.LastCommit = "" },
		"last_base":         func(w *state.Workflow) { w.LastBase = "" },
		"last_request_hash": func(w *state.Workflow) { w.LastRequestHash = "" },
		"last_review":       func(w *state.Workflow) { w.LastReview = "" },
		"round":             func(w *state.Workflow) { w.Round = 0 },
	}
	for field, damage := range cases {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t)
			tip := h.repo.git("rev-parse", "HEAD")
			repo, _ := gitrepo.Open(context.Background(), h.repo.dir)
			key := gitrepo.WorkflowKey(repo.Identity(), "refs/heads/feature")
			// The stored hash matches the request so that a damaged review
			// would otherwise replay, and a damaged hash would otherwise run.
			target, _ := repo.ValidateTarget(context.Background(), "feature", tip, "")
			hash := state.Request{Identity: repo.Identity(), BranchRef: "refs/heads/feature", Commit: tip, Base: target.Base, BranchNotes: "notes"}.Hash()
			wf := complete(tip, hash)
			damage(&wf)
			st, _ := h.store.Load()
			st.Put(key, wf)
			if err := h.store.Save(st); err != nil {
				t.Fatal(err)
			}

			res, err := h.svc.Review(context.Background(), h.request(tip, "notes"))
			if !errors.Is(err, ErrStateInvalid) || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), h.store.Path()) {
				t.Fatalf("error = %v (result %+v), want ErrStateInvalid naming the workflow and state file", err, res)
			}
			if strings.Contains(err.Error(), "prior review") || strings.Contains(err.Error(), "thr_1") {
				t.Errorf("error echoes stored values: %v", err)
			}
			if h.spawns != 0 {
				t.Error("a Codex turn was attempted on incomplete state")
			}
		})
	}
}

func TestSetupTimeoutCoversThreadCalls(t *testing.T) {
	for _, resume := range []bool{false, true} {
		h := newHarness(t)
		tip := h.repo.git("rev-parse", "HEAD")
		if resume {
			if _, err := h.svc.Review(context.Background(), h.request(tip, "r1")); err != nil {
				t.Fatal(err)
			}
			tip = h.repo.commit("feature-2")
		}
		h.svc.setupTimeout = 200 * time.Millisecond
		h.reviewer.stallSetup = true
		before := h.reviewer.closed

		start := time.Now()
		_, err := h.svc.Review(context.Background(), h.request(tip, "r2"))
		if !errors.Is(err, ErrSetupTimeout) {
			t.Fatalf("resume=%v: error = %v, want ErrSetupTimeout", resume, err)
		}
		if errors.Is(err, ErrThreadUnavailable) {
			t.Errorf("resume=%v: error = %v; a transient setup timeout must not claim the thread is unavailable", resume, err)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("resume=%v: setup stall not bounded", resume)
		}
		if h.reviewer.closed != before+1 {
			t.Errorf("resume=%v: reviewer not closed after setup timeout", resume)
		}
		if l, err := state.AcquireLock(context.Background(), h.store.LockPath(), time.Second); err != nil {
			t.Errorf("resume=%v: lock not released: %v", resume, err)
		} else {
			_ = l.Release()
		}
	}
}

func TestOversizedBranchNotesAreRejected(t *testing.T) {
	h := newHarness(t)
	tip := h.repo.git("rev-parse", "HEAD")
	_, err := h.svc.Review(context.Background(), h.request(tip, strings.Repeat("n", MaxBranchNotesBytes+1)))
	if !errors.Is(err, ErrInvalidRequest) || h.spawns != 0 {
		t.Fatalf("error = %v, spawns = %d; want ErrInvalidRequest and no spawn", err, h.spawns)
	}
}

// lockProbeReviewer records, when closed, whether the review lock was still
// held: Close must run before the lock is released.
type lockProbeReviewer struct {
	fakeReviewer
	lockPath    string
	heldOnClose bool
}

func (l *lockProbeReviewer) Close() {
	_, err := state.AcquireLock(context.Background(), l.lockPath, 0)
	l.heldOnClose = errors.Is(err, state.ErrLocked)
	l.fakeReviewer.Close()
}

func TestCancellationClosesReviewerBeforeReleasingLock(t *testing.T) {
	h := newHarness(t)
	probe := &lockProbeReviewer{lockPath: h.store.LockPath()}
	probe.blockUntilCtx = true
	probe.reviewStarted = make(chan struct{})
	h.svc.newReviewer = func(context.Context) (Reviewer, error) { return probe, nil }
	tip := h.repo.git("rev-parse", "HEAD")

	// Cancel only once the review is actually in progress, so the
	// cancellation exercises the turn path rather than setup.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-probe.reviewStarted; cancel() }()
	_, err := h.svc.Review(ctx, h.request(tip, "r1"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if probe.closed != 1 || !probe.heldOnClose {
		t.Errorf("closed=%d heldOnClose=%v; want the reviewer closed while the lock was still held", probe.closed, probe.heldOnClose)
	}
	if l, err := state.AcquireLock(context.Background(), h.store.LockPath(), time.Second); err != nil {
		t.Errorf("lock not released after cancellation: %v", err)
	} else {
		_ = l.Release()
	}
}

func TestUnincrementableRoundFailsClosed(t *testing.T) {
	h := newHarness(t)
	tip := h.repo.git("rev-parse", "HEAD")
	repo, _ := gitrepo.Open(context.Background(), h.repo.dir)
	key := gitrepo.WorkflowKey(repo.Identity(), "refs/heads/feature")
	st, _ := h.store.Load()
	st.Put(key, state.Workflow{ThreadID: "thr_1", LastCommit: tip, LastBase: "b", LastRequestHash: "h", Round: math.MaxInt, LastReview: "r"})
	if err := h.store.Save(st); err != nil {
		t.Fatal(err)
	}

	_, err := h.svc.Review(context.Background(), h.request(h.repo.commit("feature-2"), "r2"))
	if !errors.Is(err, ErrStateInvalid) || !strings.Contains(err.Error(), "incremented") {
		t.Fatalf("error = %v, want ErrStateInvalid about the round", err)
	}
	if h.spawns != 0 {
		t.Error("a Codex turn was attempted with an unincrementable round")
	}
	reloaded, _ := h.store.Load()
	if wf, _ := reloaded.Get(key); wf.Round != math.MaxInt {
		t.Errorf("state changed: round %d", wf.Round)
	}
}
