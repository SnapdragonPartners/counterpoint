package review

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/counterpoint/internal/appserver"
	"github.com/SnapdragonPartners/counterpoint/internal/appserver/apptest"
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
	t.Setenv(apptest.ScenarioEnv, "normal")
	t.Setenv(apptest.StateEnv, fakeState)

	spawns := 0
	newService := func() *Service {
		return New(Options{
			Store:  state.NewStore(statePath),
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			NewReviewer: func(ctx context.Context) (Reviewer, error) {
				spawns++
				return appserver.Start(ctx, appserver.Options{Command: os.Args[0], Version: "test", Stderr: io.Discard,
					Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			},
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
	if status := repo.git("status", "--porcelain"); status != "" {
		t.Errorf("repository modified by reviews:\n%s", status)
	}
}
