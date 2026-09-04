package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedBufferRetainsPrefixAndFlagsOverflow(t *testing.T) {
	b := &boundedBuffer{limit: 5}
	for _, chunk := range []string{"ab", "cde", "fgh", "i"} {
		n, err := b.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil so the child never blocks", chunk, n, err, len(chunk))
		}
	}
	if got := b.String(); got != "abcde" {
		t.Errorf("retained %q, want %q", got, "abcde")
	}
	if !b.overflow {
		t.Error("overflow = false after exceeding the limit")
	}

	exact := &boundedBuffer{limit: 3}
	_, _ = exact.Write([]byte("abc"))
	if exact.overflow {
		t.Error("overflow = true when output exactly fills the limit")
	}
}

func TestRunBoundedReportsTruncation(t *testing.T) {
	r := newTestRepo(t, "main")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		r.write(name, "x\n")
	}

	out, truncated, err := runBounded(context.Background(), r.dir, 4, "status", "--porcelain")
	if err != nil {
		t.Fatalf("runBounded: %v", err)
	}
	if !truncated || len(out) > 4 {
		t.Errorf("out = %q (truncated=%v), want at most 4 bytes and truncated", out, truncated)
	}

	_, _, err = runBounded(context.Background(), r.dir, 4, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("runBounded(rev-parse): %v", err)
	}
	_, err = run(context.Background(), r.dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("run(status) within limit: %v", err)
	}
}

func TestRunRejectsOversizedOutput(t *testing.T) {
	r := newTestRepo(t, "main")
	r.write("a.txt", "x\n")

	_, err := runWithLimit(context.Background(), r.dir, 1, "status", "--porcelain")
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("runWithLimit(limit 1) error = %v, want ErrOutputTooLarge", err)
	}
}

func TestRequireCleanTreatsTruncatedStatusAsDirty(t *testing.T) {
	r := newTestRepo(t, "main")
	r.write("a.txt", "x\n")
	r.write("b.txt", "x\n")
	repo := r.open()

	err := repo.requireCleanBounded(context.Background(), 3)
	if !errors.Is(err, ErrDirtyWorktree) {
		t.Fatalf("requireCleanBounded error = %v, want ErrDirtyWorktree", err)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error %q should say the output was truncated", err)
	}
}

func TestCancellationIsNotReportedAsAbsence(t *testing.T) {
	r := newTestRepo(t, "main")
	r.checkoutNew("feature")
	tip := r.commit("work")
	repo := r.open()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.ResolveBranch(ctx, "feature")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBranchNotFound) {
		t.Errorf("ResolveBranch error = %v, want context.Canceled and not ErrBranchNotFound", err)
	}
	_, err = repo.ResolveCommit(ctx, tip)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCommitNotFound) {
		t.Errorf("ResolveCommit error = %v, want context.Canceled and not ErrCommitNotFound", err)
	}
	_, _, err = repo.PrimaryBranch(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNoPrimaryBranch) {
		t.Errorf("PrimaryBranch error = %v, want context.Canceled and not ErrNoPrimaryBranch", err)
	}
	_, err = Open(ctx, r.dir)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotRepository) {
		t.Errorf("Open error = %v, want context.Canceled and not ErrNotRepository", err)
	}
}

func TestRepositoryFailureIsNotReportedAsAbsence(t *testing.T) {
	r := newTestRepo(t, "main")
	r.checkoutNew("feature")
	tip := r.commit("work")
	repo := r.open()

	// Removing the object store makes every Git command fail with exit
	// status 128 rather than the not-found status 1.
	if err := os.RemoveAll(filepath.Join(r.dir, ".git", "objects")); err != nil {
		t.Fatalf("remove objects: %v", err)
	}
	ctx := context.Background()

	_, err := repo.ResolveBranch(ctx, "feature")
	if err == nil || errors.Is(err, ErrBranchNotFound) || errors.Is(err, ErrInvalidBranch) {
		t.Errorf("ResolveBranch error = %v, want an operational error", err)
	}
	_, err = repo.ResolveCommit(ctx, tip)
	if err == nil || errors.Is(err, ErrCommitNotFound) {
		t.Errorf("ResolveCommit error = %v, want an operational error", err)
	}
	_, _, err = repo.PrimaryBranch(ctx)
	if err == nil || errors.Is(err, ErrNoPrimaryBranch) {
		t.Errorf("PrimaryBranch error = %v, want an operational error", err)
	}
	exists, err := repo.refExists(ctx, "refs/heads/feature")
	if err == nil || exists {
		t.Errorf("refExists = %v, %v on a broken repository; want false and an error", exists, err)
	}
}

func TestOpenDistinguishesNotRepositoryFromOtherFailures(t *testing.T) {
	r := newTestRepo(t, "main")
	bare := filepath.Join(t.TempDir(), "bare.git")
	r.git("clone", "--quiet", "--bare", r.dir, bare)

	_, err := Open(context.Background(), bare)
	if !errors.Is(err, ErrNotRepository) {
		t.Errorf("Open(bare) error = %v, want ErrNotRepository", err)
	}
}
