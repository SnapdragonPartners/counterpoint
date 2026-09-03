package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// featureRepo returns a repository on branch "feature" with one commit past
// main, plus the main tip and the feature tip.
func featureRepo(t *testing.T) (r *testRepo, mainTip, featureTip string) {
	t.Helper()
	r = newTestRepo(t, "main")
	mainTip = r.head()
	r.checkoutNew("feature")
	featureTip = r.commit("feature-1")
	return r, mainTip, featureTip
}

func TestResolveCommit(t *testing.T) {
	r, _, tip := featureRepo(t)
	repo := r.open()
	ctx := context.Background()

	for _, in := range []string{tip, tip[:7], "HEAD", "feature"} {
		got, err := repo.ResolveCommit(ctx, in)
		if err != nil || got != tip {
			t.Errorf("ResolveCommit(%q) = %q, %v; want %s", in, got, err, tip)
		}
	}

	r.git("tag", "-a", "-m", "tagged", "v1")
	got, err := repo.ResolveCommit(ctx, "v1")
	if err != nil || got != tip {
		t.Errorf("ResolveCommit(annotated tag) = %q, %v; want %s", got, err, tip)
	}

	for _, in := range []string{"", "-x", "nope", "0123456789abcdef0123456789abcdef01234567", "HEAD^{tree}"} {
		_, err := repo.ResolveCommit(ctx, in)
		if !errors.Is(err, ErrCommitNotFound) {
			t.Errorf("ResolveCommit(%q) error = %v, want ErrCommitNotFound", in, err)
		}
	}
}

func TestValidateTargetHappyPath(t *testing.T) {
	r, mainTip, tip := featureRepo(t)
	repo := r.open()

	target, err := repo.ValidateTarget(context.Background(), "feature", tip[:8], "")
	if err != nil {
		t.Fatalf("ValidateTarget: %v", err)
	}
	if target.Commit != tip || target.Branch.Ref != "refs/heads/feature" {
		t.Errorf("target = %+v, want commit %s on refs/heads/feature", target, tip)
	}
	if target.Base != mainTip {
		t.Errorf("Base = %s, want main tip %s", target.Base, mainTip)
	}
	if target.PrimaryName != "main" || target.PrimaryRef != "refs/heads/main" {
		t.Errorf("primary = %q %q, want main refs/heads/main", target.PrimaryName, target.PrimaryRef)
	}
	if target.HistoryRewritten {
		t.Error("HistoryRewritten = true without a previous tip")
	}
}

func TestValidateTargetRefusesPrimaryBranch(t *testing.T) {
	r := newTestRepo(t, "main")
	repo := r.open()

	_, err := repo.ValidateTarget(context.Background(), "main", "HEAD", "")
	if !errors.Is(err, ErrPrimaryBranch) {
		t.Fatalf("ValidateTarget(main) error = %v, want ErrPrimaryBranch", err)
	}
}

func TestValidateTargetRejectsNonTipCommit(t *testing.T) {
	r, _, first := featureRepo(t)
	r.commit("feature-2")
	repo := r.open()

	_, err := repo.ValidateTarget(context.Background(), "feature", first, "")
	if !errors.Is(err, ErrNotBranchTip) {
		t.Fatalf("ValidateTarget(old commit) error = %v, want ErrNotBranchTip", err)
	}
}

func TestValidateTargetRejectsWhenHeadElsewhere(t *testing.T) {
	r, _, tip := featureRepo(t)
	r.checkout("main")
	repo := r.open()

	_, err := repo.ValidateTarget(context.Background(), "feature", tip, "")
	if !errors.Is(err, ErrNotHead) {
		t.Fatalf("ValidateTarget(HEAD on main) error = %v, want ErrNotHead", err)
	}
}

