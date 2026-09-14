// Package mcpserver exposes Counterpoint's single blocking review tool over
// MCP stdio. Stdout carries protocol only; diagnostics go to stderr.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/counterpoint/internal/review"
)

const (
	// ToolName is the single tool Counterpoint exposes.
	ToolName = "review"

	// HeartbeatInterval is how often a running review call sends a
	// progress notification and measures its silence toward the client
	// (docs/design/sleep-survival.md).
	HeartbeatInterval = 30 * time.Second

	// ClientIdleTimeout is Claude Code's default idle timeout for a stdio
	// tool call: it aborts a call that has produced no response and no
	// progress notification for this long, counting wall-clock time, and
	// the abort does not cancel the call on this side.
	ClientIdleTimeout = 30 * time.Minute

	// MaxSilence is the silence toward the client at which a call is ended
	// rather than run on for a client that has probably given up. Silence
	// is wall-clock time since the last heartbeat sent, or since the
	// request when none was; a machine sleep counts in full. Every client
	// check before a tick sees at most the silence that tick measures, so
	// the interval of margin under the client's timeout keeps Counterpoint
	// failing first. With heartbeats this survives a sleep shorter than
	// MaxSilence less one interval; without a progress token it bounds the
	// whole call to MaxSilence of wall-clock time.
	MaxSilence = ClientIdleTimeout - HeartbeatInterval

	// MaxRequestBytes bounds one JSONL line read from the MCP client. A
	// decoded byte of branch notes can occupy up to six bytes on the wire
	// as a JSON escape, so the largest allowed notes fit with room for
	// framing; a longer line ends the session rather than being buffered.
	MaxRequestBytes = 6*review.MaxBranchNotesBytes + 64<<10
)

// Sentinel errors.
var (
	ErrRequestTooLarge = errors.New("mcp request exceeds the size limit")
	ErrRequestFraming  = errors.New("mcp request is not one complete JSON value per line")
	// ErrSilence ends a call whose silence toward the client reached
	// MaxSilence; it is the cause of the review's context.
	ErrSilence = errors.New("review call was silent toward the client for too long")
)

// Input is the review tool's arguments. Field descriptions become the input
// schema shown to the MCP client.
type Input struct {
	Repo        string `json:"repo" jsonschema:"Absolute path inside the Git worktree to review"`
	Branch      string `json:"branch" jsonschema:"Local branch name, bare or as refs/heads/<name>; never the primary branch"`
	Commit      string `json:"commit" jsonschema:"Commit to review; must be the branch tip and the checked-out HEAD of a clean worktree"`
	BranchNotes string `json:"branch_notes" jsonschema:"Author-written handoff notes (at most 1 MiB): what changed, verification, how prior findings were resolved, open questions"`
	Build       bool   `json:"build,omitempty" jsonschema:"Ask for a build-capable review: the reviewer gets a disposable checkout of the commit where it can build and run tests (offline). Costs more wall-clock time; default false is a read-only review of the worktree"`
}

// Output is the review tool's structured result.
type Output struct {
	Repo     string   `json:"repo" jsonschema:"Canonical worktree path that was reviewed"`
	Branch   string   `json:"branch" jsonschema:"Full local branch ref"`
	Commit   string   `json:"commit" jsonschema:"Full object id of the reviewed commit"`
	Base     string   `json:"base" jsonschema:"Merge base with the primary branch"`
	Round    int      `json:"round" jsonschema:"Review round number on this branch, owned by Counterpoint"`
	Review   string   `json:"review" jsonschema:"Codex's review text, verbatim"`
	Warnings []string `json:"warnings" jsonschema:"Bridge-level events such as declined permission requests; empty when none"`
	Replayed bool     `json:"replayed" jsonschema:"True when an identical completed request was answered from state"`
}

