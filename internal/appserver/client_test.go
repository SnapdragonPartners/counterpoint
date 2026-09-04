package appserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/counterpoint/internal/appserver/apptest"
)

func TestMain(m *testing.M) {
	if apptest.Main() {
		return
	}
	os.Exit(m.Run())
}

// fakeClient starts a client against the fake app-server running the given
// scenario. The child inherits the environment, so the scenario and state
// path are set with t.Setenv.
func fakeClient(t *testing.T, scenario string, statePath string) (*Client, *bytes.Buffer) {
	t.Helper()
	t.Setenv(apptest.ScenarioEnv, scenario)
	t.Setenv(apptest.StateEnv, statePath)
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
	th, err := cl.StartThread(context.Background(), "/work/tree", Sandbox{})
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
			th, err := cl.StartThread(context.Background(), "/work/tree", Sandbox{})
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
	resumed, err := cl2.ResumeThread(context.Background(), th.ID, "/other/worktree", Sandbox{})
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

	_, err = cl2.ResumeThread(context.Background(), "thr_nope", "/other/worktree", Sandbox{})
	if err == nil || errors.Is(err, ErrProcessExited) {
		t.Errorf("ResumeThread(unknown) error = %v, want a protocol error", err)
	}
	var rpcErr *ServerError
	if !errors.As(err, &rpcErr) {
		t.Errorf("ResumeThread(unknown) error = %v, want an app-server error", err)
	}
}

func TestUnarchiveThreadMakesAnArchivedThreadResumable(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "threads")
	cl, _ := fakeClient(t, "normal", statePath)
	th := startThread(t, cl)
	cl.Close()

	cl2, _ := fakeClient(t, "archived", statePath)
	_, err := cl2.ResumeThread(context.Background(), th.ID, "/work/tree", Sandbox{})
	var srvErr *ServerError
	if !errors.As(err, &srvErr) {
		t.Fatalf("ResumeThread(archived) error = %v, want a server error", err)
	}
	if err := cl2.UnarchiveThread(context.Background(), th.ID); err != nil {
		t.Fatalf("UnarchiveThread: %v", err)
	}
	if _, err := cl2.ResumeThread(context.Background(), th.ID, "/work/tree", Sandbox{}); err != nil {
		t.Fatalf("ResumeThread after unarchive: %v", err)
	}
	// Not idempotent, as in codex-cli 0.153.1.
	if err := cl2.UnarchiveThread(context.Background(), th.ID); !errors.As(err, &srvErr) {
		t.Errorf("UnarchiveThread(not archived) error = %v, want a server error", err)
	}
	if err := cl2.UnarchiveThread(context.Background(), "thr_nope"); !errors.As(err, &srvErr) {
		t.Errorf("UnarchiveThread(unknown) error = %v, want a server error", err)
	}
	events, _ := os.ReadFile(statePath + ".events")
	if got := strings.Count(string(events), "unarchive:"+th.ID); got != 2 {
		t.Errorf("fake recorded %d unarchive requests for %s, want 2:\n%s", got, th.ID, events)
	}
}

func TestUnarchiveThreadFailsWhileAnotherProcessHoldsTheWriter(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "threads")
	cl, _ := fakeClient(t, "normal", statePath)
	th := startThread(t, cl)
	cl.Close()

	cl2, _ := fakeClient(t, "writer-held", statePath)
	var srvErr *ServerError
	if _, err := cl2.ResumeThread(context.Background(), th.ID, "/work/tree", Sandbox{}); !errors.As(err, &srvErr) {
		t.Fatalf("ResumeThread(held) error = %v, want a server error", err)
	}
	if err := cl2.UnarchiveThread(context.Background(), th.ID); !errors.As(err, &srvErr) {
		t.Fatalf("UnarchiveThread(held) error = %v, want a server error", err)
	}
}

func TestUnarchiveReturningAnotherThreadFailsClosed(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "threads")
	cl, _ := fakeClient(t, "normal", statePath)
	th := startThread(t, cl)
	cl.Close()

	cl2, _ := fakeClient(t, "unarchive-other-id", statePath)
	err := cl2.UnarchiveThread(context.Background(), th.ID)
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("UnarchiveThread error = %v, want ErrIncompatible", err)
	}
}

