package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultCommand is the executable that provides the app-server.
	DefaultCommand = "codex"
	// clientName identifies Counterpoint in the initialize handshake.
	clientName = "counterpoint"

	// interruptGrace is how long Review waits for the interrupted terminal
	// event after sending turn/interrupt.
	interruptGrace = 5 * time.Second

	// maxCollectedText bounds the review text accumulated from deltas and
	// messages for one turn.
	maxCollectedText = MaxMessageSize
)

// DefaultArgs are the arguments that start the app-server on stdio.
func DefaultArgs() []string {
	return []string{"app-server"}
}

// Sentinel errors for review outcomes.
var (
	ErrTurnFailed      = errors.New("review turn failed")
	ErrTurnInterrupted = errors.New("review turn interrupted")
	ErrNoReviewText    = errors.New("review turn completed without review text")
)

// Options configures Start.
type Options struct {
	// Command is the executable; DefaultCommand when empty.
	Command string
	// Args are the full arguments; DefaultArgs when nil. Configuration
	// overrides such as -c model_reasoning_effort=... belong here.
	Args []string
	// Version is reported in the initialize handshake.
	Version string
	// Stderr receives the child's diagnostics; discarded when nil. It is
	// never the protocol stream.
	Stderr io.Writer
	// Logger receives client diagnostics; slog.Default when nil.
	Logger *slog.Logger
}

// Client is a live app-server session.
type Client struct {
	c   *conn
	log *slog.Logger

	closeOnce sync.Once
}

// Thread describes a started or resumed thread.
type Thread struct {
	ID              string
	Model           string
	ReasoningEffort string
}

// Review is a completed review turn.
type Review struct {
	TurnID string
	// Text is the review: the exited-review-mode item when present,
	// otherwise the completed agent messages, otherwise streamed deltas.
	Text string
	// Warnings lists declined server requests observed during the turn.
	Warnings []string
}

// Start launches the app-server and completes the initialize handshake.
func Start(ctx context.Context, opts Options) (*Client, error) {
	command := opts.Command
	if command == "" {
		command = DefaultCommand
	}
	args := opts.Args
	if args == nil {
		args = DefaultArgs()
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	c, err := spawn(ctx, command, args, stderr, log)
	if err != nil {
		return nil, err
	}
	cl := &Client{c: c, log: log}

	params := initializeParams{ClientInfo: clientInfo{Name: clientName, Version: opts.Version}}
	if err := c.call(ctx, methodInitialize, params, nil); err != nil {
		cl.Close()
		return nil, fmt.Errorf("app-server handshake: %w", err)
	}
	if err := c.notify(methodInitialized, nil); err != nil {
		cl.Close()
		return nil, fmt.Errorf("app-server handshake: %w", err)
	}
	return cl, nil
}

// StartThread creates a new thread rooted at cwd with the read-only
// sandbox and the never approval policy.
func (cl *Client) StartThread(ctx context.Context, cwd string) (Thread, error) {
	var resp threadResponse
	params := threadStartParams{Cwd: cwd, Sandbox: sandboxReadOnly, ApprovalPolicy: approvalNever}
	if err := cl.c.call(ctx, methodThreadStart, params, &resp); err != nil {
		return Thread{}, err
	}
	return threadFromResponse(resp)
}

// ResumeThread resumes threadID at cwd with the same sandbox and approval
// settings. A thread the app-server cannot find is an error; the caller
// fails closed rather than starting a replacement.
func (cl *Client) ResumeThread(ctx context.Context, threadID, cwd string) (Thread, error) {
	var resp threadResponse
	params := threadResumeParams{ThreadID: threadID, Cwd: cwd, Sandbox: sandboxReadOnly, ApprovalPolicy: approvalNever}
	if err := cl.c.call(ctx, methodThreadResume, params, &resp); err != nil {
		return Thread{}, err
	}
	return threadFromResponse(resp)
}

func threadFromResponse(resp threadResponse) (Thread, error) {
	if resp.Thread.ID == "" {
		return Thread{}, errors.New("app-server returned a thread without an id")
	}
	t := Thread{ID: resp.Thread.ID, Model: resp.Model}
	if resp.ReasoningEffort != nil {
		t.ReasoningEffort = *resp.ReasoningEffort
	}
	return t, nil
}

// Review runs an inline custom review on threadID and blocks until the
// turn reaches a terminal state. If ctx ends first, the turn is
// interrupted and the returned error wraps both ErrTurnInterrupted and the
// context error.
func (cl *Client) Review(ctx context.Context, threadID, instructions string) (*Review, error) {
	w := newTurnWatcher(threadID)
	unsubscribe := cl.c.subscribe(w.handle)
	defer unsubscribe()

	var resp reviewStartResponse
	params := reviewStartParams{
		ThreadID: threadID,
		Target:   reviewTarget{Type: reviewTargetCustom, Instructions: instructions},
		Delivery: reviewDeliveryInline,
	}
	if err := cl.c.call(ctx, methodReviewStart, params, &resp); err != nil {
		return nil, err
	}
	if resp.Turn.ID == "" {
		return nil, errors.New("review/start returned no turn id")
	}
	if resp.ReviewThreadID != "" && resp.ReviewThreadID != threadID {
		return nil, fmt.Errorf("review/start ran on thread %s, expected %s", resp.ReviewThreadID, threadID)
	}
	w.setTurn(resp.Turn.ID)

	select {
	case <-w.done:
	case <-cl.c.closed:
		return nil, fmt.Errorf("review: %w", cl.c.closedErr())
	case <-ctx.Done():
		cl.interrupt(ctx, threadID, resp.Turn.ID, w)
		return nil, fmt.Errorf("%w: %w", ErrTurnInterrupted, ctx.Err())
	}

	final := w.result()
	warnings := cl.c.takeWarnings()
	switch final.status {
	case turnStatusCompleted:
		text := final.text()
		if text == "" {
			return nil, ErrNoReviewText
		}
		return &Review{TurnID: resp.Turn.ID, Text: text, Warnings: warnings}, nil
	case turnStatusFailed:
		return nil, fmt.Errorf("%w: %s", ErrTurnFailed, final.errorMessage())
	case turnStatusInterrupted:
		return nil, fmt.Errorf("%w by app-server", ErrTurnInterrupted)
	default:
		return nil, fmt.Errorf("%w: unexpected terminal status %q", ErrTurnFailed, final.status)
	}
}

// interrupt asks the app-server to stop the turn and waits briefly for the
// terminal event. The caller's context is already done, so the request runs
// on a detached copy with its own bounded timeout.
func (cl *Client) interrupt(parent context.Context, threadID, turnID string, w *turnWatcher) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), interruptGrace)
	defer cancel()
	if err := cl.c.call(ctx, methodTurnInterrupt, turnInterruptParams{ThreadID: threadID, TurnID: turnID}, nil); err != nil {
		cl.log.Warn("app-server: interrupt failed", "error", err)
		return
	}
	select {
	case <-w.done:
	case <-cl.c.closed:
	case <-ctx.Done():
		cl.log.Warn("app-server: no terminal event after interrupt", "turn", turnID)
	}
}