// New builds the MCP server with the review tool registered. lifecycle is
// the process lifetime: when it ends, every active review is cancelled so
// the Codex turn is interrupted and the child reaped before the server
// stops, which the SDK's own shutdown does not do for in-flight handlers.
func New(lifecycle context.Context, svc *review.Service, version string, log *slog.Logger) *mcp.Server {
	return newServer(lifecycle, svc, version, log, HeartbeatInterval, wallNow)
}

// wallNow is the wall clock with the monotonic reading stripped, so that
// subtracting two readings measures wall time, sleep included.
func wallNow() time.Time { return time.Now().Round(0) }

// newServer is New with the heartbeat interval and the wall clock as
// parameters for tests.
func newServer(lifecycle context.Context, svc *review.Service, version string, log *slog.Logger, heartbeat time.Duration, now func() time.Time) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "counterpoint", Version: version}, &mcp.ServerOptions{Logger: log})
	mcp.AddTool(server, &mcp.Tool{
		Name: ToolName,
		Description: "Ask the persistent Codex reviewer for this repository and branch to review a local commit. " +
			"Blocks until the review completes: up to sixty seconds of setup plus a twenty-minute review turn, " +
			"sending a progress notification every thirty seconds meanwhile so the client's idle timeout does not fire. " +
			"Counterpoint never pushes, opens pull requests, merges, or edits the repository.",
	}, func(reqCtx context.Context, req *mcp.CallToolRequest, in Input) (*mcp.CallToolResult, Output, error) {
		ctx, cancel := context.WithCancelCause(reqCtx)
		defer cancel(nil)
		stop := context.AfterFunc(lifecycle, func() { cancel(nil) })
		defer stop()

		id := newRequestID()
		ctx = review.WithRequestID(ctx, id)
		log.Info("review request received", "request", id, "branch", in.Branch, "commit", in.Commit, "notes_bytes", len(in.BranchNotes), "build", in.Build)
		stopWatch := watchCall(reqCtx, req, id, cancel, heartbeat, now, log)
		defer stopWatch()

		res, err := svc.Review(ctx, review.Request{
			Repo: in.Repo, Branch: in.Branch, Commit: in.Commit, BranchNotes: in.BranchNotes, Build: in.Build,
		})
		if err != nil {
			// Returned errors become tool errors, not protocol errors. A
			// call the watcher ended says so first.
			if cause := context.Cause(ctx); errors.Is(cause, ErrSilence) {
				err = fmt.Errorf("%w: %w", cause, err)
			}
			return nil, Output{}, fmt.Errorf("request %s: %w", id, err)
		}
		warnings := res.Warnings
		if warnings == nil {
			warnings = []string{}
		}
		return nil, Output{
			Repo: res.Repo, Branch: res.Branch, Commit: res.Commit, Base: res.Base,
			Round: res.Round, Review: res.Review, Warnings: warnings, Replayed: res.Replayed,
		}, nil
	})
	return server
}

