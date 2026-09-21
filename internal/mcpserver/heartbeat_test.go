package mcpserver

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/counterpoint/internal/appserver"
	"github.com/SnapdragonPartners/counterpoint/internal/appserver/apptest"
	"github.com/SnapdragonPartners/counterpoint/internal/review"
	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

func TestMain(m *testing.M) {
	if apptest.Main() {
		return
	}
	os.Exit(m.Run())
}

// testHeartbeat is the heartbeat cadence under test: short enough that a
// blocked review sees many, long enough that counting them is meaningful.
const testHeartbeat = 20 * time.Millisecond

// sleepingClock is a wall clock a test moves by hand, as a machine sleep
// moves the real one while the monotonic clock stands still. Its readings
// carry no monotonic component, which is what the watcher subtracts.
type sleepingClock struct {
	base   int64 // wall nanoseconds at construction
	offset atomic.Int64
}

func newSleepingClock() *sleepingClock { return &sleepingClock{base: time.Now().UnixNano()} }

func (c *sleepingClock) now() time.Time { return time.Unix(0, c.base+c.offset.Load()) }

// sleep advances the wall clock by d without touching anything monotonic.
func (c *sleepingClock) sleep(d time.Duration) { c.offset.Add(int64(d)) }

// progressLog collects the progress notifications a client receives.
type progressLog struct {
	mu   sync.Mutex
	seen []*mcp.ProgressNotificationParams
}

func (p *progressLog) handler(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, req.Params)
}

func (p *progressLog) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.seen)
}

// awaitCount waits until at least n notifications have arrived.
func (p *progressLog) awaitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for p.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("saw %d progress notifications, want at least %d", p.count(), n)
		}
		time.Sleep(testHeartbeat / 4)
	}
}

// fixture is a server under test wired to a client that records progress
// notifications over the in-memory transport, with the heartbeat cadence
// and the wall clock injected.
type fixture struct {
	session  *mcp.ClientSession
	progress *progressLog
	clock    *sleepingClock
	logs     *syncBuffer
}

func newFixture() *fixture {
	return &fixture{progress: &progressLog{}, clock: newSleepingClock(), logs: &syncBuffer{}}
}

// build is the server under test for svc under lifecycle, with the
// fixture's cadence, clock, and log.
func (f *fixture) build(lifecycle context.Context, svc *review.Service) *mcp.Server {
	log := slog.New(slog.NewTextHandler(f.logs, nil))
	return newServer(lifecycle, svc, "test", log, testHeartbeat, f.clock.now)
}

// wire connects server and a recording client over the in-memory
// transport. The sessions are not bound to the lifecycle, as in production.
func (f *fixture) wire(t *testing.T, server *mcp.Server) {
	t.Helper()
	serverT, clientT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	f.session, err = mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: f.progress.handler,
	}).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.session.Close() })
}

// callReview starts a review call in the background and returns its
// outcome channel. token, when non-empty, is sent as the progress token.
//
// The call's context is cancelled on cleanup, and because cleanups run
// last-registered-first that cancellation happens before wire's session
// closes. Without it a test that fails while a call is in flight waits
// out the call's remaining budget during a graceful session close, which
// turned a ten-second assertion failure into a thirty-second one and made
// the real defect harder to see.
func (f *fixture) callReview(t *testing.T, ctx context.Context, dir, token string) <-chan callOutcome { //nolint:revive // t first would fight the ctx convention here
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	out := make(chan callOutcome, 1)
	params := &mcp.CallToolParams{Name: ToolName, Arguments: map[string]any{
		"repo": dir, "branch": "feature", "commit": "HEAD", "branch_notes": "notes",
	}}
	if token != "" {
		params.Meta = mcp.Meta{"progressToken": token}
	}
	go func() {
		res, err := f.session.CallTool(ctx, params)
		out <- callOutcome{res: res, err: err}
	}()
	return out
}

type callOutcome struct {
	res *mcp.CallToolResult
	err error
}

// await returns the call's outcome or fails the test after ten seconds.
func await(t *testing.T, outcome <-chan callOutcome) callOutcome {
	t.Helper()
	select {
	case res := <-outcome:
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("call did not return")
		return callOutcome{}
	}
}

// toolError returns the text of a tool error result.
func toolError(t *testing.T, res callOutcome) string {
	t.Helper()
	if res.err != nil || res.res == nil || !res.res.IsError || len(res.res.Content) == 0 {
		t.Fatalf("outcome = %+v, want a tool error", res)
	}
	text, _ := res.res.Content[0].(*mcp.TextContent)
	if text == nil {
		t.Fatalf("tool error content = %v", res.res.Content)
	}
	return text.Text
}

