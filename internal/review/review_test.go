package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/counterpoint/internal/appserver"
	"github.com/SnapdragonPartners/counterpoint/internal/gitrepo"
	"github.com/SnapdragonPartners/counterpoint/internal/scratch"
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
	omitEffort    bool // ResumeThread reports no reasoning effort, as codex-cli 0.153.1 does
	warnings      []string

	sandboxes   []appserver.Sandbox // one per StartThread or ResumeThread
	currentCwd  string
	extraArgs   [][]string       // per NewReviewer call
	duringSetup func(cwd string) // observes the thread cwd while it exists
	duringTurn  func(cwd string) // runs inside Review, e.g. to modify the checkout

	unarchived     []string
	unarchiveErr   error              // nil: unarchive succeeds and the thread resumes afterwards
	cancelOnResume context.CancelFunc // called before a scripted resume failure is returned
	named          []string
	nameErr        error
}

func (f *fakeReviewer) StartThread(ctx context.Context, cwd string, sb appserver.Sandbox) (appserver.Thread, error) {
	if f.stallSetup {
		<-ctx.Done()
		return appserver.Thread{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, cwd)
	f.sandboxes = append(f.sandboxes, sb)
	f.currentCwd = cwd
	if f.duringSetup != nil {
		f.duringSetup(cwd)
	}
	return appserver.Thread{ID: fmt.Sprintf("thr_%d", len(f.started)), Model: "fake", ReasoningEffort: "xhigh"}, nil
}

func (f *fakeReviewer) ResumeThread(ctx context.Context, id, cwd string, sb appserver.Sandbox) (appserver.Thread, error) {
	if f.stallSetup {
		<-ctx.Done()
		return appserver.Thread{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resumeErr != nil {
		if f.cancelOnResume != nil {
			// The request ends just as the refusal arrives.
			f.cancelOnResume()
		}
		return appserver.Thread{}, f.resumeErr
	}
	f.resumed = append(f.resumed, id+"@"+cwd)
	f.sandboxes = append(f.sandboxes, sb)
	f.currentCwd = cwd
	if f.duringSetup != nil {
		f.duringSetup(cwd)
	}
	th := appserver.Thread{ID: id, Model: "fake", ReasoningEffort: "xhigh"}
	if f.omitEffort {
		th.ReasoningEffort = ""
	}
	return th, nil
}

func (f *fakeReviewer) UnarchiveThread(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unarchived = append(f.unarchived, id)
	if f.unarchiveErr != nil {
		return f.unarchiveErr
	}
	f.resumeErr = nil
	return nil
}

func (f *fakeReviewer) SetThreadName(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.named = append(f.named, id+":"+name)
	return f.nameErr
}

func (f *fakeReviewer) AddWarning(w string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warnings = append(f.warnings, w)
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
	during := f.duringTurn
	f.mu.Unlock()
	if during != nil {
		during(f.lastCwd())
	}
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

// lastCwd is the cwd of the most recent StartThread or ResumeThread.
func (f *fakeReviewer) lastCwd() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currentCwd
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
		NewReviewer: func(_ context.Context, extraArgs []string) (Reviewer, error) {
			h.spawns++
			h.reviewer.mu.Lock()
			h.reviewer.extraArgs = append(h.reviewer.extraArgs, extraArgs)
			h.reviewer.mu.Unlock()
			return h.reviewer, nil
		},
		CheckoutRoot: filepath.Join(resolvedTempDir(t), "checkouts"),
	})
	return h
}

// resolvedTempDir is a fresh temp dir with symlinks resolved, so paths the
// service canonicalizes compare equal to it.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
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
	restarted := New(Options{Store: state.NewStore(h.store.Path()), NewReviewer: func(context.Context, []string) (Reviewer, error) { return h.reviewer, nil }})
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

// A resumed round whose thread/resume response omits reasoningEffort must
// still log the configured effort in force, and must label the missing
// value as unreported rather than presenting it as the effort (issue #7).
func TestResumedRoundLogsConfiguredEffortWhenUnreported(t *testing.T) {
	h := newHarness(t)
	first := h.repo.git("rev-parse", "HEAD")
	if _, err := h.svc.Review(context.Background(), h.request(first, "r1")); err != nil {
		t.Fatal(err)
	}
	second := h.repo.commit("feature-2")

	var logs bytes.Buffer
	h.reviewer.omitEffort = true
	restarted := New(Options{
		Store:       state.NewStore(h.store.Path()),
		Logger:      slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		NewReviewer: func(context.Context, []string) (Reviewer, error) { return h.reviewer, nil },
	})
	if _, err := restarted.Review(context.Background(), h.request(second, "r2")); err != nil {
		t.Fatalf("second round: %v", err)
	}
	var starting string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="review turn starting"`) && strings.Contains(line, "round=2") {
			starting = line
		}
	}
	if starting == "" {
		t.Fatalf("no round-two start line in logs:\n%s", logs.String())
	}
	if !strings.Contains(starting, "effort="+ReasoningEffort+" ") {
		t.Errorf("configured effort %q missing from: %s", ReasoningEffort, starting)
	}
	if !strings.Contains(starting, `reported_effort="" `) {
		t.Errorf("omitted effort not labelled as unreported in: %s", starting)
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

// writerHeld is codex-cli 0.153.1's refusal when another process, such as
// the Codex app with the thread open, holds the thread's writer. Both
// thread/resume and thread/unarchive answer with it.
var writerHeld = &appserver.ServerError{Code: -32600, Message: "thread thr_1 already has an active writer"}

func TestResumeFailureFailsClosed(t *testing.T) {
	h := newHarness(t)
	first := h.repo.git("rev-parse", "HEAD")
	if _, err := h.svc.Review(context.Background(), h.request(first, "r1")); err != nil {
		t.Fatal(err)
	}
	h.reviewer.resumeErr = writerHeld
	h.reviewer.unarchiveErr = writerHeld
	second := h.repo.commit("feature-2")
	_, err := h.svc.Review(context.Background(), h.request(second, "r2"))
	if !errors.Is(err, ErrThreadUnavailable) || !strings.Contains(err.Error(), h.store.Path()) || !strings.Contains(err.Error(), "refs/heads/feature") {
		t.Fatalf("error = %v, want ErrThreadUnavailable naming the workflow and state file", err)
	}
	if !strings.Contains(err.Error(), "archive it there and retry") || strings.Contains(err.Error(), "close") {
		t.Errorf("error = %v, want the archive handoff hint and no advice to close the thread", err)
	}
	if len(h.reviewer.unarchived) != 1 {
		t.Errorf("unarchive attempts = %v, want exactly one", h.reviewer.unarchived)
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

func TestArchivedThreadIsUnarchivedAndResumed(t *testing.T) {
	h := newHarness(t)
	first := h.repo.git("rev-parse", "HEAD")
	if _, err := h.svc.Review(context.Background(), h.request(first, "r1")); err != nil {
		t.Fatal(err)
	}
	h.reviewer.resumeErr = &appserver.ServerError{Code: -32600, Message: "session thr_1 is archived. Run `codex unarchive thr_1` to unarchive it first."}
	second := h.repo.commit("feature-2")
	res, err := h.svc.Review(context.Background(), h.request(second, "r2"))
	if err != nil {
		t.Fatalf("round two after archive: %v", err)
	}
	if res.Round != 2 || !strings.Contains(res.Review, "thr_1") {
		t.Fatalf("result = %+v, want round 2 on the stored thread", res)
	}
	if got := h.reviewer.unarchived; len(got) != 1 || got[0] != "thr_1" {
		t.Errorf("unarchived = %v, want [thr_1]", got)
	}
	if len(h.reviewer.resumed) != 1 || len(h.reviewer.started) != 1 {
		t.Errorf("resumed %v started %v; want one successful resume and no replacement thread", h.reviewer.resumed, h.reviewer.started)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "unarchived") {
		t.Errorf("warnings = %v, want one saying the thread was unarchived", res.Warnings)
	}
	st, _ := h.store.Load()
	repo, _ := gitrepo.Open(context.Background(), h.repo.dir)
	wf, _ := st.Get(gitrepo.WorkflowKey(repo.Identity(), "refs/heads/feature"))
	if wf.Round != 2 || len(wf.LastWarnings) != 1 {
		t.Errorf("persisted workflow = %+v, want round 2 with the warning", wf)
	}
}

func TestTransportResumeFailureSkipsUnarchive(t *testing.T) {
	h := newHarness(t)
	first := h.repo.git("rev-parse", "HEAD")
	if _, err := h.svc.Review(context.Background(), h.request(first, "r1")); err != nil {
		t.Fatal(err)
	}
	// The server never answered, so nothing says the thread is archived
	// and an unarchive attempt would be a guess against a dead process.
	h.reviewer.resumeErr = fmt.Errorf("thread/resume: %w", appserver.ErrProcessExited)
	second := h.repo.commit("feature-2")
	_, err := h.svc.Review(context.Background(), h.request(second, "r2"))
	if !errors.Is(err, ErrThreadUnavailable) {
		t.Fatalf("error = %v, want ErrThreadUnavailable", err)
	}
	if len(h.reviewer.unarchived) != 0 {
		t.Errorf("unarchive attempted after a transport failure: %v", h.reviewer.unarchived)
	}
}

func TestCancelledResumeDoesNotUnarchive(t *testing.T) {
	h := newHarness(t)
	first := h.repo.git("rev-parse", "HEAD")
	if _, err := h.svc.Review(context.Background(), h.request(first, "r1")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.reviewer.resumeErr = &appserver.ServerError{Code: -32600, Message: "session thr_1 is archived. Run `codex unarchive thr_1` to unarchive it first."}
	h.reviewer.cancelOnResume = cancel
	second := h.repo.commit("feature-2")
	_, err := h.svc.Review(ctx, h.request(second, "r2"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if len(h.reviewer.unarchived) != 0 {
		t.Errorf("an aborted review changed the thread's archival state: %v", h.reviewer.unarchived)
	}
}

func TestNewThreadIsNamedForTheWorkflow(t *testing.T) {
	h := newHarness(t)
	first := h.repo.git("rev-parse", "HEAD")
	res, err := h.svc.Review(context.Background(), h.request(first, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	want := "thr_1:Counterpoint review: " + filepath.Base(h.repo.dir) + " feature"
	if got := h.reviewer.named; len(got) != 1 || got[0] != want {
		t.Errorf("named = %v, want [%s]", got, want)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", res.Warnings)
	}

	// Naming is cosmetic: a refusal is reported, not fatal, and a resumed
	// thread is not renamed.
	h.reviewer.nameErr = &appserver.ServerError{Code: -32600, Message: "thread names are disabled"}
	second := h.repo.commit("feature-2")
	res, err = h.svc.Review(context.Background(), h.request(second, "r2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.reviewer.named) != 1 {
		t.Errorf("resumed thread was renamed: %v", h.reviewer.named)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none on a resumed round", res.Warnings)
	}
	h2 := newHarness(t)
	h2.reviewer.nameErr = h.reviewer.nameErr
	res, err = h2.svc.Review(context.Background(), h2.request(h2.repo.git("rev-parse", "HEAD"), "r1"))
	if err != nil {
		t.Fatalf("review with a refused name: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "could not be named") {
		t.Errorf("warnings = %v, want one about the name", res.Warnings)
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
	h.svc.newReviewer = func(context.Context, []string) (Reviewer, error) { return probe, nil }
	tip := h.repo.git("rev-parse", "HEAD")

	// Cancel only once the review is actually in progress, so the
	// cancellation exercises the turn path rather than setup.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-probe.reviewStarted:
			cancel()
		case <-ctx.Done(): // the test ended first; nothing to wait for
		}
	}()
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

// buildRequest is request with the build flag set.
func (h *harness) buildRequest(commit, notes string) Request {
	r := h.request(commit, notes)
	r.Build = true
	return r
}

// gitIn runs a Git command in dir for assertions about a checkout.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = cleanGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestBuildRoundRunsInADisposableCheckout(t *testing.T) {
	h := newHarness(t)
	commit := h.repo.git("rev-parse", "HEAD")
	var seenCwd string
	h.reviewer.duringSetup = func(cwd string) {
		seenCwd = cwd
		if _, err := os.Stat(filepath.Join(cwd, scratch.TmpName)); err != nil {
			t.Errorf("temp dir missing in the checkout while the thread exists: %v", err)
		}
		if head := gitIn(t, cwd, "rev-parse", "HEAD"); head != commit {
			t.Errorf("checkout HEAD = %s, want %s", head, commit)
		}
	}
	res, err := h.svc.Review(context.Background(), h.buildRequest(commit, "build me"))
	if err != nil {
		t.Fatalf("build round: %v", err)
	}
	if seenCwd == "" || seenCwd == h.repo.dir || !strings.HasPrefix(seenCwd, h.svc.checkoutRoot) {
		t.Fatalf("thread cwd = %q, want a checkout under %s and not the worktree", seenCwd, h.svc.checkoutRoot)
	}
	if _, err := os.Stat(seenCwd); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout %s still exists after the review: %v", seenCwd, err)
	}
	cacheDir := filepath.Join(filepath.Dir(seenCwd), "cache")
	if _, err := os.Stat(cacheDir); err != nil {
		t.Errorf("cache dir %s missing after the review: %v", cacheDir, err)
	}
	sb := h.reviewer.sandboxes[0]
	if !sb.Build || len(sb.WritableRoots) != 1 || sb.WritableRoots[0] != cacheDir {
		t.Errorf("sandbox = %+v, want build with the cache dir as the only root", sb)
	}
	args := strings.Join(h.reviewer.extraArgs[0], " ")
	if !strings.Contains(args, "sandbox_workspace_write.writable_roots=[") || !strings.Contains(args, "TMPDIR=") {
		t.Errorf("reviewer args %q lack the sandbox overrides", args)
	}
	inst := h.reviewer.instructions[0]
	if !strings.Contains(inst, "disposable checkout") || !strings.Contains(inst, seenCwd) || !strings.Contains(inst, cacheDir) {
		t.Errorf("prompt does not describe the checkout:\n%s", inst)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", res.Warnings)
	}
	if status := h.repo.git("status", "--porcelain"); status != "" {
		t.Errorf("worktree modified by a build review:\n%s", status)
	}

	// Identical build request replays; the same commit and notes without
	// the flag is a new, read-only round on the same thread.
	h.reviewer.duringSetup = nil
	replay, err := h.svc.Review(context.Background(), h.buildRequest(commit, "build me"))
	if err != nil || !replay.Replayed {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	ro, err := h.svc.Review(context.Background(), h.request(commit, "build me"))
	if err != nil || ro.Replayed || ro.Round != 2 {
		t.Fatalf("read-only round = %+v, %v; want a new round 2", ro, err)
	}
	if got := h.reviewer.resumed; len(got) != 1 || !strings.HasSuffix(got[0], "@"+h.repo.dir) {
		t.Errorf("resumed = %v, want one resume on the worktree", got)
	}
	if sb := h.reviewer.sandboxes[1]; sb.Build || h.reviewer.extraArgs[1] != nil {
		t.Errorf("read-only round used build settings: %+v %v", sb, h.reviewer.extraArgs[1])
	}
}

func TestBuildCheckoutIsRemovedWhenTheTurnFails(t *testing.T) {
	h := newHarness(t)
	h.reviewer.reviewErr = errors.New("turn failed")
	var cwd string
	h.reviewer.duringSetup = func(c string) { cwd = c }
	if _, err := h.svc.Review(context.Background(), h.buildRequest(h.repo.git("rev-parse", "HEAD"), "n")); err == nil {
		t.Fatal("expected the turn failure")
	}
	if cwd == "" {
		t.Fatal("thread was never started")
	}
	if _, err := os.Stat(cwd); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout survived a failed turn: %v", err)
	}
}

func TestBuildCheckoutIsRemovedOnCancellation(t *testing.T) {
	h := newHarness(t)
	h.reviewer.blockUntilCtx = true
	h.reviewer.reviewStarted = make(chan struct{})
	var cwd string
	h.reviewer.duringSetup = func(c string) { cwd = c }
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-h.reviewer.reviewStarted
		cancel()
	}()
	_, err := h.svc.Review(ctx, h.buildRequest(h.repo.git("rev-parse", "HEAD"), "n"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if _, err := os.Stat(cwd); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout survived cancellation: %v", err)
	}
	if h.reviewer.closed != 1 {
		t.Errorf("reviewer closed %d times, want 1", h.reviewer.closed)
	}
}

func TestBuildRoundReportsTrackedFilesTheReviewerChanged(t *testing.T) {
	h := newHarness(t)
	h.reviewer.duringTurn = func(cwd string) {
		// The test repository's commits write feature-1 style files; append
		// to whichever tracked file comes first.
		name := strings.Split(gitIn(t, cwd, "ls-files"), "\n")[0]
		f, err := os.OpenFile(filepath.Join(cwd, name), os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("reviewer edit\n")
		_ = f.Close()
	}
	res, err := h.svc.Review(context.Background(), h.buildRequest(h.repo.git("rev-parse", "HEAD"), "n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "changed 1 tracked file") {
		t.Errorf("warnings = %v, want one about the changed tracked file", res.Warnings)
	}
	st, _ := h.store.Load()
	repo, _ := gitrepo.Open(context.Background(), h.repo.dir)
	if wf, _ := st.Get(gitrepo.WorkflowKey(repo.Identity(), "refs/heads/feature")); len(wf.LastWarnings) != 1 {
		t.Errorf("persisted warnings = %v, want the integrity warning", wf.LastWarnings)
	}
}

func TestReadOnlyRoundTouchesNoScratch(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Review(context.Background(), h.request(h.repo.git("rev-parse", "HEAD"), "n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.svc.checkoutRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("read-only review created the scratch root: %v", err)
	}
	if h.reviewer.extraArgs[0] != nil || h.reviewer.sandboxes[0].Build {
		t.Errorf("read-only review used build settings")
	}
	if strings.Contains(h.reviewer.instructions[0], "disposable checkout") {
		t.Error("read-only prompt mentions the checkout")
	}
}

func TestBuildRoundFailsBeforeCodexWhenScratchOverlapsTheRepository(t *testing.T) {
	h := newHarness(t)
	h.svc.checkoutRoot = filepath.Join(h.repo.dir, "scratch")
	_, err := h.svc.Review(context.Background(), h.buildRequest(h.repo.git("rev-parse", "HEAD"), "n"))
	if !errors.Is(err, scratch.ErrRootOverlapsRepository) {
		t.Fatalf("error = %v, want ErrRootOverlapsRepository", err)
	}
	if h.spawns != 0 {
		t.Errorf("app-server spawned %d times for a rejected scratch root", h.spawns)
	}
	if status := h.repo.git("status", "--porcelain"); status != "" {
		t.Errorf("worktree modified:\n%s", status)
	}
}