// watchCall runs one goroutine for the call until the returned stop
// function is called, which is after the review has returned, so every
// phase is covered: the lock wait, Git validation, the disposable
// checkout, setup, the turn, and cleanup (docs/design/sleep-survival.md).
// Every interval it measures the call's silence toward the client, the
// wall-clock time since the last heartbeat it sent or since the request
// when none was, and ends the call through end with ErrSilence once that
// reaches MaxSilence: the client has probably given up, and a review must
// not run on unobserved. Then, when the request carries a progress token,
// it sends notifications/progress with a count of heartbeats as the
// progress value, which increases as the protocol requires whatever the
// wall clock does, and the wall-clock age of the call in the message.
// Without a token nothing can be sent and the log says so once. Sends run
// on a context detached from the request's cancellation, so a client that
// cancelled does not also make the last heartbeat fail. A send that fails
// means the connection is gone; it is logged once and the goroutine stops.
func watchCall(reqCtx context.Context, req *mcp.CallToolRequest, id string, end context.CancelCauseFunc, interval time.Duration, now func() time.Time, log *slog.Logger) func() {
	token := req.Params.GetProgressToken()
	if token == nil {
		log.Info("progress heartbeat disabled: the request carries no progress token; the call ends after MaxSilence", "request", id, "max_silence", MaxSilence)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(reqCtx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		start := now()
		lastSent := start
		ended := false
		for n := 1; ; n++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			t := now()
			if silence := t.Sub(lastSent); !ended && silence >= MaxSilence {
				ended = true
				why := "the machine slept"
				if token == nil {
					why = "the request carried no progress token, so no heartbeat could be sent"
				}
				log.Warn("review call ended: silent toward the client", "request", id, "silence", silence.Round(time.Second), "reason", why)
				end(fmt.Errorf("%w: %v without a heartbeat or a result (%s), which a client's default idle timeout of %v does not survive, so the client has probably given up; retry to start a fresh round",
					ErrSilence, silence.Round(time.Second), why, ClientIdleTimeout))
			}
			if token == nil {
				continue
			}
			err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token,
				Progress:      float64(n),
				Message:       fmt.Sprintf("review in progress for %s", t.Sub(start).Round(time.Second)),
			})
			if err != nil {
				log.Warn("progress heartbeat stopped: notification failed", "request", id, "error", err)
				return
			}
			lastSent = now()
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// newRequestID returns a short random correlation id for logs.
func newRequestID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "rand-unavailable"
	}
	return hex.EncodeToString(buf[:])
}

// Serve runs the server over stdin and stdout until the client disconnects
// or ctx ends. Input lines are bounded by MaxRequestBytes.
func Serve(ctx context.Context, server *mcp.Server, stdin io.ReadCloser, stdout io.WriteCloser) error {
	transport := &mcp.IOTransport{Reader: newBoundedLineReader(stdin, MaxRequestBytes), Writer: stdout}
	return server.Run(ctx, transport)
}

// boundedLineReader enforces the protocol's framing before the SDK sees
// any bytes: each physical line must be one complete JSON value of at most
// limit bytes. The SDK's decoder would otherwise accept a value spread
// across many short lines, defeating a per-line bound. An oversized or
// unframed line fails every subsequent read, ending the session instead of
// buffering the request.
type boundedLineReader struct {
	r      *bufio.Reader
	closer io.Closer
	limit  int
	buf    []byte // a validated line, including its newline, not yet consumed
	failed error
}

func newBoundedLineReader(rc io.ReadCloser, limit int) *boundedLineReader {
	return &boundedLineReader{r: bufio.NewReader(rc), closer: rc, limit: limit}
}

func (b *boundedLineReader) Read(p []byte) (int, error) {
	if b.failed != nil {
		return 0, b.failed
	}
	if len(b.buf) == 0 {
		line, err := b.nextLine()
		if err != nil {
			b.failed = err
			return 0, err
		}
		b.buf = line
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

// nextLine reads one line of at most limit bytes and requires it to be a
// complete JSON value. Blank lines are skipped. io.EOF is returned only
// when no partial line remains.
func (b *boundedLineReader) nextLine() ([]byte, error) {
	for {
		var line []byte
		for {
			chunk, err := b.r.ReadSlice('\n')
			line = append(line, chunk...)
			if len(line) > b.limit+1 {
				return nil, fmt.Errorf("%w: line longer than %d bytes", ErrRequestTooLarge, b.limit)
			}
			if err == nil {
				break
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) && len(bytes.TrimSpace(line)) == 0 {
				return nil, io.EOF
			}
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: unterminated final line", ErrRequestFraming)
			}
			return nil, fmt.Errorf("read mcp input: %w", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !json.Valid(bytes.TrimSpace(line)) {
			return nil, fmt.Errorf("%w: line is not a complete JSON value", ErrRequestFraming)
		}
		return line, nil
	}
}

func (b *boundedLineReader) Close() error {
	return b.closer.Close()
}