// slowCloseReviewer blocks its review until the context ends and then
// takes several heartbeat intervals to close, so the heartbeat through
// cleanup is observable.
type slowCloseReviewer struct {
	blockingReviewer
	closeDelay time.Duration
}

func (s *slowCloseReviewer) Close() {
	time.Sleep(s.closeDelay)
	s.blockingReviewer.Close()
}

// releasableReviewer blocks its review until released or its context ends.
type releasableReviewer struct {
	blockingReviewer
	release chan struct{}
}

func (r *releasableReviewer) Review(ctx context.Context, _, _ string) (*appserver.Review, error) {
	close(r.started)
	select {
	case <-r.release:
		return &appserver.Review{TurnID: "t", Text: "APPROVED"}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", appserver.ErrTurnInterrupted, ctx.Err())
	}
}

func newService(t *testing.T, rev review.Reviewer) *review.Service {
	t.Helper()
	return review.New(review.Options{
		Store:       state.NewStore(filepath.Join(t.TempDir(), "state.json")),
		NewReviewer: func(context.Context, []string) (review.Reviewer, error) { return rev, nil },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// A call that carries a progress token receives a heartbeat every
// interval for as long as it runs, including cleanup after the review
// ends, and none after the result is returned.
func TestHeartbeatsForTheLifeOfTheCall(t *testing.T) {
	dir := gitRepo(t)
	rev := &slowCloseReviewer{
		blockingReviewer: blockingReviewer{started: make(chan struct{}), closed: make(chan struct{})},
		closeDelay:       10 * testHeartbeat,
	}
	lifecycle, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	f := newFixture()
	f.wire(t, f.build(lifecycle, newService(t, rev)))

	outcome := f.callReview(t, context.Background(), dir, "hb-1")
	<-rev.started
	f.progress.awaitCount(t, 3)

	// Ending the review starts cleanup, which the slow Close stretches
	// over several intervals; heartbeats must continue through it.
	atShutdown := f.progress.count()
	shutdown()
	text := toolError(t, await(t, outcome))
	if strings.Contains(text, "silent") {
		t.Errorf("tool error blames silence for a lifecycle shutdown: %s", text)
	}
	atResult := f.progress.count()
	if atResult < atShutdown+3 {
		t.Errorf("%d heartbeats before shutdown, %d at the result; want several more during the %v cleanup", atShutdown, atResult, rev.closeDelay)
	}

	// Nothing after the result.
	time.Sleep(5 * testHeartbeat)
	if n := f.progress.count(); n != atResult {
		t.Errorf("%d heartbeats after the result was returned", n-atResult)
	}

	f.progress.mu.Lock()
	defer f.progress.mu.Unlock()
	for i, p := range f.progress.seen {
		if p.ProgressToken != "hb-1" {
			t.Errorf("heartbeat %d token = %v, want hb-1", i, p.ProgressToken)
		}
		if p.Progress != float64(i+1) {
			t.Errorf("heartbeat %d progress = %v, want %d", i, p.Progress, i+1)
		}
		if p.Total != 0 || !strings.HasPrefix(p.Message, "review in progress for ") {
			t.Errorf("heartbeat %d = %+v; want no total and the status message", i, p)
		}
	}
}

// Without a progress token there is nothing to attach progress to: no
// heartbeat is sent, the log says why, and the call is bounded to
// MaxSilence of wall-clock time instead.
func TestNoTokenMeansNoHeartbeatAndABoundedCall(t *testing.T) {
	dir := gitRepo(t)
	rev := &blockingReviewer{started: make(chan struct{}), closed: make(chan struct{})}
	f := newFixture()
	f.wire(t, f.build(context.Background(), newService(t, rev)))

	outcome := f.callReview(t, context.Background(), dir, "")
	<-rev.started
	time.Sleep(5 * testHeartbeat)
	if n := f.progress.count(); n != 0 {
		t.Errorf("%d heartbeats sent for a call without a progress token", n)
	}
	if !strings.Contains(f.logs.String(), "progress heartbeat disabled") {
		t.Errorf("log does not say heartbeats are disabled:\n%s", f.logs.String())
	}

	// Without heartbeats silence accumulates: two twenty-minute sleeps
	// with awake ticks between them carry the call past the bound, though
	// neither alone would.
	f.clock.sleep(20 * time.Minute)
	time.Sleep(5 * testHeartbeat)
	select {
	case res := <-outcome:
		t.Fatalf("call ended after a twenty-minute sleep: %+v", res)
	default:
	}
	f.clock.sleep(20 * time.Minute)
	text := toolError(t, await(t, outcome))
	if !strings.Contains(text, ErrSilence.Error()) || !strings.Contains(text, "no progress token") {
		t.Errorf("tool error = %q; want the silence error naming the missing token", text)
	}
	select {
	case <-rev.closed:
	default:
		t.Error("reviewer not closed after the call was ended")
	}
}

// A sleep that carries the silence to the bound ends a blocked turn
// promptly on wake: the reviewer is closed and the tool error names the
// silence, while a shorter sleep ends nothing.
func TestSilenceGuardEndsABlockedTurn(t *testing.T) {
	dir := gitRepo(t)
	rev := &releasableReviewer{
		blockingReviewer: blockingReviewer{started: make(chan struct{}), closed: make(chan struct{})},
		release:          make(chan struct{}),
	}
	f := newFixture()
	f.wire(t, f.build(context.Background(), newService(t, rev)))

	outcome := f.callReview(t, context.Background(), dir, "hb-2")
	<-rev.started
	f.progress.awaitCount(t, 2)
	// With heartbeats each sleep is measured from the last heartbeat, so
	// two twenty-minute sleeps with heartbeats between them are both
	// survived, while without heartbeats their sum would end the call.
	for i := 0; i < 2; i++ {
		f.clock.sleep(20 * time.Minute)
		n := f.progress.count()
		f.progress.awaitCount(t, n+3)
		select {
		case res := <-outcome:
			t.Fatalf("call ended after twenty-minute sleep %d: %+v", i+1, res)
		default:
		}
	}

	f.clock.sleep(time.Hour) // not survived
	text := toolError(t, await(t, outcome))
	if !strings.Contains(text, ErrSilence.Error()) || !strings.Contains(text, "the machine slept") || !strings.Contains(text, "review turn interrupted") {
		t.Errorf("tool error = %q; want the silence error, the sleep, and the interrupted turn", text)
	}
	select {
	case <-rev.closed:
	default:
		t.Error("reviewer not closed after the call was ended")
	}
	if !strings.Contains(f.logs.String(), "silent toward the client") {
		t.Errorf("log does not record the ended call:\n%s", f.logs.String())
	}
}

// The guard covers phases outside the setup and turn budgets: a sleep
// during the final save, held off by a state lock the test holds, ends the
// call the same way instead of waiting out the lock.
func TestSilenceGuardEndsAnUnbudgetedPhase(t *testing.T) {
	dir := gitRepo(t)
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	rev := &blockingReviewer{started: make(chan struct{}), closed: make(chan struct{})}
	// The reviewer takes the state lock as it finishes, so the final save
	// waits on it; the lock is released only when the test ends.
	finished := &finishingReviewer{blockingReviewer: rev, lockPath: store.LockPath()}
	t.Cleanup(finished.releaseLock)
	svc := review.New(review.Options{
		Store:       store,
		NewReviewer: func(context.Context, []string) (review.Reviewer, error) { return finished, nil },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	f := newFixture()
	f.wire(t, f.build(context.Background(), svc))

	outcome := f.callReview(t, context.Background(), dir, "hb-3")
	<-rev.started
	time.Sleep(5 * testHeartbeat) // the save is waiting on the lock by now
	start := time.Now()
	f.clock.sleep(time.Hour)
	text := toolError(t, await(t, outcome))
	if !strings.Contains(text, ErrSilence.Error()) || !strings.Contains(text, "state was not saved") {
		t.Errorf("tool error = %q; want the silence error and the refused save", text)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("call took %v to end after the sleep; the save's lock wait was not cut short", elapsed)
	}
}

// finishingReviewer completes its review at once, taking the state lock
// on the way out so the final save has to wait for it.
type finishingReviewer struct {
	*blockingReviewer
	lockPath string
	mu       sync.Mutex
	held     *state.Lock
}

func (r *finishingReviewer) Review(ctx context.Context, _, _ string) (*appserver.Review, error) {
	held, err := state.AcquireLock(ctx, r.lockPath, time.Second)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.held = held
	r.mu.Unlock()
	close(r.started)
	return &appserver.Review{TurnID: "t", Text: "APPROVED"}, nil
}

func (r *finishingReviewer) releaseLock() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held != nil {
		_ = r.held.Release()
	}
}

// A client that cancels the call sends notifications/cancelled, which ends
// the review's context: the reviewer is interrupted and closed, and the
// call returns an error rather than running on.
func TestClientCancellationEndsTheReview(t *testing.T) {
	dir := gitRepo(t)
	rev := &blockingReviewer{started: make(chan struct{}), closed: make(chan struct{})}
	f := newFixture()
	f.wire(t, f.build(context.Background(), newService(t, rev)))

	ctx, cancel := context.WithCancel(context.Background())
	outcome := f.callReview(t, ctx, dir, "hb-4")
	<-rev.started
	cancel()
	if res := await(t, outcome); !errors.Is(res.err, context.Canceled) {
		t.Errorf("CallTool error = %v, want context.Canceled", res.err)
	}
	select {
	case <-rev.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("reviewer not closed after the client cancelled")
	}
}

// Through the real app-server client and the fake subprocess: heartbeats
// flow while the turn is stalled, and the client's cancellation reaches
// the child as turn/interrupt.
func TestHeartbeatsThroughAStalledFakeTurn(t *testing.T) {
	dir := gitRepo(t)
	fakeState := filepath.Join(t.TempDir(), "fake-threads")
	t.Setenv(apptest.ScenarioEnv, "hang")
	t.Setenv(apptest.StateEnv, fakeState)
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	svc := review.New(review.Options{
		Store: state.NewStore(filepath.Join(t.TempDir(), "state.json")),
		NewReviewer: func(ctx context.Context, extraArgs []string) (review.Reviewer, error) {
			args := append(appserver.DefaultArgs(), extraArgs...)
			return appserver.Start(ctx, appserver.Options{Command: os.Args[0], Args: args, Version: "test", Stderr: &logs, Logger: log})
		},
		Logger: log,
	})
	f := newFixture()
	f.wire(t, f.build(context.Background(), svc))

	ctx, cancel := context.WithCancel(context.Background())
	outcome := f.callReview(t, ctx, dir, "hb-5")
	// The fake records the instructions when the turn starts; from then
	// on it stalls until interrupted, and heartbeats must keep coming.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(fakeState + ".instructions"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake never received review/start")
		}
		time.Sleep(testHeartbeat)
	}
	inTurn := f.progress.count()
	f.progress.awaitCount(t, inTurn+5)
	cancel()
	if res := await(t, outcome); !errors.Is(res.err, context.Canceled) {
		t.Errorf("CallTool error = %v, want context.Canceled", res.err)
	}
	// The client's call returns as soon as its context ends, while the
	// server is still interrupting the turn and reaping the child.
	deadline = time.Now().Add(30 * time.Second)
	for {
		events, _ := os.ReadFile(fakeState + ".events")
		if strings.Contains(string(events), "interrupt:") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake never received turn/interrupt: events %q\nlogs:\n%s", events, logs.String())
		}
		time.Sleep(testHeartbeat)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// jumpingClock jumps forward by d at its nth reading and stays there. It
// pins where a machine sleep lands relative to the watcher's clock reads,
// which a clock advanced from another goroutine cannot do.
type jumpingClock struct {
	base     int64
	readings atomic.Int64
	at       int64
	d        time.Duration
}

func (c *jumpingClock) now() time.Time {
	if c.readings.Add(1) >= c.at {
		return time.Unix(0, c.base+int64(c.d))
	}
	return time.Unix(0, c.base)
}

// A sleep that lands while a heartbeat is in flight must still end the
// call. watchCall reads the clock once at start, then twice per interval:
// before the heartbeat and after it. A sleep on the third reading is one
// that happened during the first heartbeat, so the pre-send check saw
// nothing; if the post-send reading simply became the new baseline, the
// hour would be erased and no later interval could ever see it.
func TestSilenceGuardCatchesASleepDuringAHeartbeat(t *testing.T) {
	dir := gitRepo(t)
	rev := &releasableReviewer{
		blockingReviewer: blockingReviewer{started: make(chan struct{}), closed: make(chan struct{})},
		release:          make(chan struct{}),
	}
	// Released whatever the outcome, so a failed assertion cannot leave
	// the reviewer blocked and the cleanup waiting on it.
	defer close(rev.release)

	clock := &jumpingClock{base: time.Now().UnixNano(), at: 3, d: time.Hour}
	f := newFixture()
	log := slog.New(slog.NewTextHandler(f.logs, nil))
	f.wire(t, newServer(context.Background(), newService(t, rev), "test", log, testHeartbeat, clock.now))

	// The reviewer is not awaited: an hour passes within the first
	// interval, and the guard covers every phase, so the call may be
	// ended before the review is ever started.
	outcome := f.callReview(t, context.Background(), dir, "hb-jump")

	select {
	case res := <-outcome:
		text := toolError(t, res)
		if !strings.Contains(text, ErrSilence.Error()) || !strings.Contains(text, "the machine slept") {
			t.Errorf("tool error = %q; want the silence error and the sleep", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("an hour passed during a heartbeat and the call never ended (%d clock readings)", clock.readings.Load())
	}
	if !strings.Contains(f.logs.String(), "silent toward the client") {
		t.Errorf("log does not record the ended call:\n%s", f.logs.String())
	}
}
