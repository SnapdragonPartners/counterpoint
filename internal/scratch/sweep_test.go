//go:build unix

package scratch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

// workflowDirName is the directory Prepare uses for a workflow key.
func workflowDirName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:keyHashLen]
}

// fakeWorkflow builds a workflow directory as Prepare would have left it:
// a lock file, a cache with content, and a stamp with the given age. A zero
// age omits the stamp. It returns the directory.
func fakeWorkflow(t *testing.T, root, name string, age time.Duration, withStamp bool) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, cacheName, "go-build", "ab"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheName, "go-build", "ab", "obj"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if withStamp {
		if err := os.WriteFile(filepath.Join(dir, usedName), []byte("t\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(dir, usedName), when, when); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(dir, cacheName), when, when); err != nil {
		t.Fatal(err)
	}
	return dir
}

// statT returns the platform stat of a FileInfo, failing the test if the
// FileInfo has none.
func statT(t *testing.T, info os.FileInfo) *syscall.Stat_t {
	t.Helper()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no Stat_t for %s", info.Name())
	}
	return st
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// prepareOther runs Prepare for a distinct workflow under root, which
// triggers the sweep, and closes it.
func prepareOther(t *testing.T, root string, hooks sweepHooks) {
	t.Helper()
	repo, commit := newRepo(t)
	co, err := Prepare(context.Background(), Options{Root: root, WorkflowKey: repo.Identity() + "::refs/heads/other", Repo: repo, Commit: commit, hooks: hooks})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	_ = co.Close()
}

func TestSweepRemovesStaleCacheAndLeavesATombstone(t *testing.T) {
	root := newRoot(t)
	key := "some-repo::refs/heads/stale"
	dir := fakeWorkflow(t, root, workflowDirName(key), 4*24*time.Hour, true)
	for _, extra := range []string{checkoutName, tmpName, hooksName} {
		if err := os.Mkdir(filepath.Join(dir, extra), dirPerm); err != nil {
			t.Fatal(err)
		}
	}
	prepareOther(t, root, sweepHooks{})
	for _, gone := range []string{cacheName, checkoutName, tmpName, hooksName, usedName} {
		if exists(filepath.Join(dir, gone)) {
			t.Errorf("%s still present after the sweep", gone)
		}
	}
	if !exists(dir) || !exists(filepath.Join(dir, lockName)) {
		t.Error("tombstone (directory and lock file) missing")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("tombstone holds %d entries, want only the lock file", len(entries))
	}
	// The swept workflow is usable again with a cold cache and a new stamp.
	repo, commit := newRepo(t)
	co, err := Prepare(context.Background(), Options{Root: root, WorkflowKey: key, Repo: repo, Commit: commit})
	if err != nil {
		t.Fatalf("Prepare after sweep: %v", err)
	}
	defer co.Close()
	if co.CacheDir != filepath.Join(dir, cacheName) || !exists(co.CacheDir) || !exists(filepath.Join(dir, usedName)) {
		t.Errorf("workflow not rebuilt in its tombstone: cache=%s", co.CacheDir)
	}
}

func TestSweepKeepsFreshAndUnknownEntries(t *testing.T) {
	root := newRoot(t)
	fresh := fakeWorkflow(t, root, workflowDirName("fresh"), time.Hour, true)
	oldStampFreshCache := fakeWorkflow(t, root, workflowDirName("nostamp-fresh"), time.Hour, false)
	tomb := filepath.Join(root, workflowDirName("tomb"))
	if err := os.MkdirAll(tomb, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tomb, lockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prepareOther(t, root, sweepHooks{})
	for _, d := range []string{fresh, oldStampFreshCache} {
		if !exists(filepath.Join(d, cacheName, "go-build", "ab", "obj")) {
			t.Errorf("cache of %s swept although fresh", filepath.Base(d))
		}
	}
	if entries, _ := os.ReadDir(tomb); len(entries) != 1 {
		t.Errorf("tombstone changed: %d entries", len(entries))
	}
}

func TestSweepUsesCacheMtimeWhenThereIsNoStamp(t *testing.T) {
	root := newRoot(t)
	dir := fakeWorkflow(t, root, workflowDirName("legacy"), 4*24*time.Hour, false)
	prepareOther(t, root, sweepHooks{})
	if exists(filepath.Join(dir, cacheName)) {
		t.Error("legacy cache with an old mtime not swept")
	}
}

func TestSweepSkipsTheCurrentWorkflowAndRefreshesItsStamp(t *testing.T) {
	root := newRoot(t)
	repo, commit := newRepo(t)
	key := repo.Identity() + "::refs/heads/main"
	dir := fakeWorkflow(t, root, workflowDirName(key), 4*24*time.Hour, true)
	before, _ := os.Lstat(filepath.Join(dir, usedName))
	co, err := Prepare(context.Background(), Options{Root: root, WorkflowKey: key, Repo: repo, Commit: commit})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if !exists(filepath.Join(dir, cacheName, "go-build", "ab", "obj")) {
		t.Error("the current workflow's cache was swept")
	}
	after, err := os.Lstat(filepath.Join(dir, usedName))
	if err != nil || !after.ModTime().After(before.ModTime()) || !after.Mode().IsRegular() {
		t.Errorf("stamp not refreshed: before %v after %v (%v)", before.ModTime(), after.ModTime(), err)
	}
}

func TestSweepSkipsAnEntryWhoseLockIsHeldWithoutWaiting(t *testing.T) {
	root := newRoot(t)
	dir := fakeWorkflow(t, root, workflowDirName("held"), 4*24*time.Hour, true)
	held, err := state.AcquireLock(context.Background(), filepath.Join(dir, lockName), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	start := time.Now()
	prepareOther(t, root, sweepHooks{})
	if time.Since(start) > 5*time.Second {
		t.Error("sweep waited on a held lock")
	}
	if !exists(filepath.Join(dir, cacheName, "go-build", "ab", "obj")) {
		t.Error("cache swept while its workflow was locked elsewhere")
	}
}

func TestSweepIgnoresWhatItDidNotCreate(t *testing.T) {
	root := newRoot(t)
	if err := os.MkdirAll(root, dirPerm); err != nil {
		t.Fatal(err)
	}
	old := 4 * 24 * time.Hour
	// A lookalike: hex name, old cache, no lock file.
	look := filepath.Join(root, "0123456789abcdef")
	if err := os.MkdirAll(filepath.Join(look, cacheName), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(look, cacheName, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-old)
	_ = os.Chtimes(filepath.Join(look, cacheName), when, when)
	// A non-hex directory that otherwise looks swept-worthy.
	other := fakeWorkflow(t, root, "not-a-workflow", old, true)
	// A regular file and a symlink to a stale workflow-shaped directory.
	if err := os.WriteFile(filepath.Join(root, "fedcba9876543210"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := fakeWorkflow(t, filepath.Dir(root), "target", old, true)
	if err := os.Symlink(target, filepath.Join(root, "abcdefabcdefabcd")); err != nil {
		t.Fatal(err)
	}
	prepareOther(t, root, sweepHooks{})
	if !exists(filepath.Join(look, cacheName, "f")) || exists(filepath.Join(look, lockName)) {
		t.Error("lookalike touched, or a lock file created in it")
	}
	if !exists(filepath.Join(other, cacheName, "go-build", "ab", "obj")) {
		t.Error("non-hex directory swept")
	}
	if !exists(filepath.Join(root, "fedcba9876543210")) {
		t.Error("regular file removed")
	}
	if !exists(filepath.Join(target, cacheName, "go-build", "ab", "obj")) || !exists(filepath.Join(root, "abcdefabcdefabcd")) {
		t.Error("symlinked entry followed or removed")
	}
}

func TestPrepareReplacesALinkedStampWithoutTouchingTheTarget(t *testing.T) {
	root := newRoot(t)
	repo, commit := newRepo(t)
	key := repo.Identity() + "::refs/heads/main"
	dir := filepath.Join(root, workflowDirName(key))
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(outside, old, old)
	for name, plant := range map[string]func() error{
		"symlink":   func() error { return os.Symlink(outside, filepath.Join(dir, usedName)) },
		"hard link": func() error { return os.Link(outside, filepath.Join(dir, usedName)) },
	} {
		_ = os.Remove(filepath.Join(dir, usedName))
		if err := plant(); err != nil {
			t.Fatal(err)
		}
		co, err := Prepare(context.Background(), Options{Root: root, WorkflowKey: key, Repo: repo, Commit: commit})
		if err != nil {
			t.Fatalf("%s: Prepare: %v", name, err)
		}
		_ = co.Close()
		info, err := os.Lstat(filepath.Join(dir, usedName))
		if err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s: stamp is not a fresh regular file: %v %v", name, info, err)
		}
		b, _ := os.ReadFile(outside)
		vi, err := os.Stat(outside)
		if err != nil {
			t.Fatal(err)
		}
		if nlink := statT(t, vi).Nlink; string(b) != "keep me\n" || !vi.ModTime().Equal(old) || nlink != 1 {
			t.Errorf("%s: target modified: content %q mtime %v nlink %d", name, b, vi.ModTime(), nlink)
		}
	}
}

func TestSweepSkipsALockReplacedBeforeItIsOpened(t *testing.T) {
	root := newRoot(t)
	dir := fakeWorkflow(t, root, workflowDirName("swap"), 4*24*time.Hour, true)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, swap := range map[string]func() error{
		"symlink":   func() error { return os.Symlink(outside, filepath.Join(dir, lockName)) },
		"hard link": func() error { return os.Link(outside, filepath.Join(dir, lockName)) },
	} {
		if err := os.WriteFile(filepath.Join(dir, lockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		hooks := sweepHooks{beforeLock: func(string) {
			_ = os.Remove(filepath.Join(dir, lockName))
			if err := swap(); err != nil {
				t.Fatal(err)
			}
		}}
		prepareOther(t, root, hooks)
		if !exists(filepath.Join(dir, cacheName, "go-build", "ab", "obj")) {
			t.Errorf("%s: cache swept although the lock was swapped", name)
		}
	}
}

func TestSweepRechecksAgeUnderTheLock(t *testing.T) {
	root := newRoot(t)
	dir := fakeWorkflow(t, root, workflowDirName("refreshed"), 4*24*time.Hour, true)
	hooks := sweepHooks{beforeLock: func(string) {
		now := time.Now()
		if err := os.Chtimes(filepath.Join(dir, usedName), now, now); err != nil {
			t.Fatal(err)
		}
	}}
	prepareOther(t, root, hooks)
	if !exists(filepath.Join(dir, cacheName, "go-build", "ab", "obj")) {
		t.Error("cache swept although the stamp was refreshed before the lock")
	}
}

func TestSweepRemovesLeftoverTrashEvenWithoutStampOrCache(t *testing.T) {
	root := newRoot(t)
	dir := filepath.Join(root, workflowDirName("cancelled"))
	if err := os.MkdirAll(filepath.Join(dir, trashPrefix+"old", "deep"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, trashPrefix+"old", "deep", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prepareOther(t, root, sweepHooks{})
	if exists(filepath.Join(dir, trashPrefix+"old")) {
		t.Error("leftover trash not removed")
	}
}

func TestSweepRemovesTrashButNotLiveItemsOfAFreshEntry(t *testing.T) {
	root := newRoot(t)
	dir := fakeWorkflow(t, root, workflowDirName("fresh-with-trash"), time.Hour, true)
	if err := os.MkdirAll(filepath.Join(dir, trashPrefix+"x", "sub"), dirPerm); err != nil {
		t.Fatal(err)
	}
	prepareOther(t, root, sweepHooks{})
	if exists(filepath.Join(dir, trashPrefix+"x")) {
		t.Error("trash of a fresh entry not removed")
	}
	if !exists(filepath.Join(dir, cacheName, "go-build", "ab", "obj")) {
		t.Error("live cache of a fresh entry removed")
	}
}

func TestSweepUnlinksLinksInsideTrashWithoutFollowingThem(t *testing.T) {
	root := newRoot(t)
	dir := fakeWorkflow(t, root, workflowDirName("links"), time.Hour, true)
	trash := filepath.Join(dir, trashPrefix+"l")
	if err := os.Mkdir(trash, dirPerm); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(trash, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", cacheName), filepath.Join(trash, "sibling")); err != nil {
		t.Fatal(err)
	}
	prepareOther(t, root, sweepHooks{})
	if exists(trash) {
		t.Error("trash with links not removed")
	}
	if !exists(filepath.Join(outside, "f")) {
		t.Error("link outside the directory was followed")
	}
	if !exists(filepath.Join(dir, cacheName, "go-build", "ab", "obj")) {
		t.Error("link to the live cache was followed")
	}
}

func TestSweepLeavesADirectoryOnAnotherDevice(t *testing.T) {
	root := newRoot(t)
	dir := fakeWorkflow(t, root, workflowDirName("device"), time.Hour, true)
	trash := filepath.Join(dir, trashPrefix+"d")
	if err := os.MkdirAll(filepath.Join(trash, "mounted", "inner"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trash, "mounted", "inner", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trash, "plain"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mountedInfo, err := os.Lstat(filepath.Join(trash, "mounted"))
	if err != nil {
		t.Fatal(err)
	}
	mountedIno := statT(t, mountedInfo).Ino
	var seen []uint64
	hooks := sweepHooks{sameDevice: func(base, d *unix.Stat_t) bool {
		seen = append(seen, d.Ino)
		return d.Ino != mountedIno
	}}
	prepareOther(t, root, hooks)
	if !exists(filepath.Join(trash, "mounted", "inner", "f")) {
		t.Error("directory refused by the device check was removed")
	}
	if exists(filepath.Join(trash, "plain")) {
		t.Error("sibling of the refused directory not removed")
	}
	if exists(filepath.Join(dir, cacheName)) == false {
		t.Error("live cache touched")
	}
	found := false
	for _, ino := range seen {
		if ino == mountedIno {
			found = true
		}
	}
	if !found {
		t.Error("device predicate never saw the opened directory's inode")
	}
}

func TestSweepStopsOnCancellationAndFinishesLater(t *testing.T) {
	root := newRoot(t)
	dir := fakeWorkflow(t, root, workflowDirName("cancel"), 4*24*time.Hour, true)
	for _, sub := range []string{"a", "b", "c", "d"} {
		if err := os.MkdirAll(filepath.Join(dir, cacheName, sub, "x"), dirPerm); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, cacheName, sub, "x", "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	hooks := sweepHooks{sameDevice: func(base, d *unix.Stat_t) bool {
		calls++
		if calls == 3 {
			cancel() // mid-walk, with directories still to remove
		}
		return true
	}}
	log := slog.New(slog.DiscardHandler)
	swept := sweep(ctx, root, "", time.Now(), log, hooks)
	if swept != 1 {
		t.Errorf("swept = %d, want 1 (the entry was renamed to trash before cancellation)", swept)
	}
	entries, _ := os.ReadDir(dir)
	trashLeft := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), trashPrefix) {
			trashLeft = true
		}
	}
	if !trashLeft {
		t.Fatal("cancellation left no trash behind, so nothing was interrupted")
	}
	if exists(filepath.Join(dir, cacheName)) {
		t.Error("live cache name still present after rename to trash")
	}
	// The lock was released: a later sweep finishes the job.
	if l, err := state.AcquireLock(context.Background(), filepath.Join(dir, lockName), 0); err != nil {
		t.Errorf("lock still held after cancellation: %v", err)
	} else {
		_ = l.Release()
	}
	sweep(context.Background(), root, "", time.Now(), log, sweepHooks{})
	entries, _ = os.ReadDir(dir)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("after the second sweep the tombstone holds %v, want only the lock file", names)
	}
}

func TestSweepLogsARemovalErrorAndContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission denial does not apply to root")
	}
	root := newRoot(t)
	bad := fakeWorkflow(t, root, workflowDirName("bad"), 4*24*time.Hour, true)
	good := fakeWorkflow(t, root, workflowDirName("good"), 4*24*time.Hour, true)
	stuck := filepath.Join(bad, trashPrefix+"stuck")
	if err := os.Mkdir(stuck, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, dirPerm) })
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	swept := sweep(context.Background(), root, "", time.Now(), log, sweepHooks{})
	if !exists(filepath.Join(stuck, "f")) {
		t.Fatal("fixture: the read-only trash directory was emptied, so no error occurred")
	}
	if exists(filepath.Join(good, cacheName)) {
		t.Error("the sweep stopped at the failing entry instead of continuing")
	}
	if !strings.Contains(logged.String(), "entry skipped") || !strings.Contains(logged.String(), "permission denied") {
		t.Errorf("removal error not logged:\n%s", logged.String())
	}
	if swept < 1 {
		t.Errorf("swept = %d", swept)
	}
}

// A link at a live-item name is left in place and does not stop the rest
// of the entry from being swept.
func TestSweepSkipsALinkedLiveItemAndSweepsTheRest(t *testing.T) {
	root := newRoot(t)
	dir := fakeWorkflow(t, root, workflowDirName("linked-item"), 4*24*time.Hour, true)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The link sits at the first name the sweep visits (the cache), so
	// every other item comes after it; the real cache moves to hooks.
	if err := os.Rename(filepath.Join(dir, cacheName), filepath.Join(dir, hooksName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, cacheName)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, trashPrefix+"old", "d"), dirPerm); err != nil {
		t.Fatal(err)
	}
	prepareOther(t, root, sweepHooks{})
	if exists(filepath.Join(dir, hooksName)) || exists(filepath.Join(dir, usedName)) {
		t.Error("items after the linked one were not swept")
	}
	if exists(filepath.Join(dir, trashPrefix+"old")) {
		t.Error("trash not removed after the linked item")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), trashPrefix) {
			t.Errorf("new trash %s left behind", e.Name())
		}
	}
	if info, err := os.Lstat(filepath.Join(dir, cacheName)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("linked item removed or followed: %v %v", info, err)
	}
	if !exists(filepath.Join(outside, "f")) {
		t.Error("link target touched")
	}
}
