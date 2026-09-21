// Package review orchestrates one review: it serializes on the review lock,
// validates the target, replays an identical request from state, otherwise
// runs a Codex review on the workflow's persistent thread and persists the
// completed result. It owns the round number and the prompt.
package review

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SnapdragonPartners/counterpoint/internal/appserver"
	"github.com/SnapdragonPartners/counterpoint/internal/gitrepo"
	"github.com/SnapdragonPartners/counterpoint/internal/scratch"
	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

const (
	// Timeout bounds one Codex review turn. It sits below the MCP client's
	// idle timeout so Counterpoint fails first with a clear error.
	Timeout = 20 * time.Minute

	// DefaultReasoningEffort is the reviewer's effort when configuration
	// does not select one, passed to the app-server as a configuration
	// override so the reviewer runs at a deliberate level regardless of
	// the user's interactive setting.
	DefaultReasoningEffort = "high"

	// SetupTimeout bounds everything before the review turn: launching the
	// app-server, its initialize handshake, and thread start or resume.
	// The review timer starts only after setup, so setup needs its own.
	SetupTimeout = 60 * time.Second

	// MaxBranchNotesBytes bounds the author's notes, which are untrusted
	// input carried into the prompt and the persisted state.
	MaxBranchNotesBytes = 1 << 20
)

// acceptedEfforts is what configuration may select, in ascending order.
// The ceiling is deliberate rather than an artifact of the catalog: some
// models advertise levels above xhigh, including one that enables automatic
// delegation a reviewer should not adopt implicitly. Raising the ceiling is
// a policy decision, not a configuration change.
var acceptedEfforts = [...]string{"low", "medium", "high", "xhigh"} //nolint:gochecknoglobals // constant table

// EffortAccepted reports whether s is an effort configuration may select.
func EffortAccepted(s string) bool {
	return slices.Contains(acceptedEfforts[:], s)
}

// AcceptedEfforts returns the accepted efforts, for error messages.
func AcceptedEfforts() []string {
	return acceptedEfforts[:]
}

// Sentinel errors.
var (
	ErrInvalidRequest    = errors.New("invalid review request")
	ErrThreadUnavailable = errors.New("stored Codex thread could not be resumed")
	ErrStateInvalid      = errors.New("stored workflow state is incomplete")
	ErrTimeout           = errors.New("review timed out")
	ErrSetupTimeout      = errors.New("app-server setup timed out")
)

// threadRecoveryHint tells the human what to do when the stored thread
// cannot be resumed. Archiving is the handoff: the Codex app has no way to
// close a thread, only to archive or delete it, and archiving releases the
// thread's writer so the next review can unarchive it and take it over.
// Unarchiving in the app would reopen it there and hold the writer again.
const threadRecoveryHint = "if the thread is open in another Codex process, such as the Codex app, archive it there and retry " +
	"so Counterpoint can unarchive and take it over; if the thread no longer exists, remove the workflow from the state file"

// workflowBusyHint tells the calling agent what to do when this branch is
// already under review. The common self-inflicted case is a call the MCP
// client moved to the background: it is still running and will deliver
// its result, so it must not be retried. A cancelled call is different:
// cancellation interrupts the turn and records nothing, so a retry after
// it is a new round, and the hint promises no replay.
const workflowBusyHint = "a review round takes minutes. " +
	"If you submitted this review yourself and the client moved the call to the background, do not retry: it is still running and its result will be delivered when it completes. " +
	"Otherwise wait for the running round to finish and retry, checking no more often than once a minute"

// stateBusyHint tells the calling agent what to do when the state file is
// locked. The lock is held for moments, so contention is either a
// coincidence or an older Counterpoint holding it for a whole review.
const stateBusyHint = "another Counterpoint process is reading or writing it, which takes moments; retry in ten seconds. " +
	"If this persists, an older Counterpoint is holding the file for a whole review: restart every Claude Code session after installing a new version"

// requestIDKey carries the request correlation id in a context.
type requestIDKey struct{}

