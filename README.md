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

Counterpoint is at the specification stage. The [MVP specification](docs/MVP.md)
defines the first implementation.

## Intended workflow

1. The authoring agent works on a feature branch, verifies its changes, and
   commits them locally.
2. The authoring agent produces branch notes describing the commit, verification,
   decisions, and any open questions.
3. The agent calls Counterpoint's blocking MCP review tool with the repository,
   branch, commit, and branch notes.
4. Counterpoint resumes the persistent Codex thread for that repository and
   branch, and Codex reviews the exact commit without modifying the workspace.
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
- launches and speaks JSON-RPC to a local `codex app-server` process;
- identifies workflows by canonical repository plus full branch ref;
- persists the corresponding Codex thread IDs in a small JSON state file;
- reviews exact local commits in a read-only Codex sandbox; and
- returns Codex's review to the MCP client.

The MVP deliberately excludes a resident daemon, background jobs, multiple
reviewers, remote execution, push/PR automation, and a general workflow engine.

## Prerequisites

- Go, for building Counterpoint.
- A locally installed and authenticated Codex CLI that provides
  `codex app-server`.
- An MCP client such as Claude Code.

## Name

In musical counterpoint, independent voices retain their own lines while
working against the same material. Here, the author and reviewer do the same.

## License

See [LICENSE](LICENSE).
