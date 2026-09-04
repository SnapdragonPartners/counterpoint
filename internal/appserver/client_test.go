package appserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClient starts a client against the fake app-server running the given
// scenario. The child inherits the environment, so the scenario and state
// path are set with t.Setenv.
func fakeClient(t *testing.T, scenario string, statePath string) (*Client, *bytes.Buffer) {
	t.Helper()
	t.Setenv(fakeEnv, scenario)
	t.Setenv(fakeStateEnv, statePath)
	var stderr syncBuffer
	cl, err := Start(context.Background(), Options{
		Command: os.Args[0],
		Version: "test",
		Stderr:  &stderr,
		Logger:  slog.New(slog.NewTextHandler(&stderr, nil)),
	})
	if err != nil {
		t.Fatalf("Start(%s): %v", scenario, err)
	}
	t.Cleanup(cl.Close)
	return cl, &stderr.buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func startThread(t *testing.T, cl *Client) Thread {
	t.Helper()
	th, err := cl.StartThread(context.Background(), "/work/tree")
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	return th
}

func TestHandshakeAndStartThread(t *testing.T) {
	cl, _ := fakeClient(t, "normal", "")
	th := startThread(t, cl)
	if th.ID == "" || th.Model != "fake-model" || th.ReasoningEffort != "xhigh" {
		t.Errorf("thread = %+v, want id, fake-model, xhigh", th)
	}
}

func TestReviewReturnsReviewItemAndKeepsStderrSeparate(t *testing.T) {
	cl, stderr := fakeClient(t, "normal", "")
	th := startThread(t, cl)

	instructions := "Round 1 review\nBase abc..def"
	rev, err := cl.Review(context.Background(), th.ID, instructions)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	want := fmt.Sprintf("REVIEW for %s: %d instruction bytes; first line: Round 1 review", th.ID, len(instructions))
	if rev.Text != want {
		t.Errorf("Text = %q, want %q", rev.Text, want)
	}
	if rev.TurnID == "" {
		t.Error("TurnID empty")
	}
	if len(rev.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", rev.Warnings)
	}
	if strings.Contains(rev.Text, "NOISE") {
		t.Error("review contains another thread's output")
	}
	cl.Close()
	if !strings.Contains(stderr.String(), "fake app-server starting") {
		t.Error("child stderr was not captured separately")
	}
}

func TestReviewHandlesNotificationsBeforeResponse(t *testing.T) {
	cl, _ := fakeClient(t, "notify-first", "")
	th := startThread(t, cl)
	rev, err := cl.Review(context.Background(), th.ID, "x")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !strings.HasPrefix(rev.Text, "REVIEW for "+th.ID) {
		t.Errorf("Text = %q", rev.Text)
	}
}

func TestConcurrentCallsAreDispatchedByID(t *testing.T) {
	cl, _ := fakeClient(t, "reorder", "")
	var wg sync.WaitGroup
	ids := make([]string, 2)
	errs := make([]error, 2)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			th, err := cl.StartThread(context.Background(), "/work/tree")
			ids[i], errs[i] = th.ID, err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("StartThread %d: %v", i, err)
		}
	}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Errorf("thread ids = %v, want two distinct ids", ids)
	}
}

func TestResumeAcrossProcessRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "threads")
	cl, _ := fakeClient(t, "normal", statePath)
	th := startThread(t, cl)
	cl.Close()

	cl2, _ := fakeClient(t, "normal", statePath)
	resumed, err := cl2.ResumeThread(context.Background(), th.ID, "/other/worktree")
	if err != nil {
		t.Fatalf("ResumeThread after restart: %v", err)
	}
	if resumed.ID != th.ID {
		t.Errorf("resumed id = %q, want %q", resumed.ID, th.ID)
	}
	rev, err := cl2.Review(context.Background(), th.ID, "round 2")
	if err != nil || !strings.Contains(rev.Text, th.ID) {
		t.Errorf("Review on resumed thread = %v, %v", rev, err)
	}

	_, err = cl2.ResumeThread(context.Background(), "thr_nope", "/other/worktree")
	if err == nil || errors.Is(err, ErrProcessExited) {
		t.Errorf("ResumeThread(unknown) error = %v, want a protocol error", err)
	}
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		t.Errorf("ResumeThread(unknown) error = %v, want an app-server error", err)
	}
}

func TestServerRequestsAreDeclinedAndReported(t *testing.T) {
	cl, _ := fakeClient(t, "approval", "")
	th := startThread(t, cl)
	rev, err := cl.Review(context.Background(), th.ID, "x")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(rev.Warnings) != 6 {
		t.Fatalf("Warnings = %v, want 6 entries", rev.Warnings)
	}
	joined := strings.Join(rev.Warnings, "\n")
	for _, want := range []string{requestCommandApproval, "rm -rf /", requestFileChangeApproval, requestPermissions, requestUserInput, requestLegacyExecApproval, "item/weird/request"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(rev.Text, "declined") {
		t.Error("warnings were spliced into the review text")
	}
}

func TestFailedTurn(t *testing.T) {
	cl, _ := fakeClient(t, "fail", "")
	th := startThread(t, cl)
	_, err := cl.Review(context.Background(), th.ID, "x")
	if !errors.Is(err, ErrTurnFailed) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Review error = %v, want ErrTurnFailed with boom", err)
	}
}

