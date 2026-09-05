// Package scratch owns the disposable checkout a build-capable review runs
// in: a Counterpoint-owned directory per workflow holding a shared clone of
// the reviewed commit, a persistent build cache, and a lock on the
// directory itself. See docs/design/disposable-checkout.md.
package scratch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SnapdragonPartners/counterpoint/internal/gitrepo"
	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

const (
	// EnvRoot overrides the scratch root for tests and unusual
	// installations, like COUNTERPOINT_STATE_FILE for the state file.
	EnvRoot = "COUNTERPOINT_CHECKOUT_DIR"

	// cacheSubdir is the path under os.UserCacheDir for the default root.
	cacheSubdir = "counterpoint/checkouts"

	// Names inside one workflow directory.
	checkoutName = "checkout"
	cacheName    = "cache"
	hooksName    = "hooks"
	lockName     = "lock"
	// tmpName is the reviewer's temp directory. It sits beside the
	// checkout, not inside it: temp directories under a Git worktree are
	// discovered as part of that repository, which breaks tests that expect
	// a directory outside any repository. It is recreated every round and
	// is the second writable root.
	tmpName = "tmp"

	dirPerm = 0o700
	// keyHashLen is how many hex characters of the workflow key's hash name
	// the workflow directory.
	keyHashLen = 16
)

// Sentinel errors.
var (
	ErrRootOverlapsRepository = errors.New("scratch root overlaps the reviewed repository")
	ErrUnexpectedSymlink      = errors.New("scratch path component is a symlink")
)

// DefaultRoot returns the scratch root: EnvRoot when set, otherwise the
// Counterpoint subdirectory of the user cache directory.
func DefaultRoot() (string, error) {
	if p := os.Getenv(EnvRoot); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", EnvRoot, p)
		}
		return p, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache dir: %w", err)
	}
	return filepath.Join(dir, cacheSubdir), nil
}

// Options describes the checkout to prepare.
type Options struct {
	// Root is the scratch root; DefaultRoot when empty.
	Root string
	// WorkflowKey names the workflow; it selects the directory.
	WorkflowKey string
	// Repo is the validated repository the commit comes from.
	Repo *gitrepo.Repository
	// Commit is the full object id to check out.
	Commit string
	// LockWait bounds waiting for the workflow directory's lock;
	// state.LockWait when zero.
	LockWait time.Duration
}

// Checkout is a prepared disposable checkout. Close removes it.
type Checkout struct {
	// Dir is the detached checkout of the commit: the reviewer's cwd.
	Dir string
	// CacheDir persists across rounds for build caches. With TmpDir it is
	// one of the two writable locations outside Dir.
	CacheDir string
	// TmpDir is the reviewer's TMPDIR, beside Dir and recreated each
	// round.
	TmpDir string

	workflowDir string
	hooksDir    string
	lock        *state.Lock
}