// Close ends the app-server process, killing it if it does not exit
// promptly. It is safe to call more than once.
func (cl *Client) Close() {
	cl.closeOnce.Do(cl.c.close)
}

// turnWatcher accumulates one turn's notifications on the reader goroutine.
type turnWatcher struct {
	mu       sync.Mutex
	threadID string
	turnID   string
	review   string
	messages strings.Builder
	deltas   strings.Builder
	overflow bool
	status   string
	turnErr  *turnError
	lastErr  *turnError
	done     chan struct{}
	finished bool
}

func newTurnWatcher(threadID string) *turnWatcher {
	return &turnWatcher{threadID: threadID, done: make(chan struct{})}
}

func (w *turnWatcher) setTurn(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.turnID == "" {
		w.turnID = id
	}
}

// matches reports whether an event belongs to this watcher's turn. Before
// the turn id is known, every event on the thread counts, since only one
// turn runs on a thread at a time.
func (w *turnWatcher) matches(threadID, turnID string) bool {
	return threadID == w.threadID && (w.turnID == "" || turnID == "" || turnID == w.turnID)
}

func (w *turnWatcher) handle(method string, params json.RawMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return
	}
	switch method {
	case notifyTurnStarted:
		var n turnNotification
		if unmarshal(params, &n) && n.ThreadID == w.threadID && w.turnID == "" {
			w.turnID = n.Turn.ID
		}
	case notifyAgentMessageDelta:
		var n agentMessageDelta
		if unmarshal(params, &n) && w.matches(n.ThreadID, n.TurnID) {
			w.append(&w.deltas, n.Delta)
		}
	case notifyItemCompleted:
		var n itemNotification
		if !unmarshal(params, &n) || !w.matches(n.ThreadID, n.TurnID) {
			return
		}
		switch n.Item.Type {
		case itemTypeExitedReviewMode:
			w.review = n.Item.Review
		case itemTypeAgentMessage:
			if w.messages.Len() > 0 {
				w.append(&w.messages, "\n\n")
			}
			w.append(&w.messages, n.Item.Text)
		}
	case notifyError:
		var n errorNotification
		if unmarshal(params, &n) && w.matches(n.ThreadID, n.TurnID) && !n.WillRetry {
			e := n.Error
			w.lastErr = &e
		}
	case notifyTurnCompleted:
		var n turnNotification
		if !unmarshal(params, &n) || !w.matches(n.ThreadID, n.Turn.ID) {
			return
		}
		w.status = n.Turn.Status
		w.turnErr = n.Turn.Error
		if w.review == "" {
			for _, item := range n.Turn.Items {
				if item.Type == itemTypeExitedReviewMode && item.Review != "" {
					w.review = item.Review
				}
			}
		}
		w.finished = true
		close(w.done)
	}
}

func (w *turnWatcher) append(b *strings.Builder, s string) {
	if w.overflow {
		return
	}
	if b.Len()+len(s) > maxCollectedText {
		w.overflow = true
		return
	}
	b.WriteString(s)
}

// turnResult is a snapshot taken after the turn finished.
type turnResult struct {
	status   string
	review   string
	messages string
	deltas   string
	turnErr  *turnError
	lastErr  *turnError
}

func (w *turnWatcher) result() turnResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	return turnResult{
		status:   w.status,
		review:   w.review,
		messages: w.messages.String(),
		deltas:   w.deltas.String(),
		turnErr:  w.turnErr,
		lastErr:  w.lastErr,
	}
}

func (r turnResult) text() string {
	switch {
	case r.review != "":
		return r.review
	case r.messages != "":
		return r.messages
	default:
		return r.deltas
	}
}

func (r turnResult) errorMessage() string {
	switch {
	case r.turnErr != nil && r.turnErr.Message != "":
		return r.turnErr.Message
	case r.lastErr != nil && r.lastErr.Message != "":
		return r.lastErr.Message
	default:
		return "no error details"
	}
}

func unmarshal(data []byte, v any) bool {
	return json.Unmarshal(data, v) == nil
}
