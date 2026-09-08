//go:build unix

package scratch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

// Retention of build caches (issue #15, docs/design/cache-sweep.md). A
// workflow's cache is kept while the workflow has been used within
// MaxCacheAge and swept afterwards by the next build-capable review of any
// workflow. Use means Prepare ran for the workflow; it is recorded in a
// stamp file whose modification time is the last-use time.
const (
	// MaxCacheAge is how long an unused workflow's cache is kept.
	MaxCacheAge = 72 * time.Hour

	// usedName is the stamp file inside a workflow directory.
	usedName = "used"
	// trashPrefix names an item renamed out of use and awaiting removal.
	trashPrefix = "trash-"

	dirOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
)

// workflowNamePattern matches the directories Prepare creates: a prefix of
// the workflow key's hash. Nothing else under the root is Counterpoint's.
var workflowNamePattern = regexp.MustCompile(`^[0-9a-f]{16}$`) //nolint:gochecknoglobals // compiled once

// sweepHooks are test seams; nil means no hook.
type sweepHooks struct {
	// sameDevice decides whether a directory opened during trash removal
	// is on the workflow directory's device; nil compares device ids.
	sameDevice func(base, dir *unix.Stat_t) bool
	// beforeLock runs after an entry is judged stale and before its lock
	// is tried, with the entry's name.
	beforeLock func(name string)
}

// stamp records that the workflow directory was used now. The stamp is
// never written in place: a fresh file is created exclusively and renamed
// over usedName, so an existing entry of any kind is replaced, not
// followed or modified. workflowDir has been verified not to be a link.
func stamp(workflowDir string) error {
	tmp, err := os.CreateTemp(workflowDir, usedName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create stamp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(time.Now().UTC().Format(time.RFC3339) + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write stamp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close stamp: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(workflowDir, usedName)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename stamp: %w", err)
	}
	return nil
}

// sweep removes stale caches under root, skipping the entry named exclude
// (the current workflow). It never fails the caller: problems with one
// entry are logged and the next is tried; only context cancellation ends
// it early. It returns how many entries were swept. Every operation on an
// entry is relative to a descriptor for that entry, opened without
// following links, so nothing that happens to the path in the meantime
// substitutes another directory; see docs/design/cache-sweep.md and ADR
// 0001 for what that does and does not defend against.
func sweep(ctx context.Context, root, exclude string, now time.Time, log *slog.Logger, hooks sweepHooks) int {
	rootFD, err := unix.Open(root, dirOpenFlags, 0)
	if err != nil {
		log.Warn("scratch sweep: open root", "root", root, "error", err)
		return 0
	}
	rootFile := os.NewFile(uintptr(rootFD), root)
	defer rootFile.Close()
	entries, err := rootFile.ReadDir(-1)
	if err != nil {
		log.Warn("scratch sweep: list root", "root", root, "error", err)
		return 0
	}

	swept := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			log.Info("scratch sweep interrupted", "swept", swept, "error", err)
			return swept
		}
		name := e.Name()
		if name == exclude || !workflowNamePattern.MatchString(name) {
			continue
		}
		did, err := sweepEntry(ctx, rootFile, name, now, log, hooks)
		if did {
			swept++ // its live items are in trash even if removal did not finish
		}
		if err != nil {
			if ctx.Err() != nil {
				log.Info("scratch sweep interrupted", "entry", name, "swept", swept, "error", err)
				return swept
			}
			log.Warn("scratch sweep: entry skipped", "entry", name, "error", err)
		}
	}
	if swept > 0 {
		log.Info("scratch sweep completed", "root", root, "swept", swept, "max_age", MaxCacheAge)
	}
	return swept
}

// entryState is what the sweep reads about an entry, always through its
// descriptor with no link following.
type entryState struct {
	hasTrash bool
	lastUse  time.Time // zero when unknown
}

func readEntry(dir *os.File) (entryState, error) {
	var st entryState
	names, err := listNames(dir)
	if err != nil {
		return st, err
	}
	for _, n := range names {
		if strings.HasPrefix(n, trashPrefix) {
			st.hasTrash = true
			break
		}
	}
	fd := int(dir.Fd())
	var used unix.Stat_t
	if err := unix.Fstatat(fd, usedName, &used, unix.AT_SYMLINK_NOFOLLOW); err == nil && used.Mode&unix.S_IFMT == unix.S_IFREG {
		st.lastUse = mtime(&used)
		return st, nil
	}
	var cache unix.Stat_t
	if err := unix.Fstatat(fd, cacheName, &cache, unix.AT_SYMLINK_NOFOLLOW); err == nil && cache.Mode&unix.S_IFMT == unix.S_IFDIR {
		st.lastUse = mtime(&cache)
	}
	return st, nil
}

func (st entryState) stale(now time.Time) bool {
	return !st.lastUse.IsZero() && now.Sub(st.lastUse) > MaxCacheAge
}

