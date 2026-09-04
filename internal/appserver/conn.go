package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// MaxMessageSize bounds one JSONL line in either direction. Reviews and
	// protocol items exceed Go's default 64 KiB scanner token size; this
	// leaves ample room while keeping memory bounded.
	MaxMessageSize = 16 << 20

	// closeGrace is how long Close waits for the child to exit after its
	// stdin is closed before killing it.
	closeGrace = 5 * time.Second

	// outboxSize bounds messages queued for the writer goroutine. A child
	// that stops reading stdin fills the queue; callers then fail through
	// their contexts instead of blocking on the pipe.
	outboxSize = 64
)

// Sentinel errors for the transport.
var (
	ErrProcessExited   = errors.New("app-server process exited")
	ErrMessageTooLarge = errors.New("app-server message exceeds the size limit")
	ErrClosed          = errors.New("app-server client is closed")
	ErrWriteBacklog    = errors.New("app-server is not reading its input")
)

// notificationHandler receives every notification; it must return quickly
// because it runs on the reader goroutine.
type notificationHandler func(method string, params json.RawMessage)

// conn owns the child process and the protocol streams.
type conn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	log   *slog.Logger

	// outbox feeds the single writer goroutine. stopWriter tells it to
	// exit; writerDone closes when it has.
	outbox     chan []byte
	stopWriter chan struct{}
	stopOnce   sync.Once
	writerDone chan struct{}
	killOnce   sync.Once

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan envelope
	handlers map[int64]notificationHandler
	nextHnd  int64
	warnings []string
	exitErr  error

	// closed is closed when the reader has drained stdout to EOF and every
	// pending call has been failed. The process is reaped only after that,
	// as StdoutPipe requires, and waitDone then closes.
	closed   chan struct{}
	waitDone chan struct{}
}

// spawn starts the child process and its reader and writer goroutines.
func spawn(ctx context.Context, command string, args []string, stderr io.Writer, log *slog.Logger) (*conn, error) {
	cmd := exec.CommandContext(ctx, command, args...) //nolint:gosec // argument array from configuration; never a shell
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("app-server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	c := &conn{
		cmd:        cmd,
		stdin:      stdin,
		log:        log,
		outbox:     make(chan []byte, outboxSize),
		stopWriter: make(chan struct{}),
		writerDone: make(chan struct{}),
		pending:    map[int64]chan envelope{},
		handlers:   map[int64]notificationHandler{},
		closed:     make(chan struct{}),
		waitDone:   make(chan struct{}),
	}
	go c.writeLoop()
	go c.readLoop(stdout)
	return c, nil
}

// readLoop is the single stdout reader. It dispatches responses to pending
// calls, notifications to handlers, and server-originated requests to the
// decline handler. It drains stdout to EOF before the process is reaped so
// no final notification is lost, then fails every pending call.
func (c *conn) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxMessageSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var env envelope
		if uerr := json.Unmarshal(line, &env); uerr != nil {
			c.log.Warn("app-server: ignoring unparseable line", "error", uerr)
			continue
		}
		c.dispatch(&env)
	}

	var err error
	switch serr := scanner.Err(); {
	case serr == nil:
		err = ErrProcessExited
	case errors.Is(serr, bufio.ErrTooLong):
		err = ErrMessageTooLarge
	default:
		err = fmt.Errorf("read app-server stdout: %w", serr)
	}
	c.shutdown(err)

	// Reap only after stdout is drained: cmd.Wait closes the read end of
	// the stdout pipe, so calling it earlier could discard final bytes the
	// child wrote just before exiting. This ordering is the guarantee; the
	// exit-after-completion test exercises it but, being a race, cannot
	// prove it. An oversized line leaves the pipe unread; the child must
	// be ended before Wait can return.
	if !errors.Is(err, ErrProcessExited) {
		c.kill()
	}
	_ = c.cmd.Wait()
	close(c.waitDone)
}