// WithRequestID attaches a correlation id that appears in every outcome log
// for the review.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the correlation id, or "-" when none was attached.
func RequestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok && id != "" {
		return id
	}
	return "-"
}

// Reviewer is the app-server session the service drives. The real
// implementation is *appserver.Client; tests substitute a fake so the
// orchestration is covered without a subprocess.
type Reviewer interface {
	StartThread(ctx context.Context, cwd string, sb appserver.Sandbox) (appserver.Thread, error)
	ResumeThread(ctx context.Context, threadID, cwd string, sb appserver.Sandbox) (appserver.Thread, error)
	UnarchiveThread(ctx context.Context, threadID string) error
	SetThreadName(ctx context.Context, threadID, name string) error
	// AddWarning queues a bridge-level warning for the next Review result
	// under the same bounds as the reviewer's own warnings.
	AddWarning(w string)
	Review(ctx context.Context, threadID, instructions string) (*appserver.Review, error)
	Close()
}

// Request is the review tool's validated-at-the-boundary input.
type Request struct {
	Repo        string
	Branch      string
	Commit      string
	BranchNotes string
	// Build asks for a build-capable review: the reviewer works in a
	// disposable checkout of the commit where it may build and run tests.
	// The default is a read-only review of the caller's worktree.
	Build bool
}

// Result is the review tool's output.
type Result struct {
	Repo     string
	Branch   string
	Commit   string
	Base     string
	Round    int
	Review   string
	Warnings []string
	// Replayed is true when an identical completed request was answered
	// from state without a new Codex turn.
	Replayed bool
}

// Options configures a Service.
type Options struct {
	Store *state.Store
	// NewReviewer starts an app-server session for one review. extraArgs
	// are configuration overrides for that session, such as the sandbox
	// settings of a build-capable review. When nil, DefaultReviewer with
	// the codex executable and ReasoningEffort is used.
	NewReviewer func(ctx context.Context, extraArgs []string) (Reviewer, error)
	Logger      *slog.Logger
	Version     string
	// ReasoningEffort is the reviewer's effort; DefaultReasoningEffort
	// when empty. The caller validates it with EffortAccepted before
	// constructing the Service.
	ReasoningEffort string
	// CheckoutRoot is the scratch root for build-capable reviews;
	// scratch.DefaultRoot when empty.
	CheckoutRoot string
}

// Service runs reviews.
type Service struct {
	store        *state.Store
	newReviewer  func(ctx context.Context, extraArgs []string) (Reviewer, error)
	log          *slog.Logger
	timeout      time.Duration
	setupTimeout time.Duration
	checkoutRoot string
	effort       string
	// recordBytes estimates a history record's encoded size for eviction
	// under size pressure; historyRecordBytes unless a test overrides it.
	recordBytes func(state.HistoryRecord) int
}

// New returns a Service.
func New(opts Options) *Service {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	effort := opts.ReasoningEffort
	if effort == "" {
		effort = DefaultReasoningEffort
	}
	nr := opts.NewReviewer
	if nr == nil {
		nr = DefaultReviewer(opts.Version, effort, log)
	}
	return &Service{store: opts.Store, newReviewer: nr, log: log, timeout: Timeout, setupTimeout: SetupTimeout,
		checkoutRoot: opts.CheckoutRoot, effort: effort, recordBytes: historyRecordBytes}
}

// DefaultReviewer starts codex app-server with the configured reasoning
// effort and the session's extra configuration overrides. The handshake is
// bounded by the setup context the service passes; the process lifetime is
// owned by the client's Close.
func DefaultReviewer(version, effort string, log *slog.Logger) func(ctx context.Context, extraArgs []string) (Reviewer, error) {
	return func(ctx context.Context, extraArgs []string) (Reviewer, error) {
		args := append(appserver.DefaultArgs(), "-c", fmt.Sprintf("model_reasoning_effort=%q", effort))
		args = append(args, extraArgs...)
		return appserver.Start(ctx, appserver.Options{
			Command: appserver.DefaultCommand,
			Args:    args,
			Version: version,
			Stderr:  os.Stderr,
			Logger:  log,
		})
	}
}

