package scratch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/counterpoint/internal/gitrepo"
	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo builds a repository with one commit on a feature branch and
// returns it opened, with the commit id.
func newRepo(t *testing.T, extraFiles map[string]string) (*gitrepo.Repository, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	git(t, dir, "config", "user.email", "t@example.com")
	git(t, dir, "config", "user.name", "T")
	git(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range extraFiles {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "initial")
	repo, err := gitrepo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, git(t, dir, "rev-parse", "HEAD")
}

func newRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "checkouts")
}

func TestPrepareCreatesCheckoutAndCloseRemovesIt(t *testing.T) {
	repo, commit := newRepo(t, nil)
	root := newRoot(t)
	opts := Options{Root: root, WorkflowKey: repo.Identity() + "::refs/heads/main", Repo: repo, Commit: commit}
	co, err := Prepare(context.Background(), opts)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if head := git(t, co.Dir, "rev-parse", "HEAD"); head != commit {
		t.Errorf("checkout HEAD = %s, want %s", head, commit)
	}
	if b, err := os.ReadFile(filepath.Join(co.Dir, "file.txt")); err != nil || string(b) != "hello\n" {
		t.Errorf("file.txt in checkout = %q, %v", b, err)
	}
	info, err := os.Stat(co.TmpDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("temp dir = %v, %v; want a 0700 directory", info, err)
	}
	if f, err := os.CreateTemp(co.TmpDir, "probe"); err != nil {
		t.Errorf("cannot create a temp file in %s: %v", co.TmpDir, err)
	} else {
		_ = f.Close()
	}
	if _, err := os.Stat(co.CacheDir); err != nil {
		t.Errorf("cache dir missing: %v", err)
	}
	if !strings.HasPrefix(co.Dir, root+string(filepath.Separator)) {
		t.Errorf("checkout %s is not under the root %s", co.Dir, root)
	}
	if status := git(t, repo.Worktree, "status", "--porcelain"); status != "" {
		t.Errorf("source repository modified:\n%s", status)
	}

	if err := co.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(co.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout still present after Close: %v", err)
	}
	if _, err := os.Stat(co.CacheDir); err != nil {
		t.Errorf("cache removed by Close: %v", err)
	}
	if err := co.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// The lock was released: a new Prepare succeeds at once.
	again, err := Prepare(context.Background(), opts)
	if err != nil {
		t.Fatalf("Prepare after Close: %v", err)
	}
	_ = again.Close()
}

func TestPrepareReplacesACrashRemnant(t *testing.T) {
	repo, commit := newRepo(t, nil)
	opts := Options{Root: newRoot(t), WorkflowKey: "k", Repo: repo, Commit: commit}
	co, err := Prepare(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = co.Close()
	// A crashed round left a checkout behind.
	if err := os.MkdirAll(co.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(co.Dir, "stray.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	co2, err := Prepare(context.Background(), opts)
	if err != nil {
		t.Fatalf("Prepare over a remnant: %v", err)
	}
	defer co2.Close()
	if _, err := os.Stat(stray); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("remnant file survived: %v", err)
	}
	if head := git(t, co2.Dir, "rev-parse", "HEAD"); head != commit {
		t.Errorf("checkout HEAD = %s, want %s", head, commit)
	}
}

func TestPrepareRejectsARootOverlappingTheRepository(t *testing.T) {
	repo, commit := newRepo(t, nil)
	cases := map[string]string{
		"inside the worktree":                filepath.Join(repo.Worktree, "scratch"),
		"nested and missing in the worktree": filepath.Join(repo.Worktree, "missing", "deeper", "scratch"),
		"inside the common dir":              filepath.Join(repo.CommonDir, "scratch"),
		"missing in the common dir":          filepath.Join(repo.CommonDir, "missing", "scratch"),
		"the worktree itself":                repo.Worktree,
		"a parent of the worktree":           filepath.Dir(repo.Worktree),
	}
	for name, root := range cases {
		_, err := Prepare(context.Background(), Options{Root: root, WorkflowKey: "k", Repo: repo, Commit: commit})
		if !errors.Is(err, ErrRootOverlapsRepository) {
			t.Errorf("%s: error = %v, want ErrRootOverlapsRepository", name, err)
		}
	}
	// Git status does not show empty directories, so check the missing
	// roots directly: a rejected root must not be created at all.
	for _, path := range []string{filepath.Join(repo.Worktree, "missing"), filepath.Join(repo.CommonDir, "missing")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("rejected root was created at %s: %v", path, err)
		}
	}
	if status := git(t, repo.Worktree, "status", "--porcelain"); status != "" {
		t.Errorf("a rejected root still wrote into the repository:\n%s", status)
	}
}

func TestPrepareResolvesAMissingRootThroughItsExistingAncestor(t *testing.T) {
	repo, commit := newRepo(t, nil)
	base := t.TempDir() // unresolved: a symlink on macOS
	root := filepath.Join(base, "a", "b", "checkouts")
	co, err := Prepare(context.Background(), Options{Root: root, WorkflowKey: "k", Repo: repo, Commit: commit})
	if err != nil {
		t.Fatalf("Prepare with a missing nested root: %v", err)
	}
	defer co.Close()
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedBase, "a", "b", "checkouts")
	if !strings.HasPrefix(co.Dir, want+string(filepath.Separator)) {
		t.Errorf("checkout %s is not under the canonical root %s", co.Dir, want)
	}
	if info, err := os.Stat(want); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("root %s = %v, %v; want a 0700 directory", want, info, err)
	}
}

