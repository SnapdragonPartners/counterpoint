package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for review-target validation.
var (
	ErrCommitNotFound = errors.New("commit not found")
	ErrPrimaryBranch  = errors.New("refusing to review the primary branch")
	ErrNotBranchTip   = errors.New("commit is not the branch tip")
	ErrNotHead        = errors.New("commit is not the worktree HEAD")
	ErrDirtyWorktree  = errors.New("worktree is not clean")
	ErrNoMergeBase    = errors.New("no merge base with the primary branch")
)

// Target is a validated, immutable review target: a clean worktree checked
// out at the tip of a local branch, with the merge base against the primary
// branch resolved.
type Target struct {
	Repo   *Repository
	Branch *Branch
	// Commit is the full object ID under review; it equals Branch.Tip.
	Commit string
	// Base is the merge base between Commit and the primary branch.
	Base string
	// PrimaryName and PrimaryRef identify the primary branch and the ref
	// the merge base was computed against.
	PrimaryName string
	PrimaryRef  string
	// HistoryRewritten is true when a previous tip was supplied and is no
	// longer an ancestor of Commit, or no longer exists.
	HistoryRewritten bool
}

// ResolveCommit resolves any unambiguous commit identifier to a full object
// ID and verifies that it names a commit.
func (r *Repository) ResolveCommit(ctx context.Context, commitish string) (string, error) {
	if commitish == "" || strings.HasPrefix(commitish, "-") {
		return "", fmt.Errorf("%w: %q", ErrCommitNotFound, commitish)
	}
	oid, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", commitish+"^{commit}")
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("%w: %q", ErrCommitNotFound, commitish)
		}
		return "", err
	}
	if !objectIDPattern.MatchString(oid) {
		return "", fmt.Errorf("unexpected object id %q for %q", oid, commitish)
	}
	return oid, nil
}

// ValidateTarget applies the review-target rules from docs/MVP.md in order:
// the branch exists and is not the primary branch, the commit resolves and
// equals both the branch tip and the worktree HEAD, the worktree is clean,
// and a merge base with the primary branch exists. previousTip, when not
// empty, is the last reviewed tip and drives HistoryRewritten.
func (r *Repository) ValidateTarget(ctx context.Context, branchName, commitish, previousTip string) (*Target, error) {
	branch, err := r.ResolveBranch(ctx, branchName)
	if err != nil {
		return nil, err
	}

	primaryName, primaryRef, err := r.PrimaryBranch(ctx)
	if err != nil {
		return nil, err
	}
	if branch.Name == primaryName {
		return nil, fmt.Errorf("%w: %q", ErrPrimaryBranch, branch.Ref)
	}

	commit, err := r.ResolveCommit(ctx, commitish)
	if err != nil {
		return nil, err
	}
	if commit != branch.Tip {
		return nil, fmt.Errorf("%w: %s is at %s, commit is %s", ErrNotBranchTip, branch.Ref, branch.Tip, commit)
	}

	head, err := r.ResolveCommit(ctx, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	if head != commit {
		return nil, fmt.Errorf("%w: HEAD is at %s, commit is %s", ErrNotHead, head, commit)
	}

	if err := r.requireClean(ctx); err != nil {
		return nil, err
	}

	base, err := r.mergeBase(ctx, primaryRef, commit)
	if err != nil {
		return nil, err
	}

	rewritten := false
	if previousTip != "" {
		rewritten, err = r.historyRewritten(ctx, previousTip, commit)
		if err != nil {
			return nil, err
		}
	}

	return &Target{
		Repo:             r,
		Branch:           branch,
		Commit:           commit,
		Base:             base,
		PrimaryName:      primaryName,
		PrimaryRef:       primaryRef,
		HistoryRewritten: rewritten,
	}, nil
}

// requireClean fails when the worktree has staged changes, unstaged changes
// to tracked files, or untracked files. Ignored files do not count.
func (r *Repository) requireClean(ctx context.Context) error {
	return r.requireCleanBounded(ctx, maxStdout)
}

// requireCleanBounded is requireClean with an explicit capture limit. Status
// output only has to be empty or not, so output beyond the limit still
// proves the worktree is dirty and is reported as such rather than failing.
func (r *Repository) requireCleanBounded(ctx context.Context, limit int) error {
	out, truncated, err := runBounded(ctx, r.Worktree, limit, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if out == "" && !truncated {
		return nil
	}
	lines := strings.Split(out, "\n")
	summary := truncate(strings.Join(lines, "; "))
	if truncated {
		// The count is unknown: the cut may fall inside the first entry.
		return fmt.Errorf("%w: status output exceeded %d bytes (truncated); begins: %s", ErrDirtyWorktree, limit, summary)
	}
	return fmt.Errorf("%w: %d entries: %s", ErrDirtyWorktree, len(lines), summary)
}

// mergeBase returns the merge base of ref and commit.
func (r *Repository) mergeBase(ctx context.Context, ref, commit string) (string, error) {
	out, err := r.git(ctx, "merge-base", "--", ref, commit)
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("%w: %s and %s share no history", ErrNoMergeBase, ref, commit)
		}
		return "", err
	}
	if !objectIDPattern.MatchString(out) {
		return "", fmt.Errorf("unexpected merge-base output %q", out)
	}
	return out, nil
}

// historyRewritten reports whether previousTip is no longer an ancestor of
// commit. A previous tip that no longer exists in the repository also counts
// as rewritten, since the reviewed history cannot be reached from it.
func (r *Repository) historyRewritten(ctx context.Context, previousTip, commit string) (bool, error) {
	if _, err := r.ResolveCommit(ctx, previousTip); err != nil {
		if errors.Is(err, ErrCommitNotFound) {
			return true, nil
		}
		return false, err
	}
	_, err := r.git(ctx, "merge-base", "--is-ancestor", "--", previousTip, commit)
	switch {
	case err == nil:
		return false, nil
	case isNotFound(err):
		return true, nil
	default:
		return false, err
	}
}