// Review performs one review round, or replays an identical completed one.
// Every outcome is logged once with the request id, terminal status, and
// duration.
func (s *Service) Review(ctx context.Context, req Request) (*Result, error) {
	start := time.Now()
	res, err := s.review(ctx, req)
	switch {
	case err != nil:
		s.log.Warn("review finished", "request", RequestIDFrom(ctx), "status", "failed", "duration", time.Since(start), "error", err)
	case res.Replayed:
		s.log.Info("review finished", "request", RequestIDFrom(ctx), "status", "replayed", "round", res.Round, "duration", time.Since(start))
	default:
		s.log.Info("review finished", "request", RequestIDFrom(ctx), "status", "completed", "round", res.Round, "duration", time.Since(start), "warnings", len(res.Warnings))
	}
	return res, err
}

func (s *Service) review(ctx context.Context, req Request) (*Result, error) {
	if req.Repo == "" || req.Branch == "" || req.Commit == "" || req.BranchNotes == "" {
		return nil, fmt.Errorf("%w: repo, branch, commit, and branch_notes are all required", ErrInvalidRequest)
	}
	if len(req.BranchNotes) > MaxBranchNotesBytes {
		return nil, fmt.Errorf("%w: branch_notes is %d bytes, limit %d", ErrInvalidRequest, len(req.BranchNotes), MaxBranchNotesBytes)
	}
	requestID := RequestIDFrom(ctx)

	repo, err := gitrepo.Open(ctx, req.Repo)
	if err != nil {
		return nil, err
	}
	branch, err := repo.ResolveBranch(ctx, req.Branch)
	if err != nil {
		return nil, err
	}
	key := gitrepo.WorkflowKey(repo.Identity(), branch.Ref)

	// The workflow lock serializes rounds of this workflow and is held for
	// the whole review, until after the child has exited: Codex's
	// per-thread writer lock is handed over that way. Reviews of other
	// workflows run concurrently (docs/design/per-workflow-locks.md). It is
	// the root of the lock order: the state lock and the scratch directory
	// lock are only ever taken while holding it.
	lock, err := state.AcquireLock(ctx, s.store.WorkflowLockPath(key), state.LockWait)
	if err != nil {
		if errors.Is(err, state.ErrLocked) {
			return nil, fmt.Errorf("another review of branch %s is in progress (%w); %s", branch.Ref, err, workflowBusyHint)
		}
		return nil, err
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			s.log.Warn("release review lock", "error", rerr)
		}
	}()

	wf, known, err := s.loadWorkflow(ctx, key)
	if err != nil {
		return nil, err
	}
	if known {
		// A stored workflow must be a complete record of a finished review;
		// anything else could only come from corruption or a foreign
		// writer. Starting a replacement thread would silently discard
		// review context, and a partial record could replay an empty
		// review or skip the idempotency check.
		if missing := incompleteWorkflowField(wf); missing != "" {
			return nil, fmt.Errorf("%w: workflow %s in %s is missing %s", ErrStateInvalid, key, s.store.Path(), missing)
		}
		// The review history is quoted into the prompt, so a record that
		// breaks the ledger's invariants is refused before any value is
		// interpolated. The description never echoes stored text.
		if bad := wf.InvalidHistory(); bad != "" {
			return nil, fmt.Errorf("%w: workflow %s in %s: %s", ErrStateInvalid, key, s.store.Path(), bad)
		}
		// The next round number must be representable; untrusted state
		// could hold a value whose increment would wrap and persist a
		// corrupted record after a paid review.
		if wf.Round >= math.MaxInt {
			return nil, fmt.Errorf("%w: workflow %s in %s has a round that cannot be incremented", ErrStateInvalid, key, s.store.Path())
		}
	}

	target, err := repo.ValidateTarget(ctx, req.Branch, req.Commit, wf.LastCommit)
	if err != nil {
		return nil, err
	}

	ident := state.Request{
		Identity:    repo.Identity(),
		BranchRef:   branch.Ref,
		Commit:      target.Commit,
		Base:        target.Base,
		BranchNotes: req.BranchNotes,
		Build:       req.Build,
	}
	hash := ident.Hash()
	// The record copied under the state lock is still current: the
	// workflow lock excludes any other round of this workflow.
	if prev, ok := replay(wf, known, hash); ok {
		s.log.Info("review replayed from state", "request", requestID, "workflow", key, "round", prev.Round, "commit", target.Commit)
		return &Result{
			Repo: repo.Worktree, Branch: branch.Ref, Commit: target.Commit, Base: target.Base,
			Round: prev.Round, Review: prev.LastReview, Warnings: prev.LastWarnings, Replayed: true,
		}, nil
	}

	// The repository's own reviewer instructions come from the commit under
	// review, so they are part of the immutable target and, through the
	// commit, of the request identity. An unusable file fails the request
	// before the reviewer is started.
	instructions, err := repo.ReadInstructions(ctx, target.Commit)
	if err != nil {
		return nil, err
	}
	if instructions != "" {
		s.log.Info("project instructions included", "request", requestID, "workflow", key, "file", gitrepo.InstructionsFile, "bytes", len(instructions))
	}

	round := wf.Round + 1
	prompt := Prompt{
		Round: round, Worktree: repo.Worktree, BranchRef: branch.Ref, Commit: target.Commit,
		Base: target.Base, PrimaryName: target.PrimaryName, PrimaryRef: target.PrimaryRef,
		PreviousTip: wf.LastCommit, HistoryRewritten: target.HistoryRewritten, BranchNotes: req.BranchNotes,
		Instructions: instructions,
	}
	// The reviewer starts every round without memory (issue #19), so the
	// retained verdicts and the previous round's review are quoted into
	// the prompt, oldest first, with the evicted rounds disclosed by count.
	var previous state.HistoryRecord
	if known {
		previous = state.NewHistoryRecord(wf.Round, wf.LastCommit, wf.LastBase, wf.LastReview)
		prompt.History = append(append([]state.HistoryRecord{}, wf.History...), previous)
		prompt.OmittedRounds = prompt.History[0].Round - 1
	}

	// A build-capable review runs in a disposable checkout of the commit,
	// never in the caller's worktree. The checkout is prepared inside the
	// review lock and removed on every exit path; its deferred Close runs
	// after the reviewer's, so the child has exited before its cwd goes.
	cwd := repo.Worktree
	var sandbox appserver.Sandbox
	var extraArgs []string
	var checkout *scratch.Checkout
	if req.Build {
		checkout, err = scratch.Prepare(ctx, scratch.Options{
			Root: s.checkoutRoot, WorkflowKey: key, Repo: repo, Commit: target.Commit, Logger: s.log,
		})
		if err != nil {
			return nil, err
		}
		defer func() {
			if cerr := checkout.Close(); cerr != nil {
				s.log.Warn("remove disposable checkout", "request", requestID, "workflow", key, "error", cerr)
			}
		}()
		cwd = checkout.Dir
		sandbox = appserver.Sandbox{Build: true, WritableRoots: []string{checkout.CacheDir, checkout.TmpDir}}
		extraArgs = appserver.BuildConfigArgs(checkout.CacheDir, checkout.TmpDir)
		prompt.Checkout = checkout.Dir
		prompt.CacheDir = checkout.CacheDir
		s.log.Info("disposable checkout ready", "request", requestID, "workflow", key, "checkout", checkout.Dir, "commit", target.Commit)
	}

	// Setup, from spawning the child through the thread call, has its own
	// bound because the review timer has not started yet.
	setupCtx, cancelSetup := context.WithTimeoutCause(ctx, s.setupTimeout, ErrSetupTimeout)
	defer cancelSetup()
	reviewer, err := s.newReviewer(setupCtx, extraArgs)
	if err != nil {
		return nil, setupError(setupCtx, err)
	}
	defer reviewer.Close()

	var thread appserver.Thread
	if known {
		thread, err = reviewer.ResumeThread(setupCtx, wf.ThreadID, cwd, sandbox)
		var refused *appserver.ServerError
		// The setup context is checked before each recovery call: an
		// aborted review must not go on to change the thread's archival
		// state, and a call on an ended context could still be sent.
		if errors.As(err, &refused) && setupCtx.Err() == nil {
			// The app-server refused the resume. An archived thread is the
			// human's handoff (issue #8): archiving in the Codex app
			// releases the thread's writer, and unarchiving from here takes
			// the thread over without reopening it in the app. The attempt
			// does not depend on why the resume failed, because unarchive
			// itself fails for a thread that is not archived or whose
			// writer another process holds; then the resume failure stands.
			if uerr := reviewer.UnarchiveThread(setupCtx, wf.ThreadID); uerr != nil {
				s.log.Info("stored thread was not unarchived", "request", requestID, "workflow", key, "thread", abbreviate(wf.ThreadID), "error", uerr)
			} else if setupCtx.Err() == nil {
				s.log.Info("stored thread unarchived", "request", requestID, "workflow", key, "thread", abbreviate(wf.ThreadID))
				reviewer.AddWarning("the stored Codex thread was archived; Counterpoint unarchived it and resumed the review there")
				thread, err = reviewer.ResumeThread(setupCtx, wf.ThreadID, cwd, sandbox)
			}
		}
		if err != nil {
			// Cancellation and the setup deadline are transient and must
			// not suggest clearing the stored association. Any other resume
			// failure, including a transport failure, is reported as the
			// thread being unavailable, since the caller cannot tell more.
			if setupCtx.Err() != nil {
				return nil, setupError(setupCtx, err)
			}
			return nil, fmt.Errorf("%w: workflow %s in %s: %w; %s", ErrThreadUnavailable, key, s.store.Path(), err, threadRecoveryHint)
		}
	} else {
		thread, err = reviewer.StartThread(setupCtx, cwd, sandbox)
		if err != nil {
			return nil, setupError(setupCtx, err)
		}
		// The name marks the thread in the Codex UIs as Counterpoint's so
		// it is left alone. It is not required for the review. The server's
		// message goes to the log only; the warning is a fixed string so
		// untrusted output cannot fill the warning budget.
		if nerr := reviewer.SetThreadName(setupCtx, thread.ID, threadName(repo.Worktree, branch.Ref)); nerr != nil {
			if setupCtx.Err() != nil {
				return nil, setupError(setupCtx, nerr)
			}
			s.log.Warn("thread not named", "request", requestID, "workflow", key, "thread", abbreviate(thread.ID), "error", nerr)
			reviewer.AddWarning("the new Codex thread could not be named; see the Counterpoint log")
		}
	}
	cancelSetup()
	// The configured effort is logged as the policy in force. The reported
	// value is what thread/start or thread/resume echoed back; the schema
	// makes it nullable and thread/resume has been observed to omit it
	// (issue #7), so an empty reported_effort means "not reported", not
	// "no effort".
	s.log.Info("review turn starting", "request", requestID, "workflow", key, "round", round, "thread", abbreviate(thread.ID),
		"model", thread.Model, "effort", s.effort, "reported_effort", thread.ReasoningEffort,
		"commit", target.Commit, "base", target.Base, "build", req.Build)

	turnCtx, cancel := context.WithTimeoutCause(ctx, s.timeout, ErrTimeout)
	defer cancel()
	rev, err := reviewer.Review(turnCtx, thread.ID, prompt.Build())
	if err != nil {
		if errors.Is(context.Cause(turnCtx), ErrTimeout) {
			err = fmt.Errorf("%w after %v: %w", ErrTimeout, s.timeout, err)
		}
		s.log.Warn("review turn failed", "request", requestID, "workflow", key, "round", round, "thread", abbreviate(thread.ID), "error", err)
		return nil, err
	}

	if checkout != nil {
		// The reviewer may build and test in the checkout, but its results
		// only describe the commit if tracked files are unchanged. Anything
		// from a lockfile rewrite to an edit despite instructions is
		// reported; untracked build output is expected.
		n, cerr := checkout.ModifiedTrackedFiles(ctx)
		if cerr != nil {
			s.log.Warn("checkout integrity check failed", "request", requestID, "workflow", key, "error", cerr)
		}
		if n > 0 || cerr != nil {
			w := fmt.Sprintf("the reviewer changed %d tracked file(s) in the disposable checkout; its build and test results may not describe the reviewed commit", n)
			if cerr != nil {
				w = "the disposable checkout could not be checked for changes; the reviewer's build and test results may not describe the reviewed commit"
			}
			s.log.Warn(w, "request", requestID, "workflow", key)
			if ws, ok := appserver.AppendWarning(rev.Warnings, w); ok {
				rev.Warnings = ws
			}
		}
	}

	done := state.Workflow{
		ThreadID:        thread.ID,
		LastCommit:      target.Commit,
		LastBase:        target.Base,
		LastRequestHash: hash,
		Round:           round,
		LastReview:      rev.Text,
		LastWarnings:    rev.Warnings,
	}
	if err := s.saveCompleted(ctx, key, requestID, wf, known, previous, done); err != nil {
		return nil, fmt.Errorf("review completed but state was not saved: %w", err)
	}
	s.log.Info("review turn completed", "request", requestID, "workflow", key, "round", round, "thread", abbreviate(thread.ID),
		"turn", rev.TurnID, "warnings", len(rev.Warnings))

	return &Result{
		Repo: repo.Worktree, Branch: branch.Ref, Commit: target.Commit, Base: target.Base,
		Round: round, Review: rev.Text, Warnings: rev.Warnings,
	}, nil
}