// Prepare creates the workflow directory under the root, locks it, replaces
// any previous checkout with a fresh detached clone of the commit, and
// creates the temp directory. On any failure it releases everything it
// acquired.
func Prepare(ctx context.Context, opts Options) (co *Checkout, err error) {
	root := opts.Root
	if root == "" {
		if root, err = DefaultRoot(); err != nil {
			return nil, err
		}
	}
	root, err = canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	// Overlap is checked before anything is created, so a misconfigured
	// root inside the repository never leaves even an empty directory
	// there.
	if err := rejectOverlap(root, opts.Repo.Worktree); err != nil {
		return nil, err
	}
	if err := rejectOverlap(root, opts.Repo.CommonDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return nil, fmt.Errorf("create scratch root: %w", err)
	}

	sum := sha256.Sum256([]byte(opts.WorkflowKey))
	workflowDir := filepath.Join(root, hex.EncodeToString(sum[:])[:keyHashLen])
	if err := os.MkdirAll(workflowDir, dirPerm); err != nil {
		return nil, fmt.Errorf("create workflow scratch dir: %w", err)
	}
	if err := requireNotSymlink(workflowDir); err != nil {
		return nil, err
	}

	wait := opts.LockWait
	if wait == 0 {
		wait = state.LockWait
	}
	lock, err := state.AcquireLock(ctx, filepath.Join(workflowDir, lockName), wait)
	if err != nil {
		return nil, fmt.Errorf("scratch: %w", err)
	}
	co = &Checkout{
		Dir:         filepath.Join(workflowDir, checkoutName),
		CacheDir:    filepath.Join(workflowDir, cacheName),
		TmpDir:      filepath.Join(workflowDir, tmpName),
		workflowDir: workflowDir,
		hooksDir:    filepath.Join(workflowDir, hooksName),
		lock:        lock,
	}
	defer func() {
		if err != nil {
			_ = co.Close()
		}
	}()

	// Leftovers are a crashed earlier round; replace them.
	for _, dir := range []string{co.Dir, co.hooksDir, co.TmpDir} {
		if err := co.removeOwned(dir); err != nil {
			return nil, err
		}
	}
	if err := os.Mkdir(co.hooksDir, dirPerm); err != nil {
		return nil, fmt.Errorf("create hooks dir: %w", err)
	}
	if err := os.Mkdir(co.TmpDir, dirPerm); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	// The cache persists, so it may have been replaced meanwhile. It is
	// handed to Codex as a writable root, and a symlink there would
	// redirect the reviewer's writes outside this tree.
	if err := os.MkdirAll(co.CacheDir, dirPerm); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	if err := requireNotSymlink(co.CacheDir); err != nil {
		return nil, err
	}
	if err := opts.Repo.CloneDetached(ctx, co.Dir, opts.Commit, co.hooksDir); err != nil {
		return nil, err
	}
	return co, nil
}

// ModifiedTrackedFiles counts tracked files the reviewer changed in the
// checkout.
func (c *Checkout) ModifiedTrackedFiles(ctx context.Context) (int, error) {
	return gitrepo.ModifiedTrackedFiles(ctx, c.Dir)
}

// Close removes the checkout, temp, and hooks directories, keeps the
// cache, and releases the lock. It is safe to call more than once.
func (c *Checkout) Close() error {
	if c == nil {
		return nil
	}
	var errs []error
	for _, dir := range []string{c.Dir, c.TmpDir, c.hooksDir} {
		if err := c.removeOwned(dir); err != nil {
			errs = append(errs, err)
		}
	}
	if c.lock != nil {
		if err := c.lock.Release(); err != nil {
			errs = append(errs, fmt.Errorf("release scratch lock: %w", err))
		}
		c.lock = nil
	}
	return errors.Join(errs...)
}

// removeOwned deletes path, which must be a direct child of the workflow
// directory and not a symlink, so removal can never follow a link out of
// Counterpoint's own tree.
func (c *Checkout) removeOwned(path string) error {
	if filepath.Dir(path) != c.workflowDir {
		return fmt.Errorf("refusing to remove %s: not inside %s", path, c.workflowDir)
	}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("inspect %s: %w", path, err)
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s", ErrUnexpectedSymlink, path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// canonicalRoot returns the root with symlinks resolved without creating
// it: the nearest existing ancestor is resolved and the missing components
// are appended, so the overlap check sees where the root would really be
// before any directory is made. Every later comparison and removal then
// uses one spelling.
func canonicalRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("scratch root must be absolute, got %q", root)
	}
	existing, missing := filepath.Clean(root), ""
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect %s: %w", existing, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing ancestor for scratch root %s", root)
		}
		missing = filepath.Join(filepath.Base(existing), missing)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolve scratch root: %w", err)
	}
	return filepath.Join(resolved, missing), nil
}

// rejectOverlap fails when root is inside dir, dir is inside root, or they
// are the same, with both already canonical.
func rejectOverlap(root, dir string) error {
	if root == dir || isWithin(root, dir) || isWithin(dir, root) {
		return fmt.Errorf("%w: %s and %s", ErrRootOverlapsRepository, root, dir)
	}
	return nil
}

func isWithin(path, dir string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

func requireNotSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrUnexpectedSymlink, path)
	}
	return nil
}
