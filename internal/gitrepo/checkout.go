package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// isolatedGitEnv keeps the user's global and system Git configuration out of
// commands that populate a disposable checkout. Those commands run before
// any Codex sandbox exists, and a global hooks path, a post-checkout hook,
// or a filter definition such as Git LFS named by a tracked .gitattributes
// would otherwise execute arbitrary code on Counterpoint's behalf. With no
// filter definitions, attribute-declared filters are inert.
var isolatedGitEnv = [...]string{ //nolint:gochecknoglobals // constant table
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_CONFIG_SYSTEM=/dev/null",
	"GIT_CONFIG_NOSYSTEM=1",
}

// ErrInvalidCommit reports a commit that is not a full object id. The
// checkout takes only resolved ids so nothing ambiguous reaches Git.
var ErrInvalidCommit = errors.New("commit is not a full object id")

// isFullObjectID accepts a full SHA-1 or SHA-256 object id in lower-case hex.
func isFullObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CloneDetached creates a shared, no-checkout clone of the repository at
// dest and checks out commit detached. hooksDir must be an existing empty
// directory: Git is pointed at it so no hook can run. The clone reads
// objects through an alternates link to the source object store and writes
// nothing into the source repository.
func (r *Repository) CloneDetached(ctx context.Context, dest, commit, hooksDir string) error {
	if !isFullObjectID(commit) {
		return fmt.Errorf("clone: %w: %q", ErrInvalidCommit, commit)
	}
	env := isolatedGitEnv[:]
	hooks := "core.hooksPath=" + hooksDir
	if _, _, err := runBoundedEnv(ctx, r.Worktree, maxStdout, env,
		"-c", hooks, "clone", "--quiet", "--shared", "--no-checkout", "--template=", "--", r.CommonDir, dest); err != nil {
		return fmt.Errorf("clone into %s: %w", dest, err)
	}
	if _, _, err := runBoundedEnv(ctx, dest, maxStdout, env,
		"-c", hooks, "checkout", "--quiet", "--detach", commit, "--"); err != nil {
		return fmt.Errorf("checkout %s in %s: %w", commit, dest, err)
	}
	return nil
}

// ModifiedTrackedFiles counts tracked files in the worktree at dir whose
// content or index entry differs from HEAD. Untracked files do not count.
// It is used after a build-capable review to report whether the reviewer's
// results still describe the reviewed commit.
func ModifiedTrackedFiles(ctx context.Context, dir string) (int, error) {
	out, truncated, err := runBoundedEnv(ctx, dir, maxStdout, isolatedGitEnv[:], "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return 0, err
	}
	if out == "" && !truncated {
		return 0, nil
	}
	n := len(strings.Split(out, "\n"))
	if truncated {
		return n, fmt.Errorf("status in %s: %w", dir, ErrOutputTooLarge)
	}
	return n, nil
}