// replay reports the completed review to return when the request hash
// matches the workflow's last request: the same commit, base, notes, and
// build flag as the round already recorded.
func replay(wf state.Workflow, known bool, hash string) (state.Workflow, bool) {
	if !known || hash == "" || wf.LastRequestHash != hash {
		return state.Workflow{}, false
	}
	return wf, true
}

// acquireStateLock takes the state lock, which guards the state file for
// the duration of one read or read-modify-write. It is never held while
// acquiring another lock.
func (s *Service) acquireStateLock(ctx context.Context, wait time.Duration) (*state.Lock, error) {
	lock, err := state.AcquireLock(ctx, s.store.LockPath(), wait)
	if err != nil {
		if errors.Is(err, state.ErrLocked) {
			return nil, fmt.Errorf("the state file %s is busy (%w); %s", s.store.Path(), err, stateBusyHint)
		}
		return nil, err
	}
	return lock, nil
}

// loadWorkflow reads one workflow's record under the state lock, which is
// released before returning so that Git validation and the reviewer's turn
// never hold it. The caller holds the workflow lock, which keeps the copy
// current until the review's final save.
func (s *Service) loadWorkflow(ctx context.Context, key string) (wf state.Workflow, known bool, err error) {
	lock, err := s.acquireStateLock(ctx, state.LockWait)
	if err != nil {
		return state.Workflow{}, false, err
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			s.log.Warn("release state lock", "error", rerr)
		}
	}()
	st, err := s.store.Load()
	if err != nil {
		return state.Workflow{}, false, err
	}
	wf, known = st.Get(key)
	return wf, known, nil
}

