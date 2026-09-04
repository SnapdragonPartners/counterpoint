package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/counterpoint/internal/appserver"
	"github.com/SnapdragonPartners/counterpoint/internal/appserver/apptest"
	"github.com/SnapdragonPartners/counterpoint/internal/gitrepo"
	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

func TestMain(m *testing.M) {
	if apptest.Main() {
		return
	}
	os.Exit(m.Run())
}

// TestEndToEndAgainstFakeAppServer exercises the acceptance scenario with a
// real app-server client and the fake subprocess: initialization, a
// new-thread review, a restart, thread resume by repository and branch,
// and a second round that references the first.
func TestEndToEndAgainstFakeAppServer(t *testing.T) {
	repo := newTestRepo(t)
	statePath := filepath.Join(t.TempDir(), "counterpoint", "state.json")
	fakeState := filepath.Join(t.TempDir(), "fake-threads")
	checkoutRoot := filepath.Join(t.TempDir(), "checkouts")
	t.Setenv(apptest.ScenarioEnv, "normal")
	t.Setenv(apptest.StateEnv, fakeState)

	spawns := 0
	newService := func() *Service {
		return New(Options{
			Store:  state.NewStore(statePath),
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			NewReviewer: func(ctx context.Context, extraArgs []string) (Reviewer, error) {
				spawns++
				args := append(appserver.DefaultArgs(), extraArgs...)
				return appserver.Start(ctx, appserver.Options{Command: os.Args[0], Args: args, Version: "test", Stderr: io.Discard,
					Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			},
			CheckoutRoot: checkoutRoot,
		})
	}
	req := func(commit, notes string) Request {
		return Request{Repo: repo.dir, Branch: "feature", Commit: commit, BranchNotes: notes}
	}

	// Round one on commit A.
	a := repo.git("rev-parse", "HEAD")
	one, err := newService().Review(context.Background(), req(a, "Round one: implemented the thing."))
	if err != nil {
		t.Fatalf("round one: %v", err)
	}
	if one.Round != 1 || !strings.Contains(one.Review, "REVIEW for thr_1") || !strings.Contains(one.Review, "review round 1 of refs/heads/feature") {
		t.Fatalf("round one result = %+v", one)
	}

	// Restart: a new service and a new app-server process, then round two
	// on descendant commit B using only repository and branch inputs.
	b := repo.commit("feature-2")
	two, err := newService().Review(context.Background(), req(b, "Round two: addressed findings."))
	if err != nil {
		t.Fatalf("round two: %v", err)
	}
	if two.Round != 2 || !strings.Contains(two.Review, "REVIEW for thr_1") || !strings.Contains(two.Review, "review round 2 of") {
		t.Fatalf("round two did not resume the persisted thread: %+v", two)
	}
	if two.Commit != b || two.Base != one.Base {
		t.Errorf("round two target = %s base %s; want %s base %s", two.Commit, two.Base, b, one.Base)
	}

	// Identical request replays without a process.
	replay, err := newService().Review(context.Background(), req(b, "Round two: addressed findings."))
	if err != nil || !replay.Replayed || replay.Review != two.Review {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	if spawns != 2 {
		t.Errorf("app-server spawned %d times, want 2", spawns)
	}

	// The human archived the thread in the Codex app between rounds. Round
	// three unarchives it and continues on the same thread.
	t.Setenv(apptest.ScenarioEnv, "archived")
	c := repo.commit("feature-3")
	three, err := newService().Review(context.Background(), req(c, "Round three: after archive."))
	if err != nil {
		t.Fatalf("round three: %v", err)
	}
	if three.Round != 3 || !strings.Contains(three.Review, "REVIEW for thr_1") {
		t.Fatalf("round three did not continue on the unarchived thread: %+v", three)
	}
	if len(three.Warnings) != 1 || !strings.Contains(three.Warnings[0], "unarchived") {
		t.Errorf("round three warnings = %v, want one about unarchiving", three.Warnings)
	}
	events, _ := os.ReadFile(fakeState + ".events")
	wantName := "name:thr_1:Counterpoint review: " + filepath.Base(repo.dir) + " feature"
	if got := string(events); !strings.Contains(got, "unarchive:thr_1") || !strings.Contains(got, wantName) {
		t.Errorf("fake events lack the unarchive and name requests:\n%s", got)
	}

	// Round four is build-capable: the thread moves to a disposable
	// checkout under workspace-write, the fake sees the temp directory
	// there at turn start, and the checkout is gone afterwards.
	t.Setenv(apptest.ScenarioEnv, "normal")
	d := repo.commit("feature-4")
	four, err := newService().Review(context.Background(), Request{Repo: repo.dir, Branch: "feature", Commit: d, BranchNotes: "Round four: please build.", Build: true})
	if err != nil {
		t.Fatalf("round four: %v", err)
	}
	if four.Round != 4 || !strings.Contains(four.Review, "REVIEW for thr_1") || len(four.Warnings) != 0 {
		t.Fatalf("round four result = %+v", four)
	}
	opened, err := gitrepo.Open(context.Background(), repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(checkoutRoot)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(gitrepo.WorkflowKey(opened.Identity(), "refs/heads/feature")))
	workflowDir := filepath.Join(resolvedRoot, hex.EncodeToString(sum[:])[:16])
	checkoutDir := filepath.Join(workflowDir, "checkout")
	events, _ = os.ReadFile(fakeState + ".events")
	for _, want := range []string{"cwd:" + checkoutDir + ":workspace-write", "checkout-tmp:" + checkoutDir + ":true"} {
		if !strings.Contains(string(events), want) {
			t.Errorf("fake events lack %q:\n%s", want, events)
		}
	}
	if _, err := os.Stat(checkoutDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout %s survived the review: %v", checkoutDir, err)
	}
	if _, err := os.Stat(filepath.Join(workflowDir, "cache")); err != nil {
		t.Errorf("cache dir missing: %v", err)
	}

	// A reviewer that edits a tracked file in the checkout is reported.
	t.Setenv(apptest.ScenarioEnv, "modify-checkout")
	e := repo.commit("feature-5")
	five, err := newService().Review(context.Background(), Request{Repo: repo.dir, Branch: "feature", Commit: e, BranchNotes: "Round five.", Build: true})
	if err != nil {
		t.Fatalf("round five: %v", err)
	}
	if len(five.Warnings) != 1 || !strings.Contains(five.Warnings[0], "changed 1 tracked file") {
		t.Errorf("round five warnings = %v, want the integrity warning", five.Warnings)
	}

	// An effective policy with an unexpected writable root fails closed,
	// and the checkout is still removed.
	t.Setenv(apptest.ScenarioEnv, "workspace-wrong-roots")
	f := repo.commit("feature-6")
	if _, err := newService().Review(context.Background(), Request{Repo: repo.dir, Branch: "feature", Commit: f, BranchNotes: "Round six.", Build: true}); !errors.Is(err, appserver.ErrPolicyMismatch) {
		t.Fatalf("wrong roots error = %v, want ErrPolicyMismatch", err)
	}
	if _, err := os.Stat(checkoutDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout survived a policy mismatch: %v", err)
	}

	// Back to a read-only round on the worktree with the same thread.
	t.Setenv(apptest.ScenarioEnv, "normal")
	six, err := newService().Review(context.Background(), req(f, "Round six, read-only."))
	if err != nil || six.Round != 6 {
		t.Fatalf("read-only round after build rounds = %+v, %v", six, err)
	}
	events, _ = os.ReadFile(fakeState + ".events")
	if !strings.Contains(string(events), "cwd:"+repo.dir+":read-only") {
		t.Errorf("fake events lack the read-only resume on the worktree:\n%s", events)
	}
	if status := repo.git("status", "--porcelain"); status != "" {
		t.Errorf("repository modified by reviews:\n%s", status)
	}
}
