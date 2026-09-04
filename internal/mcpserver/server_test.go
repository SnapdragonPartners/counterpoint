package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/counterpoint/internal/appserver"
	"github.com/SnapdragonPartners/counterpoint/internal/review"
	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

type stubReviewer struct{}

func (stubReviewer) StartThread(context.Context, string) (appserver.Thread, error) {
	return appserver.Thread{ID: "thr_1"}, nil
}

func (stubReviewer) ResumeThread(_ context.Context, id, _ string) (appserver.Thread, error) {
	return appserver.Thread{ID: id}, nil
}

func (stubReviewer) Review(_ context.Context, _, instructions string) (*appserver.Review, error) {
	first, _, _ := strings.Cut(instructions, "\n")
	return &appserver.Review{TurnID: "t", Text: "APPROVED: " + first}, nil
}

func (stubReviewer) Close() {}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = cleanGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	run("config", "commit.gpgsign", "false")
	run("commit", "--quiet", "--allow-empty", "-m", "initial")
	run("checkout", "--quiet", "-b", "feature")
	run("commit", "--quiet", "--allow-empty", "-m", "work")
	return dir
}

func TestReviewToolOverInMemoryTransport(t *testing.T) {
	dir := gitRepo(t)
	svc := review.New(review.Options{
		Store:       state.NewStore(filepath.Join(t.TempDir(), "state.json")),
		NewReviewer: func(context.Context) (review.Reviewer, error) { return stubReviewer{}, nil },
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	server := New(svc, "test", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != ToolName {
		t.Fatalf("ListTools = %+v, %v; want exactly the review tool", tools, err)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: ToolName, Arguments: map[string]any{
		"repo": dir, "branch": "feature", "commit": "HEAD", "branch_notes": "notes",
	}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out Output
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Round != 1 || out.Branch != "refs/heads/feature" || !strings.HasPrefix(out.Review, "APPROVED: Counterpoint review round 1") || out.Warnings == nil || out.Repo != dir {
		t.Errorf("output = %+v", out)
	}

	// A validation failure is a tool error with operational context, not a
	// protocol error.
	bad, err := session.CallTool(ctx, &mcp.CallToolParams{Name: ToolName, Arguments: map[string]any{
		"repo": dir, "branch": "main", "commit": "HEAD", "branch_notes": "notes",
	}})
	if err != nil {
		t.Fatalf("CallTool(primary branch): protocol error %v, want tool error", err)
	}
	if !bad.IsError {
		t.Fatal("reviewing the primary branch succeeded")
	}
	text, _ := bad.Content[0].(*mcp.TextContent)
	if text == nil || !strings.Contains(text.Text, "primary branch") {
		t.Errorf("tool error content = %v", bad.Content)
	}
}

// cleanGitEnv drops Git redirect variables that a hook or a parent test
// process may export; setting them to empty is not the same as unsetting.
func cleanGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") || strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}