// saveCompleted records a completed review. It re-reads the state file
// under the state lock so records other reviews wrote meanwhile survive,
// then reconciles this workflow's record: its replay fields must be as
// they were at the start (before, known), since the workflow lock excluded
// every other round of this workflow, and any difference means a writer
// bypassed the lock, so the save is refused rather than overwriting. Its
// history is rebuilt from the fresh record, because another review's
// size-pressure eviction may have cleared it legitimately and must not be
// resurrected. previous is the record of the round being superseded.
func (s *Service) saveCompleted(ctx context.Context, key, requestID string, before state.Workflow, known bool, previous state.HistoryRecord, done state.Workflow) error {
	lock, err := s.acquireStateLock(ctx, state.FinalSaveLockWait)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			s.log.Warn("release state lock", "error", rerr)
		}
	}()
	st, err := s.store.Load()
	if err != nil {
		return err
	}
	fresh, present := st.Get(key)
	if present != known || (known && !fresh.SameReplayFields(before)) {
		return fmt.Errorf("%w: workflow %s in %s changed during the review; the record was not overwritten", ErrStateInvalid, key, s.store.Path())
	}
	if known {
		if bad := fresh.InvalidHistory(); bad != "" {
			return fmt.Errorf("%w: workflow %s in %s changed during the review: %s", ErrStateInvalid, key, s.store.Path(), bad)
		}
		done.History = state.RetainHistory(fresh.History, previous, done.LastReview)
	}
	st.Put(key, done)
	return s.saveEvictingHistory(ctx, st, key, requestID)
}