// writeLoop is the single stdin writer. A blocked pipe blocks only this
// goroutine; callers wait on their own contexts. It exits when told to
// stop or when a write fails, and a failed write terminates the connection
// so pending and future calls fail instead of waiting on a dead queue.
func (c *conn) writeLoop() {
	defer close(c.writerDone)
	for {
		select {
		case <-c.stopWriter:
			return
		case data := <-c.outbox:
			if _, err := c.stdin.Write(data); err != nil {
				c.terminate(fmt.Errorf("write to app-server: %w", err))
				return
			}
		}
	}
}

// terminate ends the connection for a reason of Counterpoint's own: the
// reason is recorded first so it wins over the generic exit error, the
// writer is stopped, and the child is killed. The reader then reaches EOF,
// fails every pending call, and reaps the process.
func (c *conn) terminate(err error) {
	c.mu.Lock()
	if c.exitErr == nil {
		c.exitErr = err
	}
	c.mu.Unlock()
	c.log.Warn("app-server: terminating connection", "error", err)
	c.stopWriting()
	c.kill()
}

func (c *conn) stopWriting() {
	c.stopOnce.Do(func() { close(c.stopWriter) })
}

func (c *conn) kill() {
	c.killOnce.Do(func() { _ = c.cmd.Process.Kill() })
}

func (c *conn) dispatch(env *envelope) {
	switch {
	case env.Method != "" && len(env.ID) > 0:
		c.handleServerRequest(env)
	case env.Method != "":
		c.mu.Lock()
		handlers := make([]notificationHandler, 0, len(c.handlers))
		for _, h := range c.handlers {
			handlers = append(handlers, h)
		}
		c.mu.Unlock()
		for _, h := range handlers {
			h(env.Method, env.Params)
		}
	case len(env.ID) > 0:
		var id int64
		if err := json.Unmarshal(env.ID, &id); err != nil {
			c.log.Warn("app-server: response with non-integer id ignored", "id", string(env.ID))
			return
		}
		c.mu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if !ok {
			c.log.Warn("app-server: response for unknown request ignored", "id", id)
			return
		}
		ch <- *env
	default:
		c.log.Warn("app-server: message with neither method nor id ignored")
	}
}

// shutdown records why the reader ended, unless terminate already
// recorded a more specific reason, fails every pending call, and stops the
// writer.
func (c *conn) shutdown(err error) {
	c.mu.Lock()
	if c.exitErr == nil {
		c.exitErr = err
	}
	pending := c.pending
	c.pending = map[int64]chan envelope{}
	c.mu.Unlock()
	// Pending calls are woken by closing their channels, never by a
	// synthetic RPC error, so they report the typed connection error
	// recorded above and errors.Is keeps working for callers.
	for _, ch := range pending {
		close(ch)
	}
	close(c.closed)
	c.stopWriting()
}

// call sends a request and waits for its response, honoring ctx and the
// process lifetime. result may be nil to discard the result.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan envelope, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, err)
	}

	select {
	case env, ok := <-ch:
		if !ok {
			return fmt.Errorf("%s: %w", method, c.closedErr())
		}
		if env.Error != nil {
			return fmt.Errorf("%s: %w", method, env.Error)
		}
		if result != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, result); err != nil {
				return fmt.Errorf("%s: decode result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.closed:
		return fmt.Errorf("%s: %w", method, c.closedErr())
	}
}

