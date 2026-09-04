package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo is a throwaway repository built with real Git in a temp dir.
type testRepo struct {
	t   *testing.T
	dir string
}

// newTestRepo initializes a repository whose initial branch is named
// initialBranch and creates one commit on it.
func newTestRepo(t *testing.T, initialBranch string) *testRepo {
	t.Helper()
	dir := t.TempDir()
	// Canonicalize so comparisons against Open's symlink-resolved paths hold
	// on platforms where the temp dir is itself a symlink.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	r := &testRepo{t: t, dir: resolved}
	r.git("init", "--quiet", "--initial-branch="+initialBranch)
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "Test")
	r.git("config", "commit.gpgsign", "false")
	r.commit("initial")
	return r
}

// git runs a Git command in the repository and fails the test on error.
func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	out, err := r.tryGit(args...)
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// tryGit runs Git with the package's cleaned environment. That matters when
// the tests run from a Git hook: git exports GIT_INDEX_FILE and similar
// variables to hooks, and inheriting them breaks commands in the test repo.
func (r *testRepo) tryGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = cleanEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// write creates or overwrites a file relative to the worktree.
func (r *testRepo) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatalf("write %s: %v", name, err)
	}
}

// commit writes a file named after the message, stages everything, and
// commits. It returns the new commit's full object ID.
func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	r.write(msg+".txt", msg+"\n")
	r.git("add", "-A")
	r.git("commit", "--quiet", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// checkoutNew creates and checks out a branch from the current HEAD.
func (r *testRepo) checkoutNew(name string) {
	r.t.Helper()
	r.git("checkout", "--quiet", "-b", name)
}

func (r *testRepo) checkout(name string) {
	r.t.Helper()
	r.git("checkout", "--quiet", name)
}

func (r *testRepo) head() string {
	r.t.Helper()
	return r.git("rev-parse", "HEAD")
}

// open calls Open on the repository and fails the test on error.
func (r *testRepo) open() *Repository {
	r.t.Helper()
	repo, err := Open(context.Background(), r.dir)
	if err != nil {
		r.t.Fatalf("Open(%q): %v", r.dir, err)
	}
	return repo
}

// addOriginClone creates a bare clone as "origin" of r and sets the remote
// HEAD, so r looks like a repository cloned from a remote whose HEAD names
// primaryBranch.
func (r *testRepo) addOriginClone(primaryBranch string) {
	r.t.Helper()
	bare := filepath.Join(r.t.TempDir(), "origin.git")
	r.git("clone", "--quiet", "--bare", r.dir, bare)
	r.git("remote", "add", "origin", bare)
	r.git("fetch", "--quiet", "origin")
	r.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+primaryBranch)
}