func TestSetThreadName(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "threads")
	cl, _ := fakeClient(t, "normal", statePath)
	th := startThread(t, cl)
	if err := cl.SetThreadName(context.Background(), th.ID, "Counterpoint review: repo feature"); err != nil {
		t.Fatalf("SetThreadName: %v", err)
	}
	events, _ := os.ReadFile(statePath + ".events")
	if !strings.Contains(string(events), "name:"+th.ID+":Counterpoint review: repo feature") {
		t.Errorf("fake did not record the name:\n%s", events)
	}
	var srvErr *ServerError
	if err := cl.SetThreadName(context.Background(), "thr_nope", "x"); !errors.As(err, &srvErr) {
		t.Errorf("SetThreadName(unknown) error = %v, want a server error", err)
	}

	cl2, _ := fakeClient(t, "name-rejected", "")
	th2 := startThread(t, cl2)
	if err := cl2.SetThreadName(context.Background(), th2.ID, "x"); !errors.As(err, &srvErr) {
		t.Errorf("SetThreadName(rejected) error = %v, want a server error", err)
	}
}

func TestAddWarningSharesTheWarningBudget(t *testing.T) {
	cl, _ := fakeClient(t, "normal", "")
	th := startThread(t, cl)
	cl.AddWarning("setup: first")
	cl.AddWarning(strings.Repeat("x", maxWarningBytes)) // alone it would exceed the byte cap
	for i := 0; i < maxWarnings; i++ {
		cl.AddWarning(fmt.Sprintf("setup: %d", i))
	}
	rev, err := cl.Review(context.Background(), th.ID, "round 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rev.Warnings) != maxWarnings+1 || rev.Warnings[0] != "setup: first" {
		t.Fatalf("warnings = %d entries starting %q, want %d plus the omitted marker", len(rev.Warnings), rev.Warnings[0], maxWarnings)
	}
	if last := rev.Warnings[maxWarnings]; !strings.Contains(last, "2 additional") || !strings.Contains(last, "omitted") {
		t.Errorf("last warning = %q, want a marker for 2 omitted entries", last)
	}
	var total int
	for _, w := range rev.Warnings[:maxWarnings] {
		total += len(w)
	}
	if total > maxWarningBytes {
		t.Errorf("kept warnings total %d bytes, over the %d cap", total, maxWarningBytes)
	}
}

func TestServerRequestsAreDeclinedAndReported(t *testing.T) {
	cl, stderr := fakeClient(t, "approval", "")
	th := startThread(t, cl)
	rev, err := cl.Review(context.Background(), th.ID, "x")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(rev.Warnings) != 6 {
		t.Fatalf("Warnings = %v, want 6 entries", rev.Warnings)
	}
	joined := strings.Join(rev.Warnings, "\n")
	for _, want := range []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput", "execCommandApproval", "item/weird/request", "item c1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	// Command text supplied by the app-server may hold secrets or model
	// output and must not be copied into warnings or logs.
	if strings.Contains(joined, "rm -rf") || strings.Contains(stderr.String(), "rm -rf") {
		t.Errorf("warnings or logs quote the requested command:\n%s", joined)
	}
	if strings.Contains(rev.Text, "declined") {
		t.Error("warnings were spliced into the review text")
	}
}