func TestPrepareRefusesASymlinkedWorkflowDir(t *testing.T) {
	repo, commit := newRepo(t, nil)
	root := newRoot(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("k"))
	link := filepath.Join(root, hex.EncodeToString(sum[:])[:keyHashLen])
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	_, err := Prepare(context.Background(), Options{Root: root, WorkflowKey: "k", Repo: repo, Commit: commit})
	if !errors.Is(err, ErrUnexpectedSymlink) {
		t.Fatalf("error = %v, want ErrUnexpectedSymlink", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("Prepare wrote through the symlink: %v", entries)
	}
}

func TestPrepareFailsWhileAnotherHoldsTheWorkflowLock(t *testing.T) {
	repo, commit := newRepo(t, nil)
	opts := Options{Root: newRoot(t), WorkflowKey: "k", Repo: repo, Commit: commit, LockWait: 50 * time.Millisecond}
	held, err := Prepare(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, err = Prepare(context.Background(), opts)
	if !errors.Is(err, state.ErrLocked) {
		t.Fatalf("second Prepare error = %v, want ErrLocked", err)
	}
	if _, err := os.Stat(held.Dir); err != nil {
		t.Errorf("the contender disturbed the live checkout: %v", err)
	}
}

func TestPrepareRefusesACommitThatTracksTheTempName(t *testing.T) {
	repo, commit := newRepo(t, map[string]string{TmpName + "/keep": "x"})
	_, err := Prepare(context.Background(), Options{Root: newRoot(t), WorkflowKey: "k", Repo: repo, Commit: commit})
	if !errors.Is(err, ErrTmpPathTracked) {
		t.Fatalf("error = %v, want ErrTmpPathTracked", err)
	}
}

func TestDefaultRootHonorsTheOverride(t *testing.T) {
	t.Setenv(EnvRoot, "relative/path")
	if _, err := DefaultRoot(); err == nil {
		t.Error("relative override accepted")
	}
	t.Setenv(EnvRoot, "/abs/path")
	if root, err := DefaultRoot(); err != nil || root != "/abs/path" {
		t.Errorf("DefaultRoot() = %q, %v", root, err)
	}
	t.Setenv(EnvRoot, "")
	root, err := DefaultRoot()
	if err != nil || !filepath.IsAbs(root) || !strings.HasSuffix(root, filepath.FromSlash(cacheSubdir)) {
		t.Errorf("DefaultRoot() = %q, %v", root, err)
	}
}