// sweepEntry judges and, when due, sweeps one entry. It reports whether the
// entry's live items were moved to trash.
func sweepEntry(ctx context.Context, rootFile *os.File, name string, now time.Time, log *slog.Logger, hooks sweepHooks) (bool, error) {
	fd, err := unix.Openat(int(rootFile.Fd()), name, dirOpenFlags, 0)
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) {
			return false, nil // a link, a file, or gone: not a workflow directory
		}
		return false, fmt.Errorf("open: %w", err)
	}
	dir := os.NewFile(uintptr(fd), filepath.Join(rootFile.Name(), name))
	defer dir.Close()
	var base unix.Stat_t
	if err := unix.Fstat(fd, &base); err != nil {
		return false, fmt.Errorf("fstat: %w", err)
	}

	// Ownership: only a directory Prepare made has a lock file, and the
	// sweep never creates one.
	var lockSt unix.Stat_t
	if err := unix.Fstatat(fd, lockName, &lockSt, unix.AT_SYMLINK_NOFOLLOW); err != nil || lockSt.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, nil
	}

	st, err := readEntry(dir)
	if err != nil {
		return false, err
	}
	if !st.hasTrash && !st.stale(now) {
		return false, nil
	}
	if hooks.beforeLock != nil {
		hooks.beforeLock(name)
	}

	// The lock: opened without creating or following, validated as the
	// file the ownership check saw, then tried without waiting.
	lfd, err := unix.Openat(fd, lockName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, nil
	}
	lockFile := os.NewFile(uintptr(lfd), filepath.Join(dir.Name(), lockName))
	var opened unix.Stat_t
	if err := unix.Fstat(lfd, &opened); err != nil || opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Nlink != 1 ||
		opened.Dev != lockSt.Dev || opened.Ino != lockSt.Ino {
		_ = lockFile.Close()
		return false, nil
	}
	lock, err := state.TryLockFile(lockFile)
	if err != nil {
		_ = lockFile.Close()
		if errors.Is(err, state.ErrLocked) {
			return false, nil // in review elsewhere
		}
		return false, err
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			log.Warn("scratch sweep: release lock", "entry", name, "error", rerr)
		}
	}()

	// Decide again under the lock: another Prepare may have refreshed the
	// stamp meanwhile.
	st, err = readEntry(dir)
	if err != nil {
		return false, err
	}
	sweptLive := false
	if st.stale(now) {
		for _, item := range []string{cacheName, checkoutName, tmpName, hooksName, usedName} {
			var ist unix.Stat_t
			if err := unix.Fstatat(fd, item, &ist, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				continue // absent
			}
			if ist.Mode&unix.S_IFMT == unix.S_IFLNK {
				// Not Counterpoint's to move: left in place, and the other
				// items and any trash are still handled.
				log.Warn("scratch sweep: linked item left in place", "path", filepath.Join(dir.Name(), item))
				continue
			}
			trash := trashPrefix + randomSuffix()
			if err := unix.Renameat(fd, item, fd, trash); err != nil {
				log.Warn("scratch sweep: item not moved to trash", "path", filepath.Join(dir.Name(), item), "error", err)
				continue
			}
			sweptLive = true
		}
	}

	names, err := listNames(dir)
	if err != nil {
		return sweptLive, err
	}
	var errs []error
	for _, n := range names {
		if !strings.HasPrefix(n, trashPrefix) {
			continue
		}
		if err := removeTree(ctx, dir, n, &base, hooks.sameDevice, log); err != nil {
			if ctx.Err() != nil {
				return sweptLive, err
			}
			errs = append(errs, err)
		}
	}
	return sweptLive, errors.Join(errs...)
}

// removeTree removes the entry name inside the directory parent with a
// descriptor-relative walk: child directories are opened without following
// links and listed through their descriptor, files and links are unlinked
// relative to the directory that listed them, directories are removed
// post-order, the context is checked between entries, and a directory on
// another device than base is left in place. This is the strategy
// os.RemoveAll uses, plus the context and device checks.
func removeTree(ctx context.Context, parent *os.File, name string, base *unix.Stat_t, sameDevice func(base, dir *unix.Stat_t) bool, log *slog.Logger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pfd := int(parent.Fd())
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(pfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("unlink %s: %w", filepath.Join(parent.Name(), name), err)
		}
		return nil
	}
	fd, err := unix.Openat(pfd, name, dirOpenFlags, 0)
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			// Became a link or a file since the stat: unlink it as such.
			if err := unix.Unlinkat(pfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("unlink %s: %w", name, err)
			}
			return nil
		}
		return fmt.Errorf("open %s: %w", name, err)
	}
	dir := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		_ = dir.Close()
		return fmt.Errorf("fstat %s: %w", name, err)
	}
	if sameDevice == nil {
		sameDevice = func(base, dir *unix.Stat_t) bool { return base.Dev == dir.Dev }
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFDIR || !sameDevice(base, &opened) {
		_ = dir.Close()
		log.Warn("scratch sweep: directory on another device left in place", "path", filepath.Join(parent.Name(), name))
		return nil
	}
	children, err := listNames(dir)
	if err != nil {
		_ = dir.Close()
		return err
	}
	for _, c := range children {
		if err := removeTree(ctx, dir, c, base, sameDevice, log); err != nil {
			_ = dir.Close()
			return err
		}
	}
	_ = dir.Close()
	if err := unix.Unlinkat(pfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("rmdir %s: %w", filepath.Join(parent.Name(), name), err)
	}
	return nil
}

// listNames returns the entry names of an open directory. The directory
// position is rewound first so a file can be listed more than once.
func listNames(dir *os.File) ([]string, error) {
	if _, err := dir.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("rewind %s: %w", dir.Name(), err)
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir.Name(), err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func mtime(st *unix.Stat_t) time.Time {
	return time.Unix(st.Mtim.Sec, st.Mtim.Nsec) //nolint:unconvert // field widths differ by platform
}

func randomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