func TestContextCancellationInterruptsTurn(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "threads")
	cl, _ := fakeClient(t, "hang", statePath)
	th := startThread(t, cl)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := cl.Review(ctx, th.ID, "x")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTurnInterrupted) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Review error = %v, want ErrTurnInterrupted wrapping DeadlineExceeded", err)
	}
	if elapsed > interruptGrace {
		t.Errorf("interrupt took %v; the fake completes promptly once interrupted", elapsed)
	}
	events, _ := os.ReadFile(statePath + ".events")
	if !strings.Contains(string(events), "interrupt:") {
		t.Errorf("the fake never received turn/interrupt; events: %q", events)
	}

	// The connection is still usable afterwards.
	if _, err := cl.StartThread(context.Background(), "/work/tree"); err != nil {
		t.Errorf("StartThread after interrupt: %v", err)
	}
}

func TestLargeMessagesInBothDirections(t *testing.T) {
	cl, _ := fakeClient(t, "large", "")
	th := startThread(t, cl)
	instructions := strings.Repeat("i", 100*1024)
	rev, err := cl.Review(context.Background(), th.ID, instructions)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(rev.Text) != 200*1024 {
		t.Errorf("review text length = %d, want %d", len(rev.Text), 200*1024)
	}
}

func TestOversizedIncomingMessageFailsClearly(t *testing.T) {
	cl, _ := fakeClient(t, "huge", "")
	th := startThread(t, cl)
	_, err := cl.Review(context.Background(), th.ID, "x")
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Review error = %v, want ErrMessageTooLarge", err)
	}
}

func TestOversizedOutgoingMessageIsRefused(t *testing.T) {
	cl, _ := fakeClient(t, "normal", "")
	th := startThread(t, cl)
	_, err := cl.Review(context.Background(), th.ID, strings.Repeat("x", MaxMessageSize))
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Review error = %v, want ErrMessageTooLarge", err)
	}
	if _, err := cl.StartThread(context.Background(), "/work/tree"); err != nil {
		t.Errorf("connection unusable after refused write: %v", err)
	}
}

func TestProcessExitMidReview(t *testing.T) {
	cl, _ := fakeClient(t, "exit", "")
	th := startThread(t, cl)
	_, err := cl.Review(context.Background(), th.ID, "x")
	if !errors.Is(err, ErrProcessExited) {
		t.Fatalf("Review error = %v, want ErrProcessExited", err)
	}
	if _, err := cl.StartThread(context.Background(), "/work/tree"); !errors.Is(err, ErrProcessExited) {
		t.Errorf("StartThread after exit error = %v, want ErrProcessExited", err)
	}
}

func TestReviewTextFallbacks(t *testing.T) {
	cases := map[string]string{
		"no-review-item":     "first\n\nsecond",
		"delta-only":         "only deltas",
		"items-in-completed": "from completed items",
	}
	for scenario, want := range cases {
		t.Run(scenario, func(t *testing.T) {
			cl, _ := fakeClient(t, scenario, "")
			th := startThread(t, cl)
			rev, err := cl.Review(context.Background(), th.ID, "x")
			if err != nil {
				t.Fatalf("Review: %v", err)
			}
			if rev.Text != want {
				t.Errorf("Text = %q, want %q", rev.Text, want)
			}
		})
	}
}

func TestEmptyReviewIsAnError(t *testing.T) {
	cl, _ := fakeClient(t, "empty", "")
	th := startThread(t, cl)
	if _, err := cl.Review(context.Background(), th.ID, "x"); !errors.Is(err, ErrNoReviewText) {
		t.Fatalf("Review error = %v, want ErrNoReviewText", err)
	}
}

func TestReviewOnUnexpectedThreadIsRejected(t *testing.T) {
	cl, _ := fakeClient(t, "wrong-thread", "")
	th := startThread(t, cl)
	if _, err := cl.Review(context.Background(), th.ID, "x"); err == nil {
		t.Fatal("Review on a detached thread: want error")
	}
}

func TestHandshakeFailureReapsChild(t *testing.T) {
	t.Setenv(fakeEnv, "bad-init")
	t.Setenv(fakeStateEnv, "")
	_, err := Start(context.Background(), Options{Command: os.Args[0], Stderr: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "initialize rejected") {
		t.Fatalf("Start error = %v, want the app-server's rejection", err)
	}
}

func TestMissingExecutable(t *testing.T) {
	_, err := Start(context.Background(), Options{Command: filepath.Join(t.TempDir(), "no-such-codex")})
	if err == nil {
		t.Fatal("Start with a missing executable: want error")
	}
}

func TestCloseKillsLingeringChild(t *testing.T) {
	cl, _ := fakeClient(t, "linger", "")
	startThread(t, cl)
	start := time.Now()
	cl.Close()
	if elapsed := time.Since(start); elapsed > closeGrace+2*time.Second {
		t.Errorf("Close took %v, want about closeGrace then kill", elapsed)
	}
	cl.Close() // second call is a no-op
}
