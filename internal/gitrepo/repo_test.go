package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOpenRejectsRelativePath(t *testing.T) {
	_, err := Open(context.Background(), "relative/path")
	if !errors.Is(err, ErrNotAbsolute) {
		t.Fatalf("Open(relative) error = %v, want ErrNotAbsolute", err)
	}
}

func TestOpenRejectsNonRepository(t *testing.T) {
	dir := t.TempDir()
	_, err := Open(context.Background(), dir)
	if !errors.Is(err, ErrNotRepository) {
		t.Fatalf("Open(non-repo) error = %v, want ErrNotRepository", err)
	}
}

func TestOpenRejectsMissingPath(t *testing.T) {
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("Open(missing) error = nil, want error")
	}
}

func TestOpenCanonicalizesWorktreeAndCommonDir(t *testing.T) {
	r := newTestRepo(t, "main")
	repo := r.open()

	if repo.Worktree != r.dir {
		t.Errorf("Worktree = %q, want %q", repo.Worktree, r.dir)
	}
	want := filepath.Join(r.dir, ".git")
	if repo.CommonDir != want {
		t.Errorf("CommonDir = %q, want %q", repo.CommonDir, want)
	}
	if !filepath.IsAbs(repo.CommonDir) {
		t.Errorf("CommonDir %q is not absolute", repo.CommonDir)
	}
}

func TestOpenAcceptsSubdirectoryAndSymlink(t *testing.T) {
	r := newTestRepo(t, "main")
	r.write("sub/dir/file.txt", "x\n")

	sub, err := Open(context.Background(), filepath.Join(r.dir, "sub", "dir"))
	if err != nil {
		t.Fatalf("Open(subdir): %v", err)
	}
	if sub.Worktree != r.dir {
		t.Errorf("subdir Worktree = %q, want %q", sub.Worktree, r.dir)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(r.dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	viaLink, err := Open(context.Background(), link)
	if err != nil {
		t.Fatalf("Open(symlink): %v", err)
	}
	if viaLink.Worktree != r.dir {
		t.Errorf("symlink Worktree = %q, want %q", viaLink.Worktree, r.dir)
	}
	if viaLink.Identity() != sub.Identity() {
		t.Errorf("identity via symlink %q != %q", viaLink.Identity(), sub.Identity())
	}
}

func TestOpenSharesIdentityAcrossLinkedWorktrees(t *testing.T) {
	r := newTestRepo(t, "main")
	r.checkoutNew("feature")
	r.commit("feature-work")
	r.checkout("main")

	linked := filepath.Join(t.TempDir(), "linked")
	r.git("worktree", "add", "--quiet", linked, "feature")
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	primary := r.open()
	secondary, err := Open(context.Background(), linked)
	if err != nil {
		t.Fatalf("Open(linked): %v", err)
	}

	if secondary.Worktree != linked {
		t.Errorf("linked Worktree = %q, want %q", secondary.Worktree, linked)
	}
	if secondary.Worktree == primary.Worktree {
		t.Errorf("linked worktree path should differ from primary %q", primary.Worktree)
	}
	if secondary.Identity() != primary.Identity() {
		t.Errorf("identity differs across worktrees: %q vs %q", secondary.Identity(), primary.Identity())
	}
	if !filepath.IsAbs(secondary.CommonDir) {
		t.Errorf("linked CommonDir %q is not absolute", secondary.CommonDir)
	}
}

func TestWorkflowKey(t *testing.T) {
	got := WorkflowKey("/repo/.git", "refs/heads/feature/x")
	if want := "/repo/.git::refs/heads/feature/x"; got != want {
		t.Errorf("WorkflowKey = %q, want %q", got, want)
	}
}

func TestCleanEnvStripsRedirectingVariables(t *testing.T) {
	env := cleanEnv([]string{
		"GIT_DIR=/elsewhere", "PATH=/bin", "GIT_WORK_TREE=/x", "HOME=/h",
		"LC_ALL=en_US.UTF-8", "GIT_TERMINAL_PROMPT=1",
	})
	counts := map[string]int{}
	for _, kv := range env {
		if kv == "GIT_DIR=/elsewhere" || kv == "GIT_WORK_TREE=/x" {
			t.Errorf("cleanEnv kept %q", kv)
		}
		key, _, _ := strings.Cut(kv, "=")
		counts[key]++
	}
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"} {
		if !slices.Contains(env, want) {
			t.Errorf("cleanEnv = %v, missing %q", env, want)
		}
	}
	// The pinned keys must be the only definitions so the values do not
	// depend on how the platform resolves duplicate environment keys.
	for _, key := range []string{"LC_ALL", "GIT_TERMINAL_PROMPT"} {
		if counts[key] != 1 {
			t.Errorf("cleanEnv defines %s %d times, want exactly once: %v", key, counts[key], env)
		}
	}
}

func TestOpenIgnoresGitDirEnvironment(t *testing.T) {
	r := newTestRepo(t, "main")
	other := newTestRepo(t, "main")
	t.Setenv("GIT_DIR", filepath.Join(other.dir, ".git"))
	t.Setenv("GIT_WORK_TREE", other.dir)

	repo := r.open()
	if repo.Worktree != r.dir {
		t.Errorf("Worktree = %q, want %q despite GIT_DIR/GIT_WORK_TREE", repo.Worktree, r.dir)
	}
}