// notify sends a notification.
func (c *conn) notify(ctx context.Context, method string, params any) error {
	msg := map[string]any{"method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.send(ctx, msg); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

// send encodes one message as a single line and queues it for the writer,
// giving up when ctx ends or the connection is closing. A message that
// slips into the queue as the writer dies is harmless: the caller waits
// on closed, which fires once the reader has shut down.
func (c *conn) send(ctx context.Context, msg any) error {
	data, err := encodeLine(msg)
	if err != nil {
		return err
	}
	select {
	case <-c.closed:
		return c.closedErr()
	case <-c.writerDone:
		return c.closedErr()
	default:
	}
	select {
	case c.outbox <- data:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return c.closedErr()
	case <-c.writerDone:
		return c.closedErr()
	}
}

// sendFromReader queues a reply to a server-originated request without
// blocking the reader goroutine. A full outbox means the child has stopped
// consuming input while still asking questions; dropping the reply would
// let it wait forever, so the connection is terminated instead.
func (c *conn) sendFromReader(msg any) {
	data, err := encodeLine(msg)
	if err != nil {
		c.terminate(fmt.Errorf("encode response to server request: %w", err))
		return
	}
	select {
	case c.outbox <- data:
	default:
		c.terminate(ErrWriteBacklog)
	}
}

func encodeLine(msg any) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	if len(data)+1 > MaxMessageSize {
		return nil, ErrMessageTooLarge
	}
	return append(data, '\n'), nil
}

func (c *conn) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exitErr != nil {
		return c.exitErr
	}
	return ErrClosed
}

// subscribe registers a notification handler and returns its remover.
func (c *conn) subscribe(h notificationHandler) func() {
	c.mu.Lock()
	c.nextHnd++
	id := c.nextHnd
	c.handlers[id] = h
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.handlers, id)
		c.mu.Unlock()
	}
}

// legacyRejection is the reason given on the legacy approval methods.
const legacyRejection = "Counterpoint reviews in read-only mode and never grants execution or write approval"

// handleServerRequest answers a server-originated request with a bounded
// decline. With a read-only sandbox and approval policy never these should
// not occur; each one is recorded as a warning for the caller. Warnings and
// logs carry the request kind and identifiers only: a command or reason
// supplied by the app-server may contain secrets or model output.
func (c *conn) handleServerRequest(env *envelope) {
	var p serverRequestParams
	_ = json.Unmarshal(env.Params, &p)

	var result any
	switch env.Method {
	case requestCommandApproval, requestFileChangeApproval:
		result = map[string]string{"decision": decisionDecline}
	case requestLegacyExecApproval, requestLegacyPatchApproval:
		result = map[string]any{"decision": map[string]any{"denied": map[string]string{"rejection": legacyRejection}}}
	case requestPermissions:
		result = map[string]any{"permissions": map[string]any{}}
	case requestUserInput:
		result = map[string]any{"answers": map[string]any{}}
	default:
		c.addWarning(fmt.Sprintf("rejected unsupported app-server request %s%s", env.Method, identifiers(p)))
		c.sendFromReader(map[string]any{"id": env.ID, "error": &rpcError{Code: codeMethodNotFound, Message: "unsupported request: " + env.Method}})
		return
	}
	c.addWarning(fmt.Sprintf("declined app-server request %s%s", env.Method, identifiers(p)))
	c.sendFromReader(map[string]any{"id": env.ID, "result": result})
}

// identifiers formats the request's item and turn ids for a warning.
func identifiers(p serverRequestParams) string {
	var parts []string
	if p.ItemID != "" {
		parts = append(parts, "item "+p.ItemID)
	}
	if p.TurnID != "" {
		parts = append(parts, "turn "+p.TurnID)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func (c *conn) addWarning(w string) {
	c.log.Warn("app-server: " + w)
	c.mu.Lock()
	c.warnings = append(c.warnings, w)
	c.mu.Unlock()
}

// takeWarnings returns and clears accumulated warnings.
func (c *conn) takeWarnings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.warnings
	c.warnings = nil
	return w
}

// close ends the child. The writer is told to stop and stdin is closed so
// a well-behaved child exits and a writer blocked on a full pipe is
// released; after closeGrace the child is killed. It returns once stdout
// is drained, the process is reaped, and the writer has exited.
func (c *conn) close() {
	c.stopWriting()
	_ = c.stdin.Close()
	select {
	case <-c.waitDone:
	case <-time.After(closeGrace):
		c.kill()
		<-c.waitDone
	}
	<-c.writerDone
}
