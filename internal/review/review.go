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
	"os"
	"time"

	"github.com/SnapdragonPartners/counterpoint/internal/appserver"
	"github.com/SnapdragonPartners/counterpoint/internal/gitrepo"
	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

const (
	// Timeout bounds one Codex review turn. It sits below the MCP client's
	// idle timeout so Counterpoint fails first with a clear error.
	Timeout = 20 * time.Minute

	// ReasoningEffort is the fixed reviewer effort for the MVP, passed to
	// the app-server as a configuration override.
	ReasoningEffort = "xhigh"
)

// Sentinel errors.
var (
	ErrInvalidRequest    = errors.New("invalid review request")
	ErrThreadUnavailable = errors.New("stored Codex thread could not be resumed")
	ErrTimeout           = errors.New("review timed out")
)

// Reviewer is the app-server session the service drives. The real
// implementation is *appserver.Client; tests substitute a fake so the
// orchestration is covered without a subprocess.
type Reviewer interface {
	StartThread(ctx context.Context, cwd string) (appserver.Thread, error)
	ResumeThread(ctx context.Context, threadID, cwd string) (appserver.Thread, error)
	Review(ctx context.Context, threadID, instructions string) (*appserver.Review, error)
	Close()
}

// Request is the review tool's validated-at-the-boundary input.
type Request struct {
	Repo        string
	Branch      string
	Commit      string
	BranchNotes string
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
	// NewReviewer starts an app-server session for one review. When nil,
	// DefaultReviewer with the codex executable is used.
	NewReviewer func(ctx context.Context) (Reviewer, error)
	Logger      *slog.Logger
	Version     string
}

// Service runs reviews.
type Service struct {
	store       *state.Store
	newReviewer func(ctx context.Context) (Reviewer, error)
	log         *slog.Logger
	timeout     time.Duration
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
	return &Service{store: opts.Store, newReviewer: nr, log: log, timeout: Timeout}
}

// DefaultReviewer starts codex app-server with the fixed reasoning effort.
// The child is spawned on a context detached from the tool call so that a
// cancelled call still gets an orderly interrupt and Close rather than an
// immediate kill.
func DefaultReviewer(version string, log *slog.Logger) func(ctx context.Context) (Reviewer, error) {
	return func(ctx context.Context) (Reviewer, error) {
		args := append(appserver.DefaultArgs(), "-c", fmt.Sprintf("model_reasoning_effort=%q", ReasoningEffort))
		return appserver.Start(context.WithoutCancel(ctx), appserver.Options{
			Command: appserver.DefaultCommand,
			Args:    args,
			Version: version,
			Stderr:  os.Stderr,
			Logger:  log,
		})
	}
}

// Review performs one review round, or replays an identical completed one.
func (s *Service) Review(ctx context.Context, req Request) (*Result, error) {
	if req.Repo == "" || req.Branch == "" || req.Commit == "" || req.BranchNotes == "" {
		return nil, fmt.Errorf("%w: repo, branch, commit, and branch_notes are all required", ErrInvalidRequest)
	}
	start := time.Now()

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
	}
	hash := ident.Hash()
	if prev, ok := st.Replay(key, hash); ok {
		s.log.Info("review replayed from state", "workflow", key, "round", prev.Round, "commit", target.Commit)
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

	reviewer, err := s.newReviewer(ctx)
	if err != nil {
		return nil, err
	}
	defer reviewer.Close()

	var thread appserver.Thread
	if known && wf.ThreadID != "" {
		thread, err = reviewer.ResumeThread(ctx, wf.ThreadID, repo.Worktree)
		if err != nil {
			return nil, fmt.Errorf("%w: workflow %s in %s: %w", ErrThreadUnavailable, key, s.store.Path(), err)
		}
	} else {
		thread, err = reviewer.StartThread(ctx, repo.Worktree)
		if err != nil {
			return nil, err
		}
	}
	s.log.Info("review turn starting", "workflow", key, "round", round, "thread", abbreviate(thread.ID),
		"model", thread.Model, "effort", thread.ReasoningEffort, "commit", target.Commit, "base", target.Base)

	turnCtx, cancel := context.WithTimeoutCause(ctx, s.timeout, ErrTimeout)
	defer cancel()
	rev, err := reviewer.Review(turnCtx, thread.ID, prompt.Build())
	if err != nil {
		if errors.Is(context.Cause(turnCtx), ErrTimeout) {
			err = fmt.Errorf("%w after %v: %w", ErrTimeout, s.timeout, err)
		}
		s.log.Warn("review turn failed", "workflow", key, "round", round, "duration", time.Since(start), "error", err)
		return nil, err
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
	s.log.Info("review turn completed", "workflow", key, "round", round, "thread", abbreviate(thread.ID),
		"turn", rev.TurnID, "duration", time.Since(start), "warnings", len(rev.Warnings))

	return &Result{
		Repo: repo.Worktree, Branch: branch.Ref, Commit: target.Commit, Base: target.Base,
		Round: round, Review: rev.Text, Warnings: rev.Warnings,
	}, nil
}

// abbreviate shortens an identifier for logs.
func abbreviate(id string) string {
	if len(id) > 12 {
		return id[:12] + "..."
	}
	return id
}
