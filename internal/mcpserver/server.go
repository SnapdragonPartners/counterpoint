// Package mcpserver exposes Counterpoint's single blocking review tool over
// MCP stdio. Stdout carries protocol only; diagnostics go to stderr.
package mcpserver

import (
	"context"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/counterpoint/internal/review"
)

// ToolName is the single tool Counterpoint exposes.
const ToolName = "review"

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

// New builds the MCP server with the review tool registered.
func New(svc *review.Service, version string, log *slog.Logger) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "counterpoint", Version: version}, &mcp.ServerOptions{Logger: log})
	mcp.AddTool(server, &mcp.Tool{
		Name: ToolName,
		Description: "Ask the persistent Codex reviewer for this repository and branch to review a local commit. " +
			"Blocks until the review completes. Counterpoint never pushes, opens pull requests, merges, or edits the repository.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in Input) (*mcp.CallToolResult, Output, error) {
		res, err := svc.Review(ctx, review.Request{
			Repo: in.Repo, Branch: in.Branch, Commit: in.Commit, BranchNotes: in.BranchNotes,
		})
		if err != nil {
			// Returned errors become tool errors, not protocol errors.
			return nil, Output{}, err
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

// Serve runs the server over stdio until the client disconnects or ctx ends.
func Serve(ctx context.Context, server *mcp.Server) error {
	return server.Run(ctx, &mcp.StdioTransport{})
}