// saveEvictingHistory saves the state, giving up review history under size
// pressure so that history, this workflow's or any other's, never leaves a
// completed review unsaved. While Save reports ErrTooLarge, it evicts one
// record at a time, the oldest record of whichever workflow's history
// holds the most bytes, until the estimated bytes freed cover the overshoot
// Save reported, then saves again and repeats until the save fits or no
// history remains (issue #24). Taking from the largest history first frees
// the most per record and keeps every workflow's newest verdicts rather
// than clearing whole histories. The estimate is an upper bound on what a
// record's removal frees, so a record is never evicted when the file would
// already fit; an overstated estimate costs at most another pass. Candidates
// are kept in a heap built once, so E evictions over W workflows cost
// O(W + E log W) rather than a rescan per record, and the loop honors ctx
// because it runs under the state lock. Replay fields are never touched,
// so no workflow loses its completed-review record: history is derived
// convenience data, which is why evicting another workflow's copy stays
// within the rule that recovery never deletes another workflow's active
// state. The caller holds the state lock and this workflow's lock.
func (s *Service) saveEvictingHistory(ctx context.Context, st *state.State, key, requestID string) error {
	var plan *evictionPlan
	for {
		err := s.store.Save(st)
		if !errors.Is(err, state.ErrTooLarge) {
			return err
		}
		over := 0
		var tooLarge *state.TooLargeError
		if errors.As(err, &tooLarge) {
			over = tooLarge.Size - tooLarge.Limit
		}
		if plan == nil {
			plan = newEvictionPlan(st, s.recordBytes)
		}
		freed := 0
		for evicted := false; !evicted || freed < over; {
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("evicting review history: %w", cerr)
			}
			k, ok := plan.largest()
			if !ok {
				if evicted {
					break
				}
				return err
			}
			wf, _ := st.Get(k)
			oldest := wf.History[0]
			n := s.recordBytes(oldest)
			freed += n
			wf.History = wf.History[1:]
			if len(wf.History) == 0 {
				wf.History = nil
			}
			st.Put(k, wf)
			plan.shrink(n)
			evicted = true
			s.log.Warn("state file is full; review history evicted", "request", requestID, "workflow", k, "round", oldest.Round, "current_workflow", k == key)
		}
	}
}

