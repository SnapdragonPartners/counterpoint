package gitrepo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeBranchName(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  error
	}{
		{in: "feature/x", want: "feature/x"},
		{in: "refs/heads/feature/x", want: "feature/x"},
		{in: "", err: ErrInvalidBranch},
		{in: "refs/heads/", err: ErrInvalidBranch},
		{in: "refs/remotes/origin/main", err: ErrInvalidBranch},
		{in: "refs/tags/v1", err: ErrInvalidBranch},
		{in: "-x", err: ErrInvalidBranch},
		{in: "--output=/tmp/x", err: ErrInvalidBranch},
	}
	for _, tc := range cases {
		got, err := normalizeBranchName(tc.in)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("normalizeBranchName(%q) error = %v, want %v", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("normalizeBranchName(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestResolveBranchAcceptsBareAndFullRef(t *testing.T) {
	r := newTestRepo(t, "main")
	r.checkoutNew("feature/x")
	tip := r.commit("work")
	repo := r.open()
	ctx := context.Background()

	for _, name := range []string{"feature/x", "refs/heads/feature/x"} {
		b, err := repo.ResolveBranch(ctx, name)
		if err != nil {
			t.Fatalf("ResolveBranch(%q): %v", name, err)
		}
		if b.Name != "feature/x" || b.Ref != "refs/heads/feature/x" || b.Tip != tip {
			t.Errorf("ResolveBranch(%q) = %+v, want name feature/x ref refs/heads/feature/x tip %s", name, b, tip)
		}
	}
}

func TestResolveBranchRejectsInvalidNames(t *testing.T) {
	r := newTestRepo(t, "main")
	repo := r.open()
	ctx := context.Background()

	for _, name := range []string{"bad..name", "trailing.lock", "space name", "back\\slash", "refs/tags/v1", "-x", "a//b", ".hidden", "ends/"} {
		_, err := repo.ResolveBranch(ctx, name)
		if !errors.Is(err, ErrInvalidBranch) {
			t.Errorf("ResolveBranch(%q) error = %v, want ErrInvalidBranch", name, err)
		}
	}
}

func TestResolveBranchRejectsMissingAndRemoteTracking(t *testing.T) {
	r := newTestRepo(t, "main")
	r.addOriginClone("main")
	repo := r.open()
	ctx := context.Background()

	_, err := repo.ResolveBranch(ctx, "nope")
	if !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("ResolveBranch(nope) error = %v, want ErrBranchNotFound", err)
	}

	// origin/main exists as a remote-tracking ref but not as a local branch.
	_, err = repo.ResolveBranch(ctx, "origin/main")
	if !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("ResolveBranch(origin/main) error = %v, want ErrBranchNotFound", err)
	}
	if got := err.Error(); !strings.Contains(got, "remote-tracking") {
		t.Errorf("error %q should mention remote-tracking", got)
	}
}

func TestResolveBranchRejectsTagWithSameName(t *testing.T) {
	r := newTestRepo(t, "main")
	r.git("tag", "release")
	repo := r.open()

	_, err := repo.ResolveBranch(context.Background(), "release")
	if !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("ResolveBranch(tag name) error = %v, want ErrBranchNotFound", err)
	}
}

func TestPrimaryBranchFromOriginHead(t *testing.T) {
	r := newTestRepo(t, "trunk")
	r.addOriginClone("trunk")
	repo := r.open()

	name, ref, err := repo.PrimaryBranch(context.Background())
	if err != nil {
		t.Fatalf("PrimaryBranch: %v", err)
	}
	if name != "trunk" || ref != "refs/heads/trunk" {
		t.Errorf("PrimaryBranch = %q, %q; want trunk, refs/heads/trunk", name, ref)
	}
}

func TestPrimaryBranchFallsBackToRemoteTrackingRef(t *testing.T) {
	r := newTestRepo(t, "main")
	r.checkoutNew("feature")
	r.commit("work")
	r.addOriginClone("main")
	// Remove the local primary branch; only origin/main remains.
	r.git("branch", "--quiet", "-D", "main")
	repo := r.open()

	name, ref, err := repo.PrimaryBranch(context.Background())
	if err != nil {
		t.Fatalf("PrimaryBranch: %v", err)
	}
	if name != "main" || ref != "refs/remotes/origin/main" {
		t.Errorf("PrimaryBranch = %q, %q; want main, refs/remotes/origin/main", name, ref)
	}
}

func TestPrimaryBranchDefaultsToMainThenMaster(t *testing.T) {
	ctx := context.Background()

	r := newTestRepo(t, "main")
	name, ref, err := r.open().PrimaryBranch(ctx)
	if err != nil || name != "main" || ref != "refs/heads/main" {
		t.Errorf("main repo: PrimaryBranch = %q, %q, %v", name, ref, err)
	}

	r = newTestRepo(t, "master")
	name, ref, err = r.open().PrimaryBranch(ctx)
	if err != nil || name != "master" || ref != "refs/heads/master" {
		t.Errorf("master repo: PrimaryBranch = %q, %q, %v", name, ref, err)
	}

	r = newTestRepo(t, "develop")
	_, _, err = r.open().PrimaryBranch(ctx)
	if !errors.Is(err, ErrNoPrimaryBranch) {
		t.Errorf("develop repo: PrimaryBranch error = %v, want ErrNoPrimaryBranch", err)
	}
}
