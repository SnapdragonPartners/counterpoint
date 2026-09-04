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
	ErrReviewTooLarge  = errors.New("review text exceeds the size limit")
	ErrIncompatible    = errors.New("app-server response is incompatible with the expected protocol")
	ErrPolicyMismatch  = errors.New("app-server did not apply the read-only, no-approval policy")
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
	var resp initializeResponse
	if err := c.call(ctx, methodInitialize, params, &resp); err != nil {
		cl.Close()
		return nil, fmt.Errorf("app-server handshake: %w", err)
	}
	if resp.CodexHome == "" || resp.PlatformFamily == "" || resp.PlatformOS == "" || resp.UserAgent == "" {
		cl.Close()
		return nil, fmt.Errorf("app-server handshake: %w: initialize response lacks required fields", ErrIncompatible)
	}
	log.Info("app-server ready", "userAgent", resp.UserAgent, "platform", resp.PlatformOS)
	if err := c.notify(ctx, methodInitialized, nil); err != nil {
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
	return threadFromResponse(resp, cwd)
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
	if resp.Thread.ID != threadID {
		return Thread{}, fmt.Errorf("%w: resume returned thread %q, requested %q", ErrPolicyMismatch, resp.Thread.ID, threadID)
	}
	return threadFromResponse(resp, cwd)
}