// recordFramingBytes bounds the bytes a history record's fixed fields and
// JSON framing occupy in the indented state file: the round, two object
// ids, the field names, indentation at the record's depth, separators,
// and the history field itself when the last record goes. It is generous
// on purpose; see historyRecordBytes.
const recordFramingBytes = 512

// historyRecordBytes is an upper bound on the bytes removed from the
// encoded state file by evicting r: the review as JSON (escaping counted)
// plus recordFramingBytes. Over-estimating costs another Save pass;
// under-estimating would evict a record the file could have kept, so the
// bound is checked by a test against real encodings.
func historyRecordBytes(r state.HistoryRecord) int {
	b, err := json.Marshal(r.Review)
	if err != nil {
		return len(r.Review)*6 + recordFramingBytes // cannot happen for a string; be generous
	}
	return len(b) + recordFramingBytes
}

// evictionPlan orders workflows by the bytes their history quotes, largest
// first, ties by key so eviction is deterministic.
type evictionPlan struct {
	items []evictionCandidate
}

type evictionCandidate struct {
	key   string
	bytes int
}

func newEvictionPlan(st *state.State, recordBytes func(state.HistoryRecord) int) *evictionPlan {
	p := &evictionPlan{}
	for k, wf := range st.Workflows {
		if len(wf.History) == 0 {
			continue
		}
		n := 0
		for _, r := range wf.History {
			n += recordBytes(r)
		}
		p.items = append(p.items, evictionCandidate{key: k, bytes: n})
	}
	heap.Init(p)
	return p
}

