package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostileGlobalConfig installs a global Git configuration that runs a
// post-checkout hook and defines a smudge filter, the way a user's Git LFS
// setup or hooks path would, and returns the marker path the hook touches.
func hostileGlobalConfig(t *testing.T) (marker string) {
	t.Helper()
	home := t.TempDir()
	hooks := filepath.Join(home, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(home, "hook-ran")
	hook := "#!/bin/sh\ntouch \"$COUNTERPOINT_TEST_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(hook), 0o700); err != nil { //nolint:gosec // executable test hook
		t.Fatal(err)
	}
	config := "[core]\n\thooksPath = " + hooks + "\n[filter \"marker\"]\n\tclean = cat\n\tsmudge = sh -c 'echo SMUDGED'\n\trequired = true\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("COUNTERPOINT_TEST_MARKER", marker)
	return marker
}

func TestCloneDetachedIsolatesHooksAndFilters(t *testing.T) {
	marker := hostileGlobalConfig(t)
	r := newTestRepo(t, "main")
	r.write(".gitattributes", "*.txt filter=marker\n")
	r.write("data.txt", "committed bytes\n")
	commit := r.commit("attributes")
	repo := r.open()

	// Control: without isolation the hostile configuration fires, so the
	// assertions below can fail.
	control := filepath.Join(t.TempDir(), "control")
	plain := exec.Command("git", "clone", "--quiet", "--shared", "--no-checkout", repo.CommonDir, control) //nolint:gosec // test control
	plain.Env = cleanEnv(os.Environ())
	if out, err := plain.CombinedOutput(); err != nil {
		t.Fatalf("control clone: %v\n%s", err, out)
	}
	co := exec.Command("git", "-C", control, "checkout", "--quiet", "--detach", commit) //nolint:gosec // test control
	co.Env = cleanEnv(os.Environ())
	if out, err := co.CombinedOutput(); err != nil {
		t.Fatalf("control checkout: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the hook did not run, so the fixture proves nothing: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(control, "data.txt")); !strings.Contains(string(b), "SMUDGED") {
		t.Fatalf("control: the filter did not run; data.txt = %q", b)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	hooksDir := filepath.Join(t.TempDir(), "empty-hooks")
	if err := os.Mkdir(hooksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "checkout")
	if err := repo.CloneDetached(context.Background(), dest, commit, hooksDir); err != nil {
		t.Fatalf("CloneDetached: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the global post-checkout hook ran while populating the checkout")
	}
	if b, err := os.ReadFile(filepath.Join(dest, "data.txt")); err != nil || string(b) != "committed bytes\n" {
		t.Errorf("data.txt = %q, %v; want the committed bytes with no filter applied", b, err)
	}
	head, err := run(context.Background(), dest, "rev-parse", "HEAD")
	if err != nil || head != commit {
		t.Errorf("checkout HEAD = %q, %v; want %s", head, err, commit)
	}
	if status := r.git("status", "--porcelain"); status != "" {
		t.Errorf("source repository modified:\n%s", status)
	}
}

func TestCloneDetachedRequiresAFullObjectID(t *testing.T) {
	r := newTestRepo(t, "main")
	repo := r.open()
	hooksDir := t.TempDir()
	err := repo.CloneDetached(context.Background(), filepath.Join(t.TempDir(), "co"), "HEAD", hooksDir)
	if !errors.Is(err, ErrInvalidCommit) {
		t.Fatalf("CloneDetached(HEAD) error = %v, want ErrInvalidCommit", err)
	}
}

func TestModifiedTrackedFilesCountsOnlyTrackedChanges(t *testing.T) {
	r := newTestRepo(t, "main")
	r.write("a.txt", "a\n")
	r.write("b.txt", "b\n")
	commit := r.commit("two files")
	repo := r.open()
	hooksDir := t.TempDir()
	dest := filepath.Join(t.TempDir(), "co")
	if err := repo.CloneDetached(context.Background(), dest, commit, hooksDir); err != nil {
		t.Fatal(err)
	}
	if n, err := ModifiedTrackedFiles(context.Background(), dest); n != 0 || err != nil {
		t.Fatalf("fresh checkout: %d, %v", n, err)
	}
	if err := os.WriteFile(filepath.Join(dest, "untracked.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := ModifiedTrackedFiles(context.Background(), dest); n != 0 || err != nil {
		t.Fatalf("after untracked file: %d, %v", n, err)
	}
	if err := os.WriteFile(filepath.Join(dest, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dest, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if n, err := ModifiedTrackedFiles(context.Background(), dest); n != 2 || err != nil {
		t.Fatalf("after modifying and deleting tracked files: %d, %v; want 2", n, err)
	}
}