func TestValidateTargetRejectsDirtyWorktree(t *testing.T) {
	cases := map[string]func(r *testRepo){
		"modified tracked": func(r *testRepo) { r.write("feature-1.txt", "changed\n") },
		"staged":           func(r *testRepo) { r.write("feature-1.txt", "changed\n"); r.git("add", "-A") },
		"untracked":        func(r *testRepo) { r.write("new.txt", "x\n") },
		"untracked in dir": func(r *testRepo) { r.write("newdir/new.txt", "x\n") },
		"deleted tracked": func(r *testRepo) {
			if err := os.Remove(filepath.Join(r.dir, "feature-1.txt")); err != nil {
				r.t.Fatal(err)
			}
		},
	}
	for name, dirty := range cases {
		t.Run(name, func(t *testing.T) {
			r, _, tip := featureRepo(t)
			dirty(r)
			_, err := r.open().ValidateTarget(context.Background(), "feature", tip, "")
			if !errors.Is(err, ErrDirtyWorktree) {
				t.Fatalf("error = %v, want ErrDirtyWorktree", err)
			}
		})
	}
}

func TestValidateTargetIgnoresIgnoredFiles(t *testing.T) {
	r := newTestRepo(t, "main")
	r.write(".gitignore", "*.log\nbuild/\n")
	r.git("add", "-A")
	r.git("commit", "--quiet", "-m", "ignore")
	r.checkoutNew("feature")
	tip := r.commit("feature-1")
	r.write("debug.log", "x\n")
	r.write("build/out.bin", "x\n")

	if _, err := r.open().ValidateTarget(context.Background(), "feature", tip, ""); err != nil {
		t.Fatalf("ValidateTarget with only ignored files: %v", err)
	}
}

func TestValidateTargetRejectsOrphanBranch(t *testing.T) {
	r := newTestRepo(t, "main")
	r.git("checkout", "--quiet", "--orphan", "orphan")
	r.git("rm", "-rfq", ".")
	tip := r.commit("orphan-1")

	_, err := r.open().ValidateTarget(context.Background(), "orphan", tip, "")
	if !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("ValidateTarget(orphan) error = %v, want ErrNoMergeBase", err)
	}
}

func TestValidateTargetUsesRemoteTrackingPrimaryWhenLocalMissing(t *testing.T) {
	r, mainTip, _ := featureRepo(t)
	r.addOriginClone("main")
	r.git("branch", "--quiet", "-D", "main")
	tip := r.commit("feature-2")

	target, err := r.open().ValidateTarget(context.Background(), "feature", tip, "")
	if err != nil {
		t.Fatalf("ValidateTarget: %v", err)
	}
	if target.PrimaryRef != "refs/remotes/origin/main" || target.Base != mainTip {
		t.Errorf("primary ref %q base %s; want refs/remotes/origin/main and %s", target.PrimaryRef, target.Base, mainTip)
	}
}

func TestValidateTargetPreviousTipAncestor(t *testing.T) {
	r, _, first := featureRepo(t)
	second := r.commit("feature-2")

	target, err := r.open().ValidateTarget(context.Background(), "feature", second, first)
	if err != nil {
		t.Fatalf("ValidateTarget: %v", err)
	}
	if target.HistoryRewritten {
		t.Error("HistoryRewritten = true for a descendant commit")
	}
}

func TestValidateTargetDetectsRewrittenHistory(t *testing.T) {
	r, _, first := featureRepo(t)
	// Amend the tip so the previously reviewed commit is no longer an ancestor.
	r.write("feature-1.txt", "amended\n")
	r.git("add", "-A")
	r.git("commit", "--quiet", "--amend", "--no-edit")
	amended := r.head()
	if amended == first {
		t.Fatal("amend did not change the tip")
	}

	target, err := r.open().ValidateTarget(context.Background(), "feature", amended, first)
	if err != nil {
		t.Fatalf("ValidateTarget: %v", err)
	}
	if !target.HistoryRewritten {
		t.Error("HistoryRewritten = false after amend")
	}
}

func TestValidateTargetMissingPreviousTipCountsAsRewritten(t *testing.T) {
	r, _, tip := featureRepo(t)

	target, err := r.open().ValidateTarget(context.Background(), "feature", tip, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("ValidateTarget: %v", err)
	}
	if !target.HistoryRewritten {
		t.Error("HistoryRewritten = false for an unknown previous tip")
	}
}

func TestValidateTargetHonorsCancelledContext(t *testing.T) {
	r, _, tip := featureRepo(t)
	repo := r.open()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := repo.ValidateTarget(ctx, "feature", tip, ""); err == nil {
		t.Fatal("ValidateTarget with cancelled context: want error, got nil")
	}
}
