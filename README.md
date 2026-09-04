# Counterpoint

Counterpoint lets one coding agent ask a persistent Codex reviewer to review a
specific local commit, then continue the same review conversation across
multiple rounds.

The initial integration is designed for Claude Code, but the core abstraction
is not Claude-specific: an MCP client submits a repository, branch, commit, and
branch notes; Counterpoint finds or resumes the Codex thread associated with
that repository and branch; Codex inspects the immutable commit and returns its
review synchronously.

Counterpoint is intentionally small. It automates the handoff between an author
and a reviewer without becoming a general-purpose orchestrator.

## Status

The MVP described in the [MVP specification](docs/MVP.md) is implemented and
covered by automated tests against a fake app-server. Live acceptance against
a real Codex CLI is a manual step recorded in the specification.

## Intended workflow

1. The authoring agent works on a feature branch, verifies its changes, and
   commits them locally.
2. The authoring agent produces branch notes describing the commit, verification,
   decisions, and any open questions.
3. The agent calls Counterpoint's blocking MCP review tool with the repository,
   branch, commit, and branch notes.
4. Counterpoint resumes the persistent Codex thread for that repository and
   branch, and Codex reviews the branch snapshot at that commit, from its merge
   base with the primary branch to the tip, without modifying the workspace.
5. The authoring agent addresses findings, commits the next round, and calls the
   same tool again. Codex retains the earlier review context.
6. After Codex approves, work stops at a human gate. Counterpoint does not push,
   open a pull request, merge, or approve on the human's behalf.

```text
Claude Code (or another MCP client)
              |
              | MCP: review(repo, branch, commit, branch_notes)
              v
        Counterpoint
              |
              | JSON-RPC over stdio
              v
      codex app-server
              |
              `-- one persistent Codex thread per repository + branch
```

## MVP shape

The MVP is a Go executable that:

- serves one blocking MCP tool over stdio;
- launches a local `codex app-server` process per review and uses its inline
  review mode on a persistent thread;
- identifies workflows by canonical repository plus full branch ref;
- persists the corresponding Codex thread IDs in a small JSON state file;
- requires a clean worktree at the branch tip and reviews the whole branch in a
  read-only, network-disabled Codex sandbox;
- serializes reviews across processes with a file lock and bounds each review
  with a timeout; and
- returns Codex's review, plus any bridge warnings, to the MCP client.

The MVP deliberately excludes a resident daemon, background jobs, multiple
reviewers, remote execution, push/PR automation, and a general workflow engine.

## Prerequisites

- Go, for building Counterpoint.
- A locally installed and authenticated Codex CLI that provides
  `codex app-server`. Counterpoint is developed against `codex-cli 0.153.1`
  and fails clearly on an incompatible protocol rather than enforcing an exact
  version.
- An MCP client such as Claude Code.

## Timeouts

Each review is bounded by a fixed twenty-minute Counterpoint timeout. Claude
Code separately aborts a stdio MCP tool call that has produced no response for
thirty minutes by default, controlled by the
`CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT` environment variable in milliseconds. Keep
that value above twenty minutes if you have lowered it. The per-call wall-clock
limit, `MCP_TOOL_TIMEOUT` or the per-server `timeout` field in `.mcp.json`,
defaults to many hours and normally needs no change.

## Installation and use

Build the binary and register it with Claude Code as a stdio MCP server:

```bash
make build
claude mcp add counterpoint -- "$PWD/bin/counterpoint"
```

Counterpoint exposes one tool, `review`, taking `repo`, `branch`, `commit`,
and `branch_notes`. It returns the canonical repository path, the full branch
ref, the reviewed commit and its merge base, the round number, Codex's review
text, any bridge warnings, and whether the result was replayed from state.
State lives under the user configuration directory in a `counterpoint`
subdirectory; `COUNTERPOINT_STATE_FILE` overrides the path for tests and
unusual installations. Diagnostics go to stderr only.

Prerequisites at review time: a clean worktree checked out at the tip of a
non-primary branch, and a locally authenticated Codex CLI.

## Development

```bash
make check     # gofmt check, go vet, golangci-lint, go test -race; what CI runs
make build     # bin/counterpoint
make schema    # regenerate the codex app-server JSON schema into .schema/
make install-hooks  # pre-commit hook that runs make check
```

## Name

In musical counterpoint, independent voices retain their own lines while
working against the same material. Here, the author and reviewer do the same.

## License

See [LICENSE](LICENSE).
