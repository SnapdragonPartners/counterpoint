package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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

func (stubReviewer) UnarchiveThread(context.Context, string) error { return nil }

func (stubReviewer) SetThreadName(context.Context, string, string) error { return nil }

func (stubReviewer) AddWarning(string) {}

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
	server := New(context.Background(), svc, "test", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

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

// blockingReviewer holds the review until its context ends and records
// whether Close ran.
type blockingReviewer struct {
	started chan struct{}
	closed  chan struct{}
}

func (b *blockingReviewer) StartThread(context.Context, string) (appserver.Thread, error) {
	return appserver.Thread{ID: "thr_1"}, nil
}

func (b *blockingReviewer) ResumeThread(_ context.Context, id, _ string) (appserver.Thread, error) {
	return appserver.Thread{ID: id}, nil
}

func (b *blockingReviewer) UnarchiveThread(context.Context, string) error { return nil }

func (b *blockingReviewer) SetThreadName(context.Context, string, string) error { return nil }

func (b *blockingReviewer) AddWarning(string) {}

func (b *blockingReviewer) Review(ctx context.Context, _, _ string) (*appserver.Review, error) {
	close(b.started)
	<-ctx.Done()
	return nil, fmt.Errorf("%w: %w", appserver.ErrTurnInterrupted, ctx.Err())
}

func (b *blockingReviewer) Close() { close(b.closed) }

func TestLifecycleCancellationInterruptsActiveReview(t *testing.T) {
	dir := gitRepo(t)
	rev := &blockingReviewer{started: make(chan struct{}), closed: make(chan struct{})}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	svc := review.New(review.Options{
		Store:       state.NewStore(filepath.Join(t.TempDir(), "state.json")),
		NewReviewer: func(context.Context) (review.Reviewer, error) { return rev, nil },
		Logger:      log,
	})
	lifecycle, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	server := New(lifecycle, svc, "test", log)

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: ToolName, Arguments: map[string]any{
			"repo": dir, "branch": "feature", "commit": "HEAD", "branch_notes": "notes",
		}})
		if err != nil {
			t.Errorf("CallTool: %v", err)
		}
		result <- res
	}()

	<-rev.started
	shutdown() // SIGTERM equivalent
	select {
	case res := <-result:
		if !res.IsError {
			t.Fatal("review succeeded despite shutdown")
		}
		select {
		case <-rev.closed:
		default:
			t.Error("reviewer not closed after shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active review did not stop on lifecycle cancellation")
	}

	// The same request id appears on receipt and on the outcome log.
	ids := regexp.MustCompile(`request=([0-9a-f]{12})`).FindAllStringSubmatch(logs.String(), -1)
	if len(ids) < 2 || ids[0][1] != ids[len(ids)-1][1] {
		t.Errorf("request id not propagated through logs:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "status=failed") {
		t.Errorf("terminal status missing from logs:\n%s", logs.String())
	}
}

func TestBoundedLineReader(t *testing.T) {
	limit := 32
	ok := `{"a":1}` + "\n" + "\n" + `{"b":"` + strings.Repeat("x", 20) + `"}` + "\n"
	r := newBoundedLineReader(io.NopCloser(strings.NewReader(ok)), limit)
	data, err := io.ReadAll(r)
	if err != nil || string(data) != `{"a":1}`+"\n"+`{"b":"`+strings.Repeat("x", 20)+`"}`+"\n" {
		t.Fatalf("framed lines: %q, %v", data, err)
	}

	over := `{"a":1}` + "\n" + `{"c":"` + strings.Repeat("y", limit) + `"}` + "\n"
	r = newBoundedLineReader(io.NopCloser(strings.NewReader(over)), limit)
	if _, err := io.ReadAll(r); !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("oversized line error = %v, want ErrRequestTooLarge", err)
	}
	if _, err := r.Read(make([]byte, 8)); !errors.Is(err, ErrRequestTooLarge) {
		t.Errorf("reader recovered after an oversized line: %v", err)
	}

	// One JSON value spread over many short lines must not pass, however
	// short each physical line is.
	multi := "{\n" + `"a":` + "\n" + "1\n" + "}\n"
	r = newBoundedLineReader(io.NopCloser(strings.NewReader(multi)), limit)
	if _, err := io.ReadAll(r); !errors.Is(err, ErrRequestFraming) {
		t.Fatalf("multiline value error = %v, want ErrRequestFraming", err)
	}

	unterminated := `{"a":1}`
	r = newBoundedLineReader(io.NopCloser(strings.NewReader(unterminated)), limit)
	if _, err := io.ReadAll(r); !errors.Is(err, ErrRequestFraming) {
		t.Fatalf("unterminated line error = %v, want ErrRequestFraming", err)
	}
}

func TestMultilineRequestEndsSession(t *testing.T) {
	svc := review.New(review.Options{Store: state.NewStore(filepath.Join(t.TempDir(), "state.json")),
		NewReviewer: func(context.Context) (review.Reviewer, error) { return stubReviewer{}, nil },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil))})
	server := New(context.Background(), svc, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), server, inR, outW) }()
	go func() { _, _ = io.Copy(io.Discard, outR) }()

	// Many short lines forming one value: a per-line bound alone would
	// let this grow without limit.
	go func() {
		_, _ = io.WriteString(inW, "{\n\"jsonrpc\": \"2.0\",\n\"id\": 1,\n\"method\": \"tools/list\"\n}\n")
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRequestFraming) && (err == nil || !strings.Contains(err.Error(), "complete JSON value")) {
			t.Fatalf("Serve returned %v, want the framing error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end on a multiline request")
	}
}

func TestOversizedRequestEndsSession(t *testing.T) {
	svc := review.New(review.Options{Store: state.NewStore(filepath.Join(t.TempDir(), "state.json")),
		NewReviewer: func(context.Context) (review.Reviewer, error) { return stubReviewer{}, nil },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil))})
	server := New(context.Background(), svc, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), server, inR, outW) }()
	go func() { _, _ = io.Copy(io.Discard, outR) }()

	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"review","arguments":{"branch_notes":"` + strings.Repeat("x", MaxRequestBytes+1) + `"}}}` + "\n"
	go func() { _, _ = io.WriteString(inW, huge) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("Serve returned %v, want the size-limit error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end on an oversized request")
	}
}
