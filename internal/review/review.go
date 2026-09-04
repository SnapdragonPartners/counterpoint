// Package review orchestrates one review: it serializes on the review lock,
// validates the target, replays an identical request from state, otherwise
// runs a Codex review on the workflow's persistent thread and persists the
// completed result. It owns the round number and the prompt.
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

	// ReasoningEffort is the fixed reviewer effort for the MVP, passed to
	// the app-server as a configuration override.
	ReasoningEffort = "xhigh"

	// SetupTimeout bounds everything before the review turn: launching the
	// app-server, its initialize handshake, and thread start or resume.
	// The review timer starts only after setup, so setup needs its own.
	SetupTimeout = 60 * time.Second

	// MaxBranchNotesBytes bounds the author's notes, which are untrusted
	// input carried into the prompt and the persisted state.
	MaxBranchNotesBytes = 1 << 20
)

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
	// the codex executable is used.
	NewReviewer func(ctx context.Context, extraArgs []string) (Reviewer, error)
	Logger      *slog.Logger
	Version     string
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
}

// New returns a Service.
func New(opts Options) *Service {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	nr := opts.NewReviewer
	if nr == nil {
		nr = DefaultReviewer(opts.Version, log)
	}
	return &Service{store: opts.Store, newReviewer: nr, log: log, timeout: Timeout, setupTimeout: SetupTimeout, checkoutRoot: opts.CheckoutRoot}
}

// DefaultReviewer starts codex app-server with the fixed reasoning effort
// and the session's extra configuration overrides. The handshake is bounded
// by the setup context the service passes; the process lifetime is owned by
// the client's Close.
func DefaultReviewer(version string, log *slog.Logger) func(ctx context.Context, extraArgs []string) (Reviewer, error) {
	return func(ctx context.Context, extraArgs []string) (Reviewer, error) {
		args := append(appserver.DefaultArgs(), "-c", fmt.Sprintf("model_reasoning_effort=%q", ReasoningEffort))
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

	lock, err := state.AcquireLock(ctx, s.store.LockPath(), state.LockWait)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			s.log.Warn("release review lock", "error", rerr)
		}
	}()

	repo, err := gitrepo.Open(ctx, req.Repo)
	if err != nil {
		return nil, err
	}
	branch, err := repo.ResolveBranch(ctx, req.Branch)
	if err != nil {
		return nil, err
	}
	key := gitrepo.WorkflowKey(repo.Identity(), branch.Ref)

	st, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	wf, known := st.Get(key)
	if known {
		// A stored workflow must be a complete record of a finished review;
		// anything else could only come from corruption or a foreign
		// writer. Starting a replacement thread would silently discard
		// review context, and a partial record could replay an empty
		// review or skip the idempotency check.
		if missing := incompleteWorkflowField(wf); missing != "" {
			return nil, fmt.Errorf("%w: workflow %s in %s is missing %s", ErrStateInvalid, key, s.store.Path(), missing)
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
	if prev, ok := st.Replay(key, hash); ok {
		s.log.Info("review replayed from state", "request", requestID, "workflow", key, "round", prev.Round, "commit", target.Commit)
		return &Result{
			Repo: repo.Worktree, Branch: branch.Ref, Commit: target.Commit, Base: target.Base,
			Round: prev.Round, Review: prev.LastReview, Warnings: prev.LastWarnings, Replayed: true,
		}, nil
	}

	round := wf.Round + 1
	prompt := Prompt{
		Round: round, Worktree: repo.Worktree, BranchRef: branch.Ref, Commit: target.Commit,
		Base: target.Base, PrimaryName: target.PrimaryName, PrimaryRef: target.PrimaryRef,
		PreviousTip: wf.LastCommit, HistoryRewritten: target.HistoryRewritten, BranchNotes: req.BranchNotes,
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
			Root: s.checkoutRoot, WorkflowKey: key, Repo: repo, Commit: target.Commit,
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
		sandbox = appserver.Sandbox{Build: true, WritableRoots: []string{checkout.CacheDir}}
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
		"model", thread.Model, "effort", ReasoningEffort, "reported_effort", thread.ReasoningEffort,
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

	st.Put(key, state.Workflow{
		ThreadID:        thread.ID,
		LastCommit:      target.Commit,
		LastBase:        target.Base,
		LastRequestHash: hash,
		Round:           round,
		LastReview:      rev.Text,
		LastWarnings:    rev.Warnings,
	})
	if err := s.store.Save(st); err != nil {
		return nil, fmt.Errorf("review completed but state was not saved: %w", err)
	}
	s.log.Info("review turn completed", "request", requestID, "workflow", key, "round", round, "thread", abbreviate(thread.ID),
		"turn", rev.TurnID, "warnings", len(rev.Warnings))

	return &Result{
		Repo: repo.Worktree, Branch: branch.Ref, Commit: target.Commit, Base: target.Base,
		Round: round, Review: rev.Text, Warnings: rev.Warnings,
	}, nil
}

// incompleteWorkflowField names the first missing invariant of a stored
// completed-review record, or "" when the record is complete. Values are
// never echoed: they are untrusted state-file content.
func incompleteWorkflowField(wf state.Workflow) string {
	switch {
	case wf.ThreadID == "":
		return "thread_id"
	case wf.LastCommit == "":
		return "last_commit"
	case wf.LastBase == "":
		return "last_base"
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