// threadFromResponse validates what the server reports back and fails
// closed unless the effective policy is the read-only sandbox without
// network access with the never approval policy, and the effective working
// directory is exactly the canonical path that was requested.
func threadFromResponse(resp threadResponse, requestedCwd string) (Thread, error) {
	if resp.Thread.ID == "" {
		return Thread{}, fmt.Errorf("%w: thread response has no id", ErrIncompatible)
	}
	if resp.Cwd != requestedCwd {
		return Thread{}, fmt.Errorf("%w: effective cwd is %q, requested %q", ErrPolicyMismatch, resp.Cwd, requestedCwd)
	}
	var approval string
	if err := json.Unmarshal(resp.ApprovalPolicy, &approval); err != nil || approval != approvalNever {
		return Thread{}, fmt.Errorf("%w: effective approval policy is %s", ErrPolicyMismatch, string(resp.ApprovalPolicy))
	}
	if resp.Sandbox.Type != sandboxPolicyReadOnly || resp.Sandbox.NetworkAccess {
		return Thread{}, fmt.Errorf("%w: effective sandbox is %q with networkAccess=%v", ErrPolicyMismatch, resp.Sandbox.Type, resp.Sandbox.NetworkAccess)
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
		// The turn may already be running if turn/started arrived before
		// the response; on cancellation, stop it rather than leave it.
		if ctx.Err() != nil {
			if known := w.knownTurn(); known != "" {
				cl.interrupt(ctx, threadID, known, w)
				return nil, fmt.Errorf("%w: %w", ErrTurnInterrupted, err)
			}
		}
		return nil, err
	}
	if resp.Turn.ID == "" {
		return nil, fmt.Errorf("%w: review/start returned no turn id", ErrIncompatible)
	}
	if resp.ReviewThreadID != threadID {
		return nil, fmt.Errorf("%w: review/start ran on thread %q, expected the persistent thread %q", ErrPolicyMismatch, resp.ReviewThreadID, threadID)
	}
	if !w.setTurn(resp.Turn.ID) {
		return nil, fmt.Errorf("%w: turn/started reported turn %q but review/start returned %q", ErrIncompatible, w.knownTurn(), resp.Turn.ID)
	}

	switch awaitTurn(w.done, cl.c.closed, ctx.Done()) {
	case turnFinished:
	case connectionClosed:
		return nil, fmt.Errorf("review: %w", cl.c.closedErr())
	case callerCancelled:
		cl.interrupt(ctx, threadID, resp.Turn.ID, w)
		return nil, fmt.Errorf("%w: %w", ErrTurnInterrupted, ctx.Err())
	}

	final := w.result()
	warnings := cl.c.takeWarnings()
	switch final.status {
	case turnStatusCompleted:
		text, err := final.text()
		if err != nil {
			return nil, err
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

// waitOutcome is the result of awaitTurn.
type waitOutcome int

const (
	turnFinished waitOutcome = iota
	connectionClosed
	callerCancelled
)

// awaitTurn waits for the turn's terminal event, the connection closing, or
// the caller giving up. A finished turn has priority: when the connection
// closes right after the terminal event, both channels are ready and Go's
// select would pick at random, so the done channel is rechecked before a
// completed review is discarded.
func awaitTurn(done, closed, cancelled <-chan struct{}) waitOutcome {
	select {
	case <-done:
		return turnFinished
	case <-closed:
		select {
		case <-done:
			return turnFinished
		default:
			return connectionClosed
		}
	case <-cancelled:
		select {
		case <-done:
			return turnFinished
		default:
			return callerCancelled
		}
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

// knownTurn returns the turn id learned from turn/started, if any.
func (w *turnWatcher) knownTurn() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.turnID
}

// setTurn records the turn id from the review/start response. It reports
// false when an earlier turn/started established a different id.
func (w *turnWatcher) setTurn(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.turnID == "" {
		w.turnID = id
		return true
	}
	return w.turnID == id
}

// matches reports whether an event belongs to this watcher's turn. It is
// fail-closed: the turn id must already be established and the event must
// carry the same nonempty thread and turn ids. Events that arrive before
// turn/started, or with a missing id, are ignored rather than attributed.
func (w *turnWatcher) matches(threadID, turnID string) bool {
	return w.turnID != "" && threadID == w.threadID && turnID != "" && turnID == w.turnID
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
		if unmarshal(params, &n) && n.ThreadID == w.threadID && w.turnID == "" && n.Turn.ID != "" {
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
	overflow bool
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
		overflow: w.overflow,
		turnErr:  w.turnErr,
		lastErr:  w.lastErr,
	}
}

// text selects the review text. The review item is a single bounded
// message and is always complete. The fallbacks are aggregates; when they
// overflowed, a truncated prefix must not be returned as a review.
func (r turnResult) text() (string, error) {
	if r.review != "" {
		return r.review, nil
	}
	if r.overflow {
		return "", fmt.Errorf("%w: accumulated output exceeded %d bytes", ErrReviewTooLarge, maxCollectedText)
	}
	if r.messages != "" {
		return r.messages, nil
	}
	if r.deltas != "" {
		return r.deltas, nil
	}
	return "", ErrNoReviewText
}

// errorMessage describes a failed turn, including the Codex error code when
// the server supplied one.
func (r turnResult) errorMessage() string {
	e := r.turnErr
	if e == nil || e.Message == "" {
		e = r.lastErr
	}
	if e == nil {
		return "no error details"
	}
	msg := e.Message
	if msg == "" {
		msg = "no error details"
	}
	if code := codexErrorCode(e.CodexErrorInfo); code != "" {
		msg += " (codex error: " + code + ")"
	}
	return msg
}

// codexErrorCode renders the schema's codexErrorInfo, which is either a
// bare code string or an object with one variant key that may carry an
// HTTP status, as a short bounded diagnostic.
func codexErrorCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var code string
	if json.Unmarshal(raw, &code) == nil {
		return code
	}
	var variant map[string]struct {
		HTTPStatusCode *int `json:"httpStatusCode"`
	}
	if json.Unmarshal(raw, &variant) != nil || len(variant) == 0 {
		return "unrecognized"
	}
	for name, v := range variant {
		if v.HTTPStatusCode != nil {
			return fmt.Sprintf("%s, http %d", name, *v.HTTPStatusCode)
		}
		return name
	}
	return "unrecognized"
}

func unmarshal(data []byte, v any) bool {
	return json.Unmarshal(data, v) == nil
}
