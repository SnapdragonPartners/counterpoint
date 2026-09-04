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

	// maxWarningDetail bounds how much of a declined request is quoted.
	maxWarningDetail = 200
)

// Sentinel errors for the transport.
var (
	ErrProcessExited   = errors.New("app-server process exited")
	ErrMessageTooLarge = errors.New("app-server message exceeds the size limit")
	ErrClosed          = errors.New("app-server client is closed")
)

// notificationHandler receives every notification; it must return quickly
// because it runs on the reader goroutine.
type notificationHandler func(method string, params json.RawMessage)

// conn owns the child process and the protocol streams.
type conn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	log   *slog.Logger

	writeMu sync.Mutex

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan envelope
	handlers map[int64]notificationHandler
	nextHnd  int64
	warnings []string

	// closed is closed when the reader loop ends; exitErr says why.
	closed  chan struct{}
	exitErr error
	// waitDone is closed once cmd.Wait has returned.
	waitDone chan struct{}
	waitErr  error
}

// spawn starts the child process and its reader goroutine.
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
		cmd:      cmd,
		stdin:    stdin,
		log:      log,
		pending:  map[int64]chan envelope{},
		handlers: map[int64]notificationHandler{},
		closed:   make(chan struct{}),
		waitDone: make(chan struct{}),
	}
	go c.readLoop(stdout)
	go func() {
		c.waitErr = cmd.Wait()
		close(c.waitDone)
	}()
	return c, nil
}

// readLoop is the single stdout reader. It dispatches responses to pending
// calls, notifications to handlers, and server-originated requests to the
// decline handler. When it ends, every pending call fails.
func (c *conn) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxMessageSize)

	var err error
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
	if serr := scanner.Err(); serr != nil {
		if errors.Is(serr, bufio.ErrTooLong) {
			err = ErrMessageTooLarge
		} else {
			err = fmt.Errorf("read app-server stdout: %w", serr)
		}
	} else {
		err = ErrProcessExited
	}
	c.shutdown(err)
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

// shutdown records why the reader ended and fails every pending call.
func (c *conn) shutdown(err error) {
	c.mu.Lock()
	c.exitErr = err
	pending := c.pending
	c.pending = map[int64]chan envelope{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- envelope{Error: &rpcError{Message: err.Error()}}
	}
	close(c.closed)
}

// call sends a request and waits for its response, honoring ctx and the
// process lifetime. result may be nil to discard the result.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	select {
	case <-c.closed:
		return c.closedErr()
	default:
	}
	id := c.nextID.Add(1)
	ch := make(chan envelope, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, err)
	}

	select {
	case env := <-ch:
		if env.Error != nil {
			select {
			case <-c.closed:
				return fmt.Errorf("%s: %w", method, c.closedErr())
			default:
			}
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
func (c *conn) notify(method string, params any) error {
	msg := map[string]any{"method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.write(msg); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

// write serializes one message as a single line. Writes are serialized so
// lines never interleave.
func (c *conn) write(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	if len(data)+1 > MaxMessageSize {
		return ErrMessageTooLarge
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("write to app-server: %w", err)
	}
	return nil
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

// handleServerRequest answers a server-originated request with a bounded
// decline. With a read-only sandbox and approval policy never these should
// not occur; each one is recorded as a warning for the caller.
func (c *conn) handleServerRequest(env *envelope) {
	var p serverRequestParams
	_ = json.Unmarshal(env.Params, &p)

	var result any
	switch env.Method {
	case requestCommandApproval, requestFileChangeApproval:
		result = map[string]string{"decision": decisionDecline}
	case requestLegacyExecApproval, requestLegacyPatchApproval:
		result = map[string]string{"decision": legacyDecisionDenied}
	case requestPermissions:
		result = map[string]any{"permissions": map[string]any{}}
	case requestUserInput:
		result = map[string]any{"answers": map[string]any{}}
	default:
		c.addWarning(fmt.Sprintf("rejected unsupported app-server request %s", env.Method))
		c.respond(env.ID, nil, &rpcError{Code: codeMethodNotFound, Message: "unsupported request: " + env.Method})
		return
	}

	detail := p.Command
	if detail == "" {
		detail = p.Reason
	}
	if detail != "" {
		detail = ": " + truncateDetail(detail)
	}
	c.addWarning(fmt.Sprintf("declined app-server request %s%s", env.Method, detail))
	c.respond(env.ID, result, nil)
}

func (c *conn) respond(id json.RawMessage, result any, rpcErr *rpcError) {
	msg := map[string]any{"id": id}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	if err := c.write(msg); err != nil {
		c.log.Warn("app-server: failed to answer server request", "error", err)
	}
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

func truncateDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxWarningDetail {
		return s[:maxWarningDetail] + "..."
	}
	return s
}

// close ends the child: stdin is closed, the process gets closeGrace to
// exit, then it is killed. It always waits for the process to be reaped.
func (c *conn) close() {
	_ = c.stdin.Close()
	select {
	case <-c.waitDone:
	case <-time.After(closeGrace):
		_ = c.cmd.Process.Kill()
		<-c.waitDone
	}
	<-c.closed
}
