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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/counterpoint/internal/review"
)

const (
	// ToolName is the single tool Counterpoint exposes.
	ToolName = "review"

	// MaxRequestBytes bounds one JSONL line read from the MCP client. A
	// decoded byte of branch notes can occupy up to six bytes on the wire
	// as a JSON escape, so the largest allowed notes fit with room for
	// framing; a longer line ends the session rather than being buffered.
	MaxRequestBytes = 6*review.MaxBranchNotesBytes + 64<<10
)

// Sentinel errors for MCP input framing.
var (
	ErrRequestTooLarge = errors.New("mcp request exceeds the size limit")
	ErrRequestFraming  = errors.New("mcp request is not one complete JSON value per line")
)

// Input is the review tool's arguments. Field descriptions become the input
// schema shown to the MCP client.
type Input struct {
	Repo        string `json:"repo" jsonschema:"Absolute path inside the Git worktree to review"`
	Branch      string `json:"branch" jsonschema:"Local branch name, bare or as refs/heads/<name>; never the primary branch"`
	Commit      string `json:"commit" jsonschema:"Commit to review; must be the branch tip and the checked-out HEAD of a clean worktree"`
	BranchNotes string `json:"branch_notes" jsonschema:"Author-written handoff notes (at most 1 MiB): what changed, verification, how prior findings were resolved, open questions"`
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
	server := mcp.NewServer(&mcp.Implementation{Name: "counterpoint", Version: version}, &mcp.ServerOptions{Logger: log})
	mcp.AddTool(server, &mcp.Tool{
		Name: ToolName,
		Description: "Ask the persistent Codex reviewer for this repository and branch to review a local commit. " +
			"Blocks until the review completes: up to sixty seconds of setup plus a twenty-minute review turn. " +
			"Counterpoint never pushes, opens pull requests, merges, or edits the repository.",
	}, func(reqCtx context.Context, _ *mcp.CallToolRequest, in Input) (*mcp.CallToolResult, Output, error) {
		ctx, cancel := context.WithCancel(reqCtx)
		defer cancel()
		stop := context.AfterFunc(lifecycle, cancel)
		defer stop()

		id := newRequestID()
		ctx = review.WithRequestID(ctx, id)
		log.Info("review request received", "request", id, "branch", in.Branch, "commit", in.Commit, "notes_bytes", len(in.BranchNotes))

		res, err := svc.Review(ctx, review.Request{
			Repo: in.Repo, Branch: in.Branch, Commit: in.Commit, BranchNotes: in.BranchNotes,
		})
		if err != nil {
			// Returned errors become tool errors, not protocol errors.
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
