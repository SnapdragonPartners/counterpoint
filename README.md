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

Counterpoint is in use, released through the Homebrew tap described under
"Installation and use". The [specification](docs/SPEC.md) is the accepted
contract for its behavior; it is covered by automated tests against a fake
app-server, and every change since the first release has been reviewed
through the tool itself.

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

## Shape

Counterpoint is a Go executable that:

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

It deliberately excludes a resident daemon, background jobs, multiple
reviewers, remote execution, push/PR automation, and a general workflow
engine; the specification lists what is out of scope.

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
itself. Both count awake time: Go's timers stop while the machine sleeps, so
a sleep never consumes budget. These are not a bound on the whole call. Lock
acquisition waits up to two seconds at the start (thirty when recording a
completed review), Git validation and state persistence have no Counterpoint
deadline, and cleanup after a failure or cancellation can add up to five
seconds waiting for the turn to interrupt and five more waiting for the child
to exit before it is killed. A call that hits every budget can exceed
twenty-one minutes by that cleanup time plus however long Git takes.

While a call runs, from the request's arrival until the result is returned,
Counterpoint sends an MCP progress notification every thirty seconds, so a
client's idle timeout does not fire on a review that is merely slow. The
notification carries the progress token the client put on the request,
which Claude Code sends on every tool call; a request without one gets no
heartbeats and a log line saying so. At the same
cadence Counterpoint measures the call's silence toward the client, the
wall-clock time since the last heartbeat it sent, or since the request when
it could send none, and ends the call once that reaches twenty-nine and a
half minutes: the turn is interrupted, the child reaped, the workflow lock
released, and the tool error says the call went silent and why. A review
that has already completed and been recorded when the bound is reached is
returned as a success, since the next identical request would replay it.
With
heartbeats, silence grows only while the machine sleeps, so a sleep shorter
than twenty-nine minutes is survived with the budgets untouched and a longer
one ends the call within thirty seconds of wake. Without a token the bound
applies to the call's whole wall-clock length, sleep or not.