func TestFailedTurn(t *testing.T) {
	cl, _ := fakeClient(t, "fail", "")
	th := startThread(t, cl)
	_, err := cl.Review(context.Background(), th.ID, "x")
	if !errors.Is(err, ErrTurnFailed) || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "codex error: other") {
		t.Fatalf("Review error = %v, want ErrTurnFailed with boom and the codex error code", err)
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
	if _, err := cl.StartThread(context.Background(), "/work/tree", Sandbox{}); err != nil {
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
	if _, err := cl.StartThread(context.Background(), "/work/tree", Sandbox{}); err != nil {
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
	if _, err := cl.StartThread(context.Background(), "/work/tree", Sandbox{}); !errors.Is(err, ErrProcessExited) {
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
	t.Setenv(apptest.ScenarioEnv, "bad-init")
	t.Setenv(apptest.StateEnv, "")
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

func TestChildExitingRightAfterCompletionLosesNothing(t *testing.T) {
	// The terminal event and the review item are the last bytes the child
	// writes before exiting; they must be read before the process is
	// reaped. The failure mode is a race, so this test is probabilistic:
	// it repeats with a large final burst but cannot prove the ordering.
	// The guarantee itself is by construction in conn.readLoop.
	for i := 0; i < 10; i++ {
		cl, _ := fakeClient(t, "exit-after-complete", "")
		th := startThread(t, cl)
		rev, err := cl.Review(context.Background(), th.ID, "x")
		if err != nil {
			t.Fatalf("iteration %d: Review: %v", i, err)
		}
		if !strings.HasPrefix(rev.Text, "REVIEW for ") {
			t.Fatalf("iteration %d: Text = %q", i, rev.Text)
		}
		cl.Close()
	}
}

func TestWritesAreCancellableWhenChildStopsReading(t *testing.T) {
	cl, _ := fakeClient(t, "deaf", "")
	startThread(t, cl) // after this the child never reads stdin again

	// Enough to fill the pipe so the write itself blocks.
	instructions := strings.Repeat("i", 4<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := cl.Review(ctx, "thr_1", instructions)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Review error = %v, want DeadlineExceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Review blocked on a write the child never consumes")
	}

	// Close must still complete: it releases the blocked writer and kills
	// the child after the grace period.
	start := time.Now()
	cl.Close()
	if elapsed := time.Since(start); elapsed > closeGrace+3*time.Second {
		t.Errorf("Close took %v", elapsed)
	}
}

func TestIncompatibleInitializeIsRejected(t *testing.T) {
	t.Setenv(apptest.ScenarioEnv, "bad-init-shape")
	t.Setenv(apptest.StateEnv, "")
	_, err := Start(context.Background(), Options{Command: os.Args[0], Stderr: &bytes.Buffer{}})
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Start error = %v, want ErrIncompatible", err)
	}
}

func TestEffectivePolicyMismatchFailsClosed(t *testing.T) {
	cl, _ := fakeClient(t, "wrong-policy", "")
	_, err := cl.StartThread(context.Background(), "/work/tree", Sandbox{})
	if !errors.Is(err, ErrPolicyMismatch) {
		t.Fatalf("StartThread error = %v, want ErrPolicyMismatch", err)
	}
}

func TestAggregateOverflowIsAnError(t *testing.T) {
	cl, _ := fakeClient(t, "aggregate-overflow", "")
	th := startThread(t, cl)
	_, err := cl.Review(context.Background(), th.ID, "x")
	if !errors.Is(err, ErrReviewTooLarge) {
		t.Fatalf("Review error = %v, want ErrReviewTooLarge", err)
	}
}

func TestCodexErrorCodeRendering(t *testing.T) {
	cases := map[string]string{
		``:                        "",
		`null`:                    "",
		`"contextWindowExceeded"`: "contextWindowExceeded",
		`{"httpConnectionFailed":{"httpStatusCode":502}}`: "httpConnectionFailed, http 502",
		`{"responseStreamDisconnected":{}}`:               "responseStreamDisconnected",
		`[1,2]`:                                           "unrecognized",
	}
	for raw, want := range cases {
		if got := codexErrorCode([]byte(raw)); got != want {
			t.Errorf("codexErrorCode(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestEffectiveCwdMismatchFailsClosed(t *testing.T) {
	cl, _ := fakeClient(t, "wrong-cwd", "")
	_, err := cl.StartThread(context.Background(), "/work/tree", Sandbox{})
	if !errors.Is(err, ErrPolicyMismatch) {
		t.Fatalf("StartThread error = %v, want ErrPolicyMismatch", err)
	}
}

func TestMissingReviewThreadIDIsRejected(t *testing.T) {
	cl, _ := fakeClient(t, "no-review-thread", "")
	th := startThread(t, cl)
	_, err := cl.Review(context.Background(), th.ID, "x")
	if !errors.Is(err, ErrPolicyMismatch) {
		t.Fatalf("Review error = %v, want ErrPolicyMismatch", err)
	}
}

func TestCancellationBeforeResponseInterruptsKnownTurn(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "threads")
	cl, _ := fakeClient(t, "started-then-stall", statePath)
	th := startThread(t, cl)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := cl.Review(ctx, th.ID, "x")
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrTurnInterrupted) {
		t.Fatalf("Review error = %v, want ErrTurnInterrupted wrapping DeadlineExceeded", err)
	}
	events, _ := os.ReadFile(statePath + ".events")
	if !strings.Contains(string(events), "interrupt:turn_1") {
		t.Errorf("the turn that started before the response was not interrupted; events: %q", events)
	}
}

func TestServerRequestBacklogTerminatesConnection(t *testing.T) {
	cl, _ := fakeClient(t, "flood", "")
	startThread(t, cl) // the child now waits for our next bytes, then floods

	done := make(chan error, 1)
	go func() {
		_, err := cl.Review(context.Background(), "thr_1", strings.Repeat("i", 4<<20))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrWriteBacklog) {
			t.Fatalf("Review error = %v, want ErrWriteBacklog", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Review did not fail although the child stopped reading while asking questions")
	}

	start := time.Now()
	cl.Close()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Close took %v after termination, want prompt", elapsed)
	}
	select {
	case <-cl.c.writerDone:
	default:
		t.Error("writer still running after Close")
	}
}

func TestWriterExitsOnCloseAndOnChildExit(t *testing.T) {
	cl, _ := fakeClient(t, "normal", "")
	startThread(t, cl)
	cl.Close()
	select {
	case <-cl.c.writerDone:
	default:
		t.Error("writer still running after Close of an idle connection")
	}

	cl2, _ := fakeClient(t, "exit", "")
	th := startThread(t, cl2)
	_, _ = cl2.Review(context.Background(), th.ID, "x")
	select {
	case <-cl2.c.writerDone:
	case <-time.After(2 * time.Second):
		t.Error("writer still running after the child exited")
	}
	if _, err := cl2.StartThread(context.Background(), "/work/tree", Sandbox{}); !errors.Is(err, ErrProcessExited) {
		t.Errorf("send after child exit error = %v, want ErrProcessExited", err)
	}
}

func TestResumeReturningAnotherThreadFailsClosed(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "threads")
	cl, _ := fakeClient(t, "resume-other-id", statePath)
	th := startThread(t, cl)
	_, err := cl.ResumeThread(context.Background(), th.ID, "/work/tree", Sandbox{})
	if !errors.Is(err, ErrPolicyMismatch) {
		t.Fatalf("ResumeThread error = %v, want ErrPolicyMismatch", err)
	}
}

func TestAwaitTurnPrefersFinishedTurn(t *testing.T) {
	// Both channels already closed: a plain select would choose at random,
	// so repeat enough times that a random choice cannot pass.
	done := make(chan struct{})
	closed := make(chan struct{})
	close(done)
	close(closed)
	for i := 0; i < 2000; i++ {
		if got := awaitTurn(done, closed, nil); got != turnFinished {
			t.Fatalf("iteration %d: awaitTurn = %v, want turnFinished", i, got)
		}
		if got := awaitTurn(done, nil, closed); got != turnFinished {
			t.Fatalf("iteration %d: awaitTurn with cancellation = %v, want turnFinished", i, got)
		}
	}
	open := make(chan struct{})
	if got := awaitTurn(open, closed, nil); got != connectionClosed {
		t.Errorf("awaitTurn(open, closed) = %v, want connectionClosed", got)
	}
	if got := awaitTurn(open, nil, closed); got != callerCancelled {
		t.Errorf("awaitTurn(open, cancelled) = %v, want callerCancelled", got)
	}
}

func TestPendingCallReportsTypedTransportError(t *testing.T) {
	cl, _ := fakeClient(t, "normal", "")
	startThread(t, cl)

	result := make(chan error, 1)
	go func() { result <- cl.c.call(context.Background(), "test/ping", nil, nil) }()
	// Let the call become pending, then terminate with a specific reason.
	time.Sleep(100 * time.Millisecond)
	cl.c.terminate(ErrWriteBacklog)

	select {
	case err := <-result:
		if !errors.Is(err, ErrWriteBacklog) {
			t.Fatalf("pending call error = %v, want ErrWriteBacklog", err)
		}
		var rpcErr *ServerError
		if errors.As(err, &rpcErr) {
			t.Errorf("pending call error %v is a synthetic RPC error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call did not return after termination")
	}
}

func TestStaleAndUnidentifiedEventsAreNotAttributed(t *testing.T) {
	cl, _ := fakeClient(t, "stale-events", "")
	th := startThread(t, cl)
	rev, err := cl.Review(context.Background(), th.ID, "x")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !strings.HasPrefix(rev.Text, "REVIEW for ") || strings.Contains(rev.Text, "STALE") || strings.Contains(rev.Text, "EMPTY ID") {
		t.Errorf("Text = %q, want the real review only", rev.Text)
	}
}

func TestEarlyTurnIDDisagreementIsAnError(t *testing.T) {
	cl, _ := fakeClient(t, "started-id-disagrees", "")
	th := startThread(t, cl)
	_, err := cl.Review(context.Background(), th.ID, "x")
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Review error = %v, want ErrIncompatible", err)
	}
}

func TestWarningsAreBounded(t *testing.T) {
	cl, _ := fakeClient(t, "warning-flood", "")
	th := startThread(t, cl)
	rev, err := cl.Review(context.Background(), th.ID, "x")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(rev.Warnings) == 0 || len(rev.Warnings) > maxWarnings+1 {
		t.Fatalf("got %d warnings, want at most %d plus one marker", len(rev.Warnings), maxWarnings)
	}
	total := 0
	for _, w := range rev.Warnings[:len(rev.Warnings)-1] {
		total += len(w)
		if len(w) > 2*maxIdentifierLen+128 {
			t.Errorf("warning not truncated: %d bytes", len(w))
		}
	}
	if total > maxWarningBytes {
		t.Errorf("warnings total %d bytes, want at most %d", total, maxWarningBytes)
	}
	last := rev.Warnings[len(rev.Warnings)-1]
	if !strings.Contains(last, "additional app-server warnings omitted") {
		t.Errorf("last warning %q is not the omission marker", last)
	}
	// The bound resets between reviews.
	if rev2, err := cl.Review(context.Background(), th.ID, "y"); err != nil || len(rev2.Warnings) > maxWarnings+1 {
		t.Errorf("second review: %v, %d warnings", err, len(rev2.Warnings))
	}
}

func TestStalledInitializeIsCancellable(t *testing.T) {
	t.Setenv(apptest.ScenarioEnv, "stall-init")
	t.Setenv(apptest.StateEnv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Start(ctx, Options{Command: os.Args[0], Stderr: &bytes.Buffer{}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > closeGrace+3*time.Second {
		t.Errorf("Start took %v to give up and reap the child", elapsed)
	}
}

func TestCheckSandboxRequiresTheExactRequestedPolicy(t *testing.T) {
	build := Sandbox{Build: true, WritableRoots: []string{"/cache"}}
	good := sandboxPolicy{Type: "workspaceWrite", WritableRoots: []string{"/cache"}, ExcludeSlashTmp: true, ExcludeTmpdirEnvVar: true}
	cases := []struct {
		name string
		got  sandboxPolicy
		want Sandbox
		ok   bool
	}{
		{"read-only as requested", sandboxPolicy{Type: "readOnly"}, Sandbox{}, true},
		{"workspace when read-only requested", good, Sandbox{}, false},
		{"read-only when build requested", sandboxPolicy{Type: "readOnly"}, build, false},
		{"exact build policy", good, build, true},
		{"network on", sandboxPolicy{Type: "workspaceWrite", NetworkAccess: true, WritableRoots: []string{"/cache"}, ExcludeSlashTmp: true, ExcludeTmpdirEnvVar: true}, build, false},
		{"extra root", sandboxPolicy{Type: "workspaceWrite", WritableRoots: []string{"/cache", "/extra"}, ExcludeSlashTmp: true, ExcludeTmpdirEnvVar: true}, build, false},
		{"missing root", sandboxPolicy{Type: "workspaceWrite", ExcludeSlashTmp: true, ExcludeTmpdirEnvVar: true}, build, false},
		{"duplicate root", sandboxPolicy{Type: "workspaceWrite", WritableRoots: []string{"/cache", "/cache"}, ExcludeSlashTmp: true, ExcludeTmpdirEnvVar: true}, Sandbox{Build: true, WritableRoots: []string{"/cache", "/other"}}, false},
		{"/tmp kept writable", sandboxPolicy{Type: "workspaceWrite", WritableRoots: []string{"/cache"}, ExcludeTmpdirEnvVar: true}, build, false},
		{"$TMPDIR kept writable", sandboxPolicy{Type: "workspaceWrite", WritableRoots: []string{"/cache"}, ExcludeSlashTmp: true}, build, false},
	}
	for _, tc := range cases {
		err := checkSandbox(tc.got, tc.want)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, ErrPolicyMismatch) {
			t.Errorf("%s: error = %v, want ErrPolicyMismatch", tc.name, err)
		}
	}
}

func TestBuildConfigArgsQuoteTOMLStrings(t *testing.T) {
	args := BuildConfigArgs(`/c/"quoted"\dir`, "/work/.counterpoint-tmp")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		`sandbox_workspace_write.writable_roots=["/c/\"quoted\"\\dir"]`,
		"sandbox_workspace_write.exclude_slash_tmp=true",
		"sandbox_workspace_write.exclude_tmpdir_env_var=true",
		`shell_environment_policy.set.TMPDIR="/work/.counterpoint-tmp"`,
		`shell_environment_policy.set.COUNTERPOINT_CACHE_DIR="/c/\"quoted\"\\dir"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q lack %q", joined, want)
		}
	}
	if n := strings.Count(joined, "-c "); n != 5 {
		t.Errorf("%d -c flags, want 5", n)
	}
}

func TestAppendWarningHonorsTheBounds(t *testing.T) {
	var list []string
	for i := 0; i < maxWarnings; i++ {
		var ok bool
		if list, ok = AppendWarning(list, "w"); !ok {
			t.Fatalf("entry %d refused", i)
		}
	}
	if _, ok := AppendWarning(list, "one too many"); ok {
		t.Error("entry beyond the count cap accepted")
	}
	if _, ok := AppendWarning([]string{"x"}, strings.Repeat("y", maxWarningBytes)); ok {
		t.Error("entry beyond the byte cap accepted")
	}
	if got, ok := AppendWarning([]string{"a"}, "b"); !ok || len(got) != 2 {
		t.Errorf("AppendWarning([a], b) = %v, %v", got, ok)
	}
}

// fakeClientArgs is fakeClient with configuration overrides on the child's
// command line, which the fake parses and echoes as its effective policy.
func fakeClientArgs(t *testing.T, scenario string, extraArgs []string) *Client {
	t.Helper()
	t.Setenv(apptest.ScenarioEnv, scenario)
	t.Setenv(apptest.StateEnv, "")
	cl, err := Start(context.Background(), Options{
		Command: os.Args[0], Args: append(DefaultArgs(), extraArgs...), Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Start(%s): %v", scenario, err)
	}
	t.Cleanup(cl.Close)
	return cl
}

func TestWorkspaceWriteSessionIsValidatedAgainstTheEchoedPolicy(t *testing.T) {
	build := Sandbox{Build: true, WritableRoots: []string{"/cache/dir"}}
	args := BuildConfigArgs("/cache/dir", "/work/tree/.counterpoint-tmp")

	cl := fakeClientArgs(t, "normal", args)
	th, err := cl.StartThread(context.Background(), "/work/tree", build)
	if err != nil {
		t.Fatalf("StartThread(build): %v", err)
	}
	if _, err := cl.ResumeThread(context.Background(), th.ID, "/work/tree", build); err != nil {
		t.Fatalf("ResumeThread(build): %v", err)
	}
	// The same thread may go back to read-only on the worktree.
	if _, err := cl.ResumeThread(context.Background(), th.ID, "/user/worktree", Sandbox{}); err != nil {
		t.Fatalf("ResumeThread(read-only): %v", err)
	}

	// Without the overrides the server reports no roots and writable temp
	// roots; the exact check must refuse that.
	plain := fakeClientArgs(t, "normal", nil)
	if _, err := plain.StartThread(context.Background(), "/work/tree", build); !errors.Is(err, ErrPolicyMismatch) {
		t.Errorf("StartThread without overrides error = %v, want ErrPolicyMismatch", err)
	}

	wrong := fakeClientArgs(t, "workspace-wrong-roots", args)
	if _, err := wrong.StartThread(context.Background(), "/work/tree", build); !errors.Is(err, ErrPolicyMismatch) {
		t.Errorf("StartThread with an extra root error = %v, want ErrPolicyMismatch", err)
	}
}
