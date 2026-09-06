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

The MVP described in the [MVP specification](docs/MVP.md) is complete. It is
covered by automated tests against a fake app-server and was accepted in a
live run against a real Codex CLI; the specification records the status and
links the follow-up issues.

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
   same tool again. Counterpoint quotes the last three rounds' verdicts back
   to the reviewer, which otherwise starts each round without memory.
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
  read-only, network-disabled Codex sandbox, or, on request, in a disposable
  checkout of the commit where the reviewer can build and run tests offline;
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

Counterpoint applies two fixed phase budgets inside a review call: sixty
seconds for setup, which covers starting `codex app-server`, its handshake, and
starting or resuming the thread, and then twenty minutes for the review turn
itself. These are not a bound on the whole call. Lock acquisition waits up to
two seconds, Git validation and state persistence have no Counterpoint
deadline, and cleanup after a failure or cancellation can add up to five
seconds waiting for the turn to interrupt and five more waiting for the child
to exit before it is killed. A call that hits every budget can exceed
twenty-one minutes by that cleanup time plus however long Git takes.

Claude Code separately aborts a stdio MCP tool call that has produced no
response for thirty minutes by default, controlled by the
`CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT` environment variable in milliseconds. Keep
that default; it leaves adequate margin over the phase budgets. Do not lower it
to anything near twenty-one minutes. The per-call wall-clock limit,
`MCP_TOOL_TIMEOUT` or the per-server `timeout` field in `.mcp.json`, defaults
to many hours and normally needs no change.

## Limits

- `branch_notes` may be at most 1 MiB (1,048,576 bytes) decoded; longer notes
  are rejected before any work starts.
- One MCP request line may be at most 6 MiB plus 64 KiB (6,356,992 bytes) on
  the wire, which fits maximal notes after JSON escaping plus framing. A longer
  line, or a request spread over several lines, ends the session.
- Bridge warnings returned with a review are capped at 32 entries totalling at
  most 8 KiB (8,192 bytes). When any are omitted, one additional final entry
  reports the omitted count; that marker is not counted against either cap.

## Installation and use

Install the binary and register it with Claude Code as a stdio MCP server at
user scope, so it is available in every project:

```bash
make install
make register
```

`make install` builds a version-stamped binary into `GOBIN`, or `GOPATH/bin`
when `GOBIN` is unset; make sure that directory is on `PATH`. `make register`
runs `claude mcp add -s user counterpoint -- counterpoint` when the name is
not yet registered at user scope and does nothing otherwise, so it is safe to
repeat. The
registration stores only the command name, resolved on `PATH` each session,
so it is a once-per-machine step that survives later installs. If that
directory cannot be on `PATH`, register the absolute path by hand instead; the
target prints the command when `counterpoint` does not resolve. Registering
the installed binary rather than `bin/counterpoint` keeps the tool stable
while the repository is being developed: `make build` produces the build
under test, and `make install` promotes it. MCP servers start with the client
session, so restart Claude Code after installing a new version.

Counterpoint exposes one tool, `review`, taking `repo`, `branch`, `commit`,
`branch_notes`, and an optional `build` flag. It returns the canonical
repository path, the full branch ref, the reviewed commit and its merge base,
the round number, Codex's review text, any bridge warnings, and whether the
result was replayed from state. State lives under the user configuration
directory in a `counterpoint` subdirectory; `COUNTERPOINT_STATE_FILE`
overrides the path for tests and unusual installations. Diagnostics go to
stderr only.

With `build: true` the reviewer gets a disposable checkout of the commit under
the user cache directory (`counterpoint/checkouts`, or
`COUNTERPOINT_CHECKOUT_DIR`) and may build and run tests there, offline. The
checkout is deleted after every review; a per-branch build cache beside it is
kept, so expect one cold build per branch and a test run per round on top of
the review itself, and ask for it when the change warrants that evidence. The
reviewer's tracked-file changes in the checkout, if any, come back as a
warning. Lint tooling that needs a download is not available to the reviewer
and is reported as not run; CI still lints. The cache has no eviction yet
([issue 15](https://github.com/SnapdragonPartners/counterpoint/issues/15));
deleting directories under the scratch root is safe between reviews.

Prerequisites at review time: a clean worktree checked out at the tip of a
non-primary branch, and a locally authenticated Codex CLI.

## Operational notes

- Counterpoint names its threads `Counterpoint review: <repository> <branch>`
  so they are easy to leave alone in the Codex app. Only one process can hold
  a thread at a time. If the thread is open in the app, the next review fails
  and says so; archive the thread in the app and retry. Counterpoint
  unarchives it and takes it over. Do not unarchive it in the app, which would
  open it there again. Opening the thread in the app while a review runs
  fails on the app side.
- The "review turn starting" log line reports `effort`, the configured
  override in force, alongside `reported_effort`, the value the app-server
  echoed back. After a resume the app-server may omit that value, so an empty
  `reported_effort` means it was not reported, not that no effort applied.

## Development

```bash
make check     # gofmt check, go vet, golangci-lint, go test -race; what CI runs
make build     # bin/counterpoint, the build under test
make install   # versioned binary into GOBIN or GOPATH/bin
make register  # register the installed binary with Claude Code once per machine
make schema    # regenerate the codex app-server JSON schema into .schema/
make install-hooks  # pre-commit hook that runs make check
```

## Name

In musical counterpoint, independent voices retain their own lines while
working against the same material. Here, the author and reviewer do the same.

## License

See [LICENSE](LICENSE).
