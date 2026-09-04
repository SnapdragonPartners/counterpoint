//go:build unix

package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// LockWait is how long AcquireLock keeps trying before reporting that
	// another review holds the lock. It is short on purpose: a blocked
	// caller should fail clearly rather than queue behind a full review.
	LockWait = 2 * time.Second
	// lockPollInterval is the retry interval while waiting for the lock.
	lockPollInterval = 100 * time.Millisecond
)

// ErrLocked reports that another Counterpoint process holds the review lock.
var ErrLocked = errors.New("another review is in progress")

// Lock is a held advisory file lock. It is released by Release, and by the
// operating system if the process exits.
type Lock struct {
	f *os.File
}

// AcquireLock takes an exclusive advisory lock on path, creating the file
// and its parent directory (mode 0700) if needed, since on a fresh
// installation the state directory does not exist until the first review.
// It retries until the lock is obtained, wait elapses, or ctx is done. The
// lock is per open file description, so it also excludes other goroutines in
// the same process that call AcquireLock.
func AcquireLock(ctx context.Context, path string, wait time.Duration) (*Lock, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create lock dir %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm) //nolint:gosec // lock file beside the state file, not user input
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("%w: lock file %s", ErrLocked, path)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}

// Release unlocks and closes the lock file. It is safe to call once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock file: %w", closeErr)
	}
	return nil
}
