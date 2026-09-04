//go:build unix

package state

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// helperEnv marks the re-executed test binary as the lock-holding child.
const helperEnv = "COUNTERPOINT_TEST_LOCK_HOLDER"

// TestMain lets the test binary act as a lock-holding subprocess: when
// helperEnv names a lock path, it takes the lock, prints "locked", holds it
// until stdin closes, and exits.
func TestMain(m *testing.M) {
	if path := os.Getenv(helperEnv); path != "" {
		os.Exit(holdLock(path))
	}
	os.Exit(m.Run())
}

func holdLock(path string) int {
	l, err := AcquireLock(context.Background(), path, LockWait)
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		return 1
	}
	_, _ = os.Stdout.WriteString("locked\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := l.Release(); err != nil {
		return 1
	}
	return 0
}

// startHolder launches a subprocess that holds the lock at path until the
// returned release function is called.
func startHolder(t *testing.T, path string) (release func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0]) //nolint:gosec // re-executes this test binary
	cmd.Env = append(os.Environ(), helperEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		_ = cmd.Process.Kill()
		t.Fatalf("holder did not report the lock: %q, %v", line, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				t.Errorf("holder exit: %v", err)
			}
		})
	}
}

func TestAcquireLockCreatesFileAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")
	l, err := AcquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file not created: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}

	again, err := AcquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	_ = again.Release()
}

func TestAcquireLockExcludesAnotherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")
	release := startHolder(t, path)
	defer release()

	start := time.Now()
	_, err := AcquireLock(context.Background(), path, 300*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("AcquireLock while held elsewhere: error = %v, want ErrLocked", err)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("gave up after %v, want at least the 300ms wait", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("waited %v, want a bounded wait", elapsed)
	}

	release()
	l, err := AcquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("AcquireLock after holder exited: %v", err)
	}
	_ = l.Release()
}

func TestAcquireLockWaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")
	release := startHolder(t, path)
	defer release()

	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()
	l, err := AcquireLock(context.Background(), path, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireLock did not obtain the lock once released: %v", err)
	}
	_ = l.Release()
}

func TestAcquireLockExcludesSameProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")
	first, err := AcquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Release() }()

	_, err = AcquireLock(context.Background(), path, 200*time.Millisecond)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second in-process AcquireLock error = %v, want ErrLocked", err)
	}
}

func TestAcquireLockHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")
	release := startHolder(t, path)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := AcquireLock(ctx, path, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireLock with expiring context: error = %v, want DeadlineExceeded", err)
	}
}

func TestAcquireLockCreatesParentDirectory(t *testing.T) {
	// A fresh installation has no state directory yet; the first review
	// must be able to take the lock.
	path := filepath.Join(t.TempDir(), "fresh", "nested", "state.json.lock")
	l, err := AcquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("AcquireLock on a fresh nested path: %v", err)
	}
	defer func() { _ = l.Release() }()

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("parent directory not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != dirPerm {
		t.Errorf("parent directory mode = %o, want %o", perm, dirPerm)
	}
}

func TestAcquireLockPreCanceledHasNoSideEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh", "state.json.lock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := AcquireLock(ctx, path, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireLock with canceled context: error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("directory created despite canceled context: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file created despite canceled context: %v", err)
	}
}

func TestAcquireLockCancellationBeatsDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")
	release := startHolder(t, path)
	defer release()

	// Both the wait and the context expire together; cancellation must win
	// so the caller sees why it stopped.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := AcquireLock(ctx, path, 0)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrLocked) {
		t.Fatalf("error = %v, want context.Canceled and not ErrLocked", err)
	}
}

func TestAcquireLockRejectsUnusablePath(t *testing.T) {
	// The parent is a regular file, so the directory cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, filePerm); err != nil {
		t.Fatal(err)
	}
	_, err := AcquireLock(context.Background(), filepath.Join(blocker, "x.lock"), time.Second)
	if err == nil {
		t.Fatal("AcquireLock under a regular file: want error")
	}
}