Claude Code aborts a stdio MCP tool call that has produced no response and no
progress notification for thirty minutes by default, controlled by the
`CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT` environment variable in milliseconds. Keep
that default: the heartbeat holds it off while Counterpoint runs, and the
silence bound is derived from it so that Counterpoint gives up first. Do not
lower it to anything near the heartbeat interval. The abort does not cancel
the call on Counterpoint's side; that is why Counterpoint ends the call
itself rather than run a review for a client that has given up, which is
what happened in the incident behind
[issue 35](https://github.com/SnapdragonPartners/counterpoint/issues/35)
(`docs/design/sleep-survival.md`). A cancellation the client does send, or
closing the connection, interrupts the review at once. After a sleep long
enough to reach the client's timeout in one step, the client may abort on
wake before Counterpoint's next tick and discard the error it is sent; the
lock is still free within about forty seconds of wake, and a retry starts a
fresh round. The per-call wall-clock limit, `MCP_TOOL_TIMEOUT` or the
per-server `timeout` field in `.mcp.json`, defaults to many hours and
normally needs no change.

## Limits

- `branch_notes` may be at most 1 MiB (1,048,576 bytes) decoded; longer notes
  are rejected before any work starts.
- One MCP request line may be at most 6 MiB plus 64 KiB (6,356,992 bytes) on
  the wire, which fits maximal notes after JSON escaping plus framing. A longer
  line, or a request spread over several lines, ends the session.
- Bridge warnings returned with a review are capped at 32 entries totalling at
  most 8 KiB (8,192 bytes). When any are omitted, one additional final entry
  reports the omitted count; that marker is not counted against either cap.
- A build-capable review runs offline, with no network and no access to
  Docker or any other service on the machine, and there is no switch to
  change that. The reviewer acts on untrusted input (the commit, the branch
  notes, the repository's `COUNTERPOINT.md`), so the sandbox is what keeps a
  review from touching anything outside its checkout (`docs/adr/0001-threat-model.md`).
  A Docker socket would undo that entirely: the daemon runs as root outside
  the sandbox and will mount any host path into a container, and a network
  would let the reviewer send the repository anywhere. Tests that need a
  database, a message broker, or another service belong to CI, not to the
  review. Make them skip cleanly when the service is absent, so the reviewer
  runs the offline subset and reports the rest as not run, as this
  repository's own `COUNTERPOINT.md` does for lint tooling that needs a
  download; and put the integration run's commands and outcomes in the branch
  notes, which the reviewer checks against the commit rather than takes on
  trust.

## Installation and use

Install the binary, then register it with Claude Code as a stdio MCP server
at user scope, so it is available in every project. On macOS or Linux with
Homebrew:

```bash
brew install SnapdragonPartners/tap/counterpoint
claude mcp add -s user counterpoint -- counterpoint
```

Without Homebrew, download the archive for your platform from the
[releases page](https://github.com/SnapdragonPartners/counterpoint/releases),
check it against `checksums.txt`, put `counterpoint` on `PATH`, and run the
same `claude mcp add` line. The binaries are not signed, so on macOS a
browser download carries the quarantine attribute and Gatekeeper blocks the
first launch; once it is on `PATH`, clear it with
`xattr -d com.apple.quarantine "$(command -v counterpoint)"` (a `curl`
download is not quarantined, and the Homebrew cask clears it for you). With
a Go toolchain,
`go install github.com/SnapdragonPartners/counterpoint/cmd/counterpoint@latest`
also works, but reports the version as `dev`. The registration stores only the
command name, resolved on `PATH` each session, so it is a once-per-machine
step that survives upgrades. If the binary's directory cannot be on `PATH`,
register its absolute path instead. MCP servers start with the client session,
so restart Claude Code after installing a new version.

From a clone of this repository, `make install` builds a version-stamped
binary into `GOBIN`, or `GOPATH/bin` when `GOBIN` is unset, and
`make register` runs the `claude mcp add` line when the name is not yet
registered at user scope and does nothing otherwise, so it is safe to repeat.
The target prints the absolute-path command when `counterpoint` does not
resolve. Registering the installed binary rather than `bin/counterpoint`
keeps the tool stable while the repository is being developed: `make build`
produces the build under test, and `make install` promotes it.

Counterpoint exposes one tool, `review`, taking `repo`, `branch`, `commit`,
`branch_notes`, and an optional `build` flag. It returns the canonical
repository path, the full branch ref, the reviewed commit and its merge base,
the round number, Codex's review text, any bridge warnings, and whether the
result was replayed from state. State lives under the user configuration
directory in a `counterpoint` subdirectory; `COUNTERPOINT_STATE_FILE`
overrides the path for tests and unusual installations. Diagnostics go to
stderr only.

An optional configuration file sits beside the default state file at
`config.json`, or at `COUNTERPOINT_CONFIG_FILE`. Its own location does not
follow `COUNTERPOINT_STATE_FILE`:

```json
{
  "review_effort": "high",
  "state_file": "/absolute/path/state.json",
  "checkout_dir": "/absolute/path/checkouts"
}
```

`review_effort` is one of `low`, `medium`, `high`, or `xhigh` and defaults to
`high`; lower it to spend less model capacity per round, raise it when a
change warrants a deeper read. Environment variables win over the file, which
wins over the built-in defaults. A missing file means the defaults; a file
that exists is validated at startup, so a typo fails before a review rather
than during one. `docs/SPEC.md` has the full contract.

With `build: true` the reviewer gets a disposable checkout of the commit under
the user cache directory (`counterpoint/checkouts`, or
`COUNTERPOINT_CHECKOUT_DIR`) and may build and run tests there, offline. The
checkout is deleted after every review; a per-branch build cache beside it is
kept, so expect one cold build per branch and a test run per round on top of
the review itself, and ask for it when the change warrants that evidence. The
reviewer's tracked-file changes in the checkout, if any, come back as a
warning. Lint tooling that needs a download is not available to the reviewer
and is reported as not run; CI still lints. A workflow's cache is kept
while the workflow has had a build-capable review in the last 72 hours and
is swept by the next build-capable review of any workflow after that, so
a branch resumed after longer pays one cold build; deleting directories
under the scratch root by hand is also safe between reviews.

Prerequisites at review time: a clean worktree checked out at the tip of a
non-primary branch, and a locally authenticated Codex CLI.

To adopt Counterpoint in a project, give the authoring agent the review
workflow in that project's `CLAUDE.md` or `AGENTS.md`.
[WORKFLOW-SAMPLE.md](WORKFLOW-SAMPLE.md) is template language for that: the
roles, the branch-and-review loop up to the human push gate, how to respond
to findings, how to submit each round through the tool, and what branch
notes must contain. Copy it in and edit the placeholders.

A repository can also instruct the reviewer. If the commit under review has a
`COUNTERPOINT.md` at its root, Counterpoint quotes it into every round's
prompt: project conventions, how to build and test, what to prioritize. It is
read from the commit, not the worktree, so it is part of what is reviewed. The
reviewer is told it is author-controlled input that adds guidance but cannot
change the target, the sandbox rules, the severity labels, or the verdict
format. The file must be a regular file of at most 16 KiB (16,384 bytes) of
UTF-8; a symlink, directory, oversized, or non-UTF-8 file fails the review
before Codex starts. This repository's own `COUNTERPOINT.md` is an example.

## Operational notes

- Reviews of different repositories or branches run at the same time, each
  in its own Counterpoint process with its own Codex session. A second
  review of the same branch fails while the first runs with "another review
  of branch ... is in progress" and tells the agent not to retry a call the
  client moved to the background, since it is still running, and otherwise
  to wait for the round to finish and retry. An error saying the state file
  at its path "is busy" means another process is reading or writing the
  shared state file at that instant, which takes moments, or an older
  Counterpoint is holding it for a whole review; the error says to retry in
  ten seconds and to restart Claude Code sessions after installing a new
  version.
- Counterpoint names its threads `Counterpoint review: <repository> <branch>`
  so they are easy to leave alone in the Codex app. Only one process can hold
  a thread at a time. If the thread is open in the app, the next review fails
  and says so; archive the thread in the app and retry. Counterpoint
  unarchives it and takes it over. Do not unarchive it in the app, which would
  open it there again. Opening the thread in the app while a review runs
  fails on the app side.
- The "review turn starting" log line reports `effort`, the configured
  level in force, alongside `reported_effort`, the value the app-server
  echoed back. After a resume the app-server may omit that value, so an empty
  `reported_effort` means it was not reported, not that no effort applied.

## Token usage

`counterpoint --usage` prints the token usage the app-server reported for
each recorded round, per branch, newest first, and exits. It is a report of
what was reported, not a measurement of what a round cost:

```
Counterpoint token usage from ~/.config/counterpoint/state.json
Completed rounds only; failed and interrupted rounds are not recorded.
Every figure is what the app-server reported, not a cost Counterpoint measured.

/Users/you/code/project/.git::refs/heads/feature
  round 3    7de7d024eb12  last report 22,644 tokens  (input 18,234, cached 12,000, output 4,410, reasoning 3,890)
  round 2    3b72c9bbe66f  last report 19,100 tokens  (input 15,900, cached 9,800, output 3,200, reasoning 2,700)
  thread total reported at round 3: 61,744 tokens

1 workflow(s), 2 recorded round(s).
Sum of last reports: 41,744 tokens.
A last report may cover only the final model request of a turn rather than
the whole turn, so treat these as reported figures and the sum as a lower
bound, not as measured round costs.
```

**Rounds reviewed through `codex-cli 0.153.1` report `not recorded`.** That
version runs an inline review in a separate delegate session and drops the
delegate's token-count events before they reach the parent thread, so the
usage never appears on the protocol stream Counterpoint listens to. The
same exclusion is in `0.155.1`. The command and the ledger are in place for
an app-server that does report; a round whose cost is unknown is shown as
unknown rather than as free. `docs/SPEC.md` records the evidence and the
limits of it.

It counts completed rounds only, so a review that failed or timed out is
missing from the totals, and usage is discarded when the ledger record it
belongs to is evicted. Rounds reviewed before usage was tracked print as
not recorded rather than as zero. The figures are the app-server's own and
are labelled as such, because the protocol does not define what a last
report covers. `docs/SPEC.md` has the full contract.

## Development

```bash
make check     # gofmt check, go vet, golangci-lint, go test -race; what CI runs
make build     # bin/counterpoint, the build under test
make install   # versioned binary into GOBIN or GOPATH/bin
make register  # register the installed binary with Claude Code once per machine
make snapshot  # all release artifacts into dist/, unpublished; needs goreleaser
make schema    # regenerate the codex app-server JSON schema into .schema/
make install-hooks  # pre-commit hook that runs make check
```

### Releases

Pushing a `v*` tag runs `.github/workflows/release.yml`, which re-runs
`make check` on the tagged commit and then runs
[GoReleaser](https://goreleaser.com) from `.goreleaser.yaml`: static
binaries for macOS and Linux on amd64 and arm64, tarballs with the license
and docs, `checksums.txt`, release notes from the commit subjects since the
previous tag, and the Homebrew cask in `SnapdragonPartners/homebrew-tap`,
written with the organization secret `HOMEBREW_TAP_TOKEN`. The binaries are
not signed; the cask clears the quarantine attribute after install so
Gatekeeper does not block them. Tag only a commit on the primary branch that
has been reviewed and merged, with an annotated tag: `git tag -a v0.1.1 -m
"v0.1.1"` then `git push origin v0.1.1`. Verify a config change with
`make snapshot` before tagging.

## Name

In musical counterpoint, independent voices retain their own lines while
working against the same material. Here, the author and reviewer do the same.

## License

See [LICENSE](LICENSE).