// largest returns the key with the most history bytes, or false when no
// history remains.
func (p *evictionPlan) largest() (string, bool) {
	if len(p.items) == 0 {
		return "", false
	}
	return p.items[0].key, true
}

// shrink records that n bytes left the largest candidate's history, and
// drops it from the plan when nothing remains.
func (p *evictionPlan) shrink(n int) {
	p.items[0].bytes -= n
	if p.items[0].bytes <= 0 {
		heap.Pop(p)
		return
	}
	heap.Fix(p, 0)
}

func (p *evictionPlan) Len() int { return len(p.items) }
func (p *evictionPlan) Less(i, j int) bool {
	if p.items[i].bytes != p.items[j].bytes {
		return p.items[i].bytes > p.items[j].bytes
	}
	return p.items[i].key < p.items[j].key
}
func (p *evictionPlan) Swap(i, j int) { p.items[i], p.items[j] = p.items[j], p.items[i] }
func (p *evictionPlan) Push(x any)    { p.items = append(p.items, x.(evictionCandidate)) } //nolint:forcetypeassert // heap.Interface contract
func (p *evictionPlan) Pop() any {
	last := p.items[len(p.items)-1]
	p.items = p.items[:len(p.items)-1]
	return last
}

// incompleteWorkflowField names the first missing invariant of a stored
// completed-review record, or "" when the record is complete. Values are
// never echoed: they are untrusted state-file content.
func incompleteWorkflowField(wf state.Workflow) string {
	switch {
	case wf.ThreadID == "":
		return "thread_id"
	case !state.IsObjectID(wf.LastCommit):
		return "a full object id in last_commit"
	case !state.IsObjectID(wf.LastBase):
		return "a full object id in last_base"
	case wf.LastRequestHash == "":
		return "last_request_hash"
	case wf.LastReview == "":
		return "last_review"
	case wf.Round < 1:
		return "a round of at least 1"
	}
	return ""
}

// setupError attributes a setup failure to whatever ended the setup
// context, when it ended: the setup deadline or the caller's cancellation.
// A call can return its own error, such as a server refusal, in the same
// instant the context ends; the outcome is still the abort.
func setupError(setupCtx context.Context, err error) error {
	cause := context.Cause(setupCtx)
	switch {
	case cause == nil || errors.Is(err, cause):
		return err
	case errors.Is(cause, ErrSetupTimeout):
		return fmt.Errorf("%w: %w", ErrSetupTimeout, err)
	default:
		return fmt.Errorf("%w: %w", cause, err)
	}
}

// threadName is the Codex UI name for a workflow's thread.
func threadName(worktree, branchRef string) string {
	return fmt.Sprintf("Counterpoint review: %s %s", filepath.Base(worktree), strings.TrimPrefix(branchRef, "refs/heads/"))
}

// abbreviate shortens an identifier for logs.
func abbreviate(id string) string {
	if len(id) > 12 {
		return id[:12] + "..."
	}
	return id
}
