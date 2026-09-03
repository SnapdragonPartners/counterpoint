package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Sentinel errors for repository validation. Wrapped errors carry detail;
// callers match with errors.Is.
var (
	ErrNotAbsolute   = errors.New("repository path is not absolute")
	ErrNotRepository = errors.New("path is not inside a git worktree")
)

// Repository is a validated Git worktree.
type Repository struct {
	// Worktree is the canonical worktree root with symlinks resolved. Codex
	// inspects this path.
	Worktree string
	// CommonDir is the absolute, symlink-resolved common Git directory. All
	// worktrees of one clone share it, so it is the repository identity.
	CommonDir string
}

// Open validates path and returns the worktree it belongs to. The path must
// be absolute and inside a Git worktree; it may point at a subdirectory.
func Open(ctx context.Context, path string) (*Repository, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: %q", ErrNotAbsolute, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", path, err)
	}

	out, err := run(ctx, resolved, "rev-parse", "--show-toplevel", "--git-common-dir")
	if err != nil {
		if exitCode(err) >= 0 {
			return nil, fmt.Errorf("%w: %q: %w", ErrNotRepository, path, err)
		}
		return nil, err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return nil, fmt.Errorf("%w: %q: unexpected rev-parse output %q", ErrNotRepository, path, out)
	}

	worktree, err := filepath.EvalSymlinks(lines[0])
	if err != nil {
		return nil, fmt.Errorf("resolve worktree %q: %w", lines[0], err)
	}

	// Git reports the common directory relative to the directory it ran in
	// when the two are close, so anchor it there before canonicalizing.
	commonDir := lines[1]
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(resolved, commonDir)
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil {
		return nil, fmt.Errorf("resolve common dir %q: %w", lines[1], err)
	}

	return &Repository{Worktree: worktree, CommonDir: commonDir}, nil
}

// Identity is the canonical repository identity shared by all worktrees of
// the same clone.
func (r *Repository) Identity() string {
	return r.CommonDir
}

// WorkflowKey builds the state key for a repository identity and a full
// local branch ref, in the form "<identity>::<ref>".
func WorkflowKey(identity, ref string) string {
	return identity + "::" + ref
}

// git runs a Git command inside the worktree.
func (r *Repository) git(ctx context.Context, args ...string) (string, error) {
	return run(ctx, r.Worktree, args...)
}
