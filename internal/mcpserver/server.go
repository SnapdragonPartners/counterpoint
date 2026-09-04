// Package mcpserver exposes Counterpoint's single blocking review tool over
// MCP stdio. Stdout carries protocol only; diagnostics go to stderr.
package mcpserver

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
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

	// MaxRequestBytes bounds one JSONL line read from the MCP client. It
	// leaves room for the largest allowed branch notes plus framing; a
	// longer line ends the session rather than being buffered.
	MaxRequestBytes = review.MaxBranchNotesBytes + 64<<10
)

// ErrRequestTooLarge reports an MCP input line over MaxRequestBytes.
var ErrRequestTooLarge = errors.New("mcp request exceeds the size limit")

// Input is the review tool's arguments. Field descriptions become the input
// schema shown to the MCP client.
type Input struct {
	Repo        string `json:"repo" jsonschema:"Absolute path inside the Git worktree to review"`
	Branch      string `json:"branch" jsonschema:"Local branch name, bare or as refs/heads/<name>; never the primary branch"`
	Commit      string `json:"commit" jsonschema:"Commit to review; must be the branch tip and the checked-out HEAD of a clean worktree"`
	BranchNotes string `json:"branch_notes" jsonschema:"Author-written handoff notes: what changed, verification, how prior findings were resolved, open questions"`
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
			"Blocks until the review completes. Counterpoint never pushes, opens pull requests, merges, or edits the repository.",
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

// boundedLineReader passes bytes through until a single line exceeds the
// limit, after which every read fails with ErrRequestTooLarge. The SDK
// decodes newline-delimited JSON from this reader, so an oversized request
// ends the session instead of being buffered in full.
type boundedLineReader struct {
	r       *bufio.Reader
	closer  io.Closer
	limit   int
	current int
	failed  error
}

func newBoundedLineReader(rc io.ReadCloser, limit int) *boundedLineReader {
	return &boundedLineReader{r: bufio.NewReader(rc), closer: rc, limit: limit}
}

func (b *boundedLineReader) Read(p []byte) (int, error) {
	if b.failed != nil {
		return 0, b.failed
	}
	n, err := b.r.Read(p)
	for _, c := range p[:n] {
		if c == '\n' {
			b.current = 0
			continue
		}
		b.current++
		if b.current > b.limit {
			b.failed = fmt.Errorf("%w: line longer than %d bytes", ErrRequestTooLarge, b.limit)
			return 0, b.failed
		}
	}
	return n, err
}

func (b *boundedLineReader) Close() error {
	return b.closer.Close()
}
