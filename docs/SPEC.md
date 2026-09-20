+++
title = "Counterpoint Specification"
edit_date = "2026-09-20"
status = "live"
summary = "The accepted contract for Counterpoint as it is: one blocking MCP review tool that hands a local commit to a persistent Codex review thread, what it validates, persists, bounds, and refuses, and where it stops."
+++

# Counterpoint Specification

Status: Accepted (Codex round 1 of `docs/spec-rewrite` and DR, 2026-09-20)

This document is the accepted contract for Counterpoint's current behavior.
It ranks below code and tests, which describe the runtime, and above the
ADRs in `docs/adr/` and the design records in `docs/design/`, which keep the
reasoning and evidence behind decisions and bind only where they do not
contradict it (`CLAUDE.md`, "Project and authority"). When code and this
document differ, the difference is identified and resolved by fixing one or
the other with DR's approval, never silently. A change that alters behavior
or a public contract updates this document in the same change, and the
review of that change is the review of the amendment. A finding settled by
argument rather than by a change is recorded as a dated amendment here when
it governs a specific rule, or as an ADR when it applies more broadly
(`CLAUDE.md`, "Responding to findings").

Product behavior lives here; operating rules for working in the repository
live in `CLAUDE.md`; orientation, installation, and client settings live in
`README.md`. Reviewer instructions for this repository are in
`COUNTERPOINT.md`, quoted into every review by the mechanism described
under "Project instructions".

## Purpose and scope

Counterpoint removes the manual copy-and-paste loop between an authoring
coding agent and Codex. One Codex review conversation is preserved across
successive local commits on a branch, each round is a review of an immutable
commit, and the loop stops when the work reaches the human push and
pull-request gate.

Counterpoint is a Go executable that serves one blocking tool over MCP
stdio, launches a local `codex app-server` child per review, and persists
the thread association in a small JSON state file. It is not a general
agent orchestrator: it does not schedule work, plan across agents, automate
a forge, or implement findings. Additions of that kind need an accepted
design first (`CLAUDE.md`, "Scope discipline").

### Roles

- **Authoring agent:** Claude Code initially, or any MCP client. It writes
  the change, runs verification, commits locally, writes branch notes, calls
  the review tool, and resolves findings.
- **Reviewer:** Codex, in its review mode, on a sandboxed thread. It inspects
  the branch snapshot and repository, reports findings, and eventually
  approves. It never edits the reviewed repository.
- **Human:** owns intent, resolves disagreements the agents do not converge
  on, accepts the reviewed result, and authorizes push and pull-request
  creation.
- **Counterpoint:** owns the deterministic plumbing: MCP exposure, target
  validation, workflow lookup, the Codex process and protocol, persistence,
  serialization, and time bounds.

### The loop

1. The authoring agent changes files on a branch other than the repository's
   primary branch, verifies the change, and commits locally, leaving the
   worktree clean.
2. It writes branch notes: the commit and material changes, verification
   performed and its outcome, how prior findings were resolved, decisions
   and rejected alternatives, and open questions. Counterpoint owns the
   round number and states it in the prompt; a round number in the notes is
   not authoritative.
3. It calls `review` and waits.
4. Counterpoint validates the target, takes the workflow's lock, starts or
   resumes the Codex thread for the repository and branch, runs the review,
   records the completed round, and returns the review text with any bridge
   warnings.
5. The agent fixes findings in a further commit and repeats, or, after
   approval, stops for the human. Counterpoint has no tool that pushes,
   opens a pull request, approves, or merges.

## Command line

`counterpoint` with no arguments serves MCP on stdin and stdout until the
client disconnects or the process receives `SIGINT` or `SIGTERM`, either of
which cancels every active review so the Codex turn is interrupted and the
child reaped before the server stops. `counterpoint --version` prints the
version, injected at build time, and exits. Positional arguments are an
error. Stdout carries MCP protocol data only; every diagnostic goes to
stderr.

## The review tool

The server exposes exactly one tool, `review`, over MCP stdio. Input:

```json
{
  "repo": "/absolute/path/inside/the/worktree",
  "branch": "feature/x",
  "commit": "404b3a2",
  "branch_notes": "This commit carries ...",
  "build": false
}
```

- `repo`: an absolute path located inside a Git worktree; it may point at a
  subdirectory. Symlinks are resolved.
- `branch`: a local branch, bare (`feature/x`) or as a full local ref
  (`refs/heads/feature/x`); both normalize to `refs/heads/<name>`. Any other
  ref namespace, a remote-tracking name, and a name beginning with a dash
  are rejected.
- `commit`: any unambiguous commit identifier not beginning with a dash,
  resolved immediately to a full object id. It must be the branch tip and
  the worktree's `HEAD` (see "Review target").
- `branch_notes`: non-empty author text of at most 1 MiB. It is quoted into
  the prompt unchanged apart from trailing newlines and is never derived,
  summarized, or rewritten by Counterpoint. Longer notes fail before any
  work starts.
- `build`: optional, default false. True asks for a build-capable review in
  a disposable checkout (see "Build-capable reviews"). It is part of the
  request identity, so the same commit and notes in the other mode is a new
  round, not a replay.

All four text fields are required. Output on success:

```json
{
  "repo": "/canonical/worktree/path",
  "branch": "refs/heads/feature/x",
  "commit": "<full object id>",
  "base": "<merge-base object id>",
  "round": 8,
  "review": "Codex's review text, verbatim",
  "warnings": [],
  "replayed": false
}
```

- `review` is text, not a machine-enforced verdict. The prompt asks the
  reviewer to label findings P0, P1, or suggestion and to approve the commit
  by its full object id when nothing blocks; the authoring agent and the
  human read that directly. Counterpoint adds no second interpretation.
- `warnings` lists bridge-level events that did not stop the review but the
  caller should know about: declined server requests, an unarchived thread,
  an unnamed thread, or tracked files changed in a disposable checkout. The
  list is bounded at 32 entries and 8 KiB in total; when entries are
  omitted, one final entry outside those bounds reports the count. Warnings
  Counterpoint raises itself are fixed strings; identifiers quoted from
  server requests are truncated to 64 characters. Warnings are never spliced
  into the review text.
- `replayed` is true when an identical completed request was answered from
  state without a Codex turn (see "Request identity and replay").

Errors are MCP tool errors carrying operational context and a request
correlation id, without credentials or unrelated process output. Each
tool call is described to the client as blocking for up to sixty seconds
of setup plus a twenty-minute review turn, with a progress notification
every thirty seconds when the request carries a progress token (see "Time
bounds").

MCP input is read one line at a time, and each line must be one complete
JSON value of at most 6 MiB plus 64 KiB, which fits maximal branch notes
after JSON escaping plus framing. A longer line, or a value spread across
lines, ends the session rather than being buffered.

## Review target

The target is an immutable branch snapshot: the whole diff from the merge
base with the primary branch to the branch tip, not one commit's diff.
Before anything else, Counterpoint establishes, in this order:

1. The repository path is absolute and inside a Git worktree. Git reports the
   worktree root and the common Git directory; a relative common directory
   is anchored at the queried path, and both are canonicalized.
2. The branch name normalizes as above, passes `git check-ref-format`, and
   exists as a local branch. A name that exists only as an origin
   remote-tracking branch is reported as such.
3. The primary branch is known and is not the branch under review. Its name
   is the branch that `origin`'s `HEAD` symbolic ref points at when that is
   set, otherwise the first of `main` and `master` that exists locally or as
   an `origin` remote-tracking branch. The ref diffed against is the local
   branch when it exists and otherwise the remote-tracking branch.
4. The commit resolves to a commit object and equals the branch tip.
5. The commit equals the worktree's `HEAD`.
6. The worktree is clean: no staged changes, no unstaged changes to tracked
   files, and no untracked files. Ignored files do not count.
7. A merge base between the commit and the primary ref exists; it is the
   `base` of the review.

Requiring a clean worktree at the tip means the files the reviewer reads on
disk in a read-only review are exactly the files in the commit; the prompt
still directs it to use Git for history and diffs rather than relying on
working-tree state alone.

On rounds after the first, the previously reviewed tip is named so the
reviewer can concentrate on the delta while still validating the whole
branch. If that tip is no longer an ancestor of the new tip, or no longer
exists, history was rewritten: the prompt says so and asks for a complete
review, and earlier verdicts are quoted with that caveat.

Different worktrees of one clone share a repository identity through the
common Git directory, and each review passes its own canonical worktree
path to the thread so the reviewer inspects the right checkout.

Every Git and Codex invocation uses an argument array; nothing passes
through a shell. Paths, refs, commit strings, notes, state, model output,
and child-process messages are untrusted input at every boundary. Git runs
with a pinned `C` locale, terminal prompts disabled, optional locks off,
and the environment variables that would redirect it away from the supplied
repository (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_COMMON_DIR`,
`GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_NAMESPACE`)
removed. Captured Git output is bounded (1 MiB of stdout, 64 KiB of stderr,
512 bytes quoted into an error); a dirty-worktree listing that exceeds the
bound still proves the worktree dirty.

## Project instructions

When the commit under review has a `COUNTERPOINT.md` at its root, its text
is quoted into every round's prompt between the rules and the branch notes,
delimited like the notes and introduced as author-controlled input. The
reviewer applies its project-specific conventions, build and test guidance,
and review priorities, but it cannot change the target, the sandbox rules,
the severity labels, or the verdict format, cannot grant permissions or
request input, and cannot excuse a finding; the prompt wins on conflict.

The file is read from the commit object, never from a worktree or the
disposable checkout, so it is part of the immutable target and covered by
the request identity through the commit. It must be a blob in mode `100644`
or `100755` of at most 16 KiB of valid UTF-8. A symbolic link, submodule,
directory, oversized, or non-UTF-8 file fails the request before the
app-server is started, with no content echoed in the error, and the text is
never truncated; only trailing newlines are removed. A blank file is treated
as absent.

## Workflow identity and state

A workflow is one repository and one branch. Its key is the canonical
repository identity, the absolute symlink-resolved common Git directory,
joined to the full local branch ref:

```text
<common Git directory>::refs/heads/<branch>
```

Counterpoint retains the Codex thread id internally; callers never supply or
receive it.

### State file

The state file lives at `os.UserConfigDir()/counterpoint/state.json`, or at
the absolute path in `COUNTERPOINT_STATE_FILE`, the one override, for tests
and unusual installations. It is a versioned JSON envelope:

```json
{
  "version": 2,
  "workflows": {
    "/canonical/repo/.git::refs/heads/feature/x": {
      "thread_id": "thr_123",
      "last_commit": "<full object id>",
      "last_base": "<full object id>",
      "last_request_hash": "<sha256 of the normalized request>",
      "round": 3,
      "last_review": "the round-three verdict",
      "last_warnings": ["..."],
      "history": [
        {"round": 1, "commit": "<id>", "base": "<id>", "omitted": "too-large"},
        {"round": 2, "commit": "<id>", "base": "<id>", "review": "the round-two verdict"}
      ]
    }
  }
}
```

State records only completed reviews; in-flight turns are never persisted.
Writes go to a temporary file in the same directory, synced, renamed over
the old file, and the directory synced, so a failure leaves either the old
complete state or the new complete state. A file that fails to parse, or
whose version is outside the readable range, produces an error naming the
file and is never overwritten. The file is read through a 16 MiB bound and
a save that would exceed the same bound is refused before it touches the
file. Version 1 files, which predate the history ledger, load as version 2
with empty history and are rewritten as version 2 on the next save; a
version 1 binary rejects a version 2 file rather than silently dropping the
ledger.

A stored workflow must be a complete record of a finished review: a thread
id, full lowercase object ids for commit and base, a request hash, a review,
and a round of at least one that can still be incremented. Anything less
can only come from corruption or a foreign writer and fails the review
without starting a replacement thread, since that would silently discard
review context.

### Review ledger

`history` is the ledger of completed rounds before the one in
`last_review`, oldest first. Codex's review mode gives the reviewer no
memory of earlier rounds, so the ledger is what later prompts quote back
(`docs/design/review-ledger.md`). A record keeps its verdict verbatim when
it is at most 24 KiB and otherwise a placeholder whose `omitted` is
`too-large`; a verdict is never truncated. At most two records are kept, and
the records plus the newest review may quote at most 48 KiB, so three rounds
are visible; older rounds are evicted whole and disclosed to the reviewer by
count. A ledger that breaks these invariants, including a record whose
`commit` or `base` is not a full lowercase object id or whose rounds are not
a contiguous run ending just before `round`, is refused before any of it
reaches a prompt, and the error never echoes stored text.

Under size pressure the ledger yields before a completed review is lost.
When the state file would exceed its bound, Counterpoint evicts history one
record at a time, the oldest record of whichever workflow's history holds
the most bytes, until the save fits, never touching the replay fields, so
every workflow keeps its newest verdicts as long as possible. A save that
still fails once no history remains reports the review as completed but
unsaved; the reviewer's turn is not repeated.

### Request identity and replay

The identity of a request is the repository identity, the full branch ref,
the resolved commit, the resolved merge base, the branch notes, and the
build flag, hashed with each field length-prefixed. When the hash equals
the workflow's `last_request_hash`, the stored review and warnings are
returned with `replayed: true` and no child is started. A changed commit
with identical notes is a new request, and so is an unchanged tip whose
merge base moved because the primary branch advanced, since the reviewed
diff differs. A read-only request hashes exactly as it did before the build
flag existed, so records written by earlier versions still replay.

## Locks and concurrency

Reviews are serialized per workflow, and reviews of different workflows run
concurrently, so several projects can advance at once
(`docs/design/per-workflow-locks.md`). Each MCP client spawns its own
Counterpoint process, so every lock is an advisory file lock, released by
the operating system if the process dies:

- The **workflow lock**, one file per workflow in a `locks` directory
  beside the state file, named by the first 16 hex characters of the
  workflow key's SHA-256, is held for the entire review: from the first
  state read through validation, the disposable checkout, the child's
  lifetime, and the final state write, until after the child has exited.
  Acquisition waits two seconds and then fails with "another review of
  branch ... is in progress" rather than queueing behind a full review. The
  error tells the calling agent not to retry a call its client moved to the
  background, since that round is still running and will deliver its
  result, and otherwise to wait for the round to finish and retry no more
  often than once a minute.
- The **state lock**, beside the state file, guards the file and is held
  only around each read or read-modify-write, which runs no Git or Codex:
  once at the start to copy the workflow's record and once at the end to
  re-read the file, record the completed round, evict history if needed,
  and save. The start wait is two seconds; the final save waits up to thirty
  seconds, bounded by the request context, because the holder may be another
  review finishing at the same moment. Contention fails with "the state file
  ... is busy", telling the caller to retry in ten seconds and, if it
  persists, to restart sessions still running an older Counterpoint.
- The **scratch lock**, on each workflow's directory under the scratch root,
  is held from before a disposable checkout is prepared until after it is
  removed (see "Build-capable reviews").

Lock order is the workflow lock first, then the scratch lock, with the state
lock a leaf never held while acquiring another. The record copied at the
start stays current through the review because the workflow lock excludes
every other round of that workflow. At the final save the record is
reconciled with a fresh read: other workflows' records are carried as read;
this workflow's history is rebuilt from the fresh record, so another
review's size-pressure eviction is honored rather than resurrected; and any
change to this workflow's replay fields means a writer bypassed the lock, so
the save is refused rather than overwriting.

## The Codex session

Counterpoint launches a child `codex app-server` for each review that is
not a replay, speaks JSON-RPC over JSONL on its stdin and stdout, and keeps
the child's stderr separate from the protocol stream. The child is started
after the workflow lock is taken and terminated, with its exit awaited,
before the lock is released, on every outcome. Codex keeps a per-thread
writer lock, so ending the child inside the workflow lock is the ownership
handoff between processes. No child runs at MCP startup, so the tool can be
listed when Codex is missing; an absent or unauthenticated Codex CLI is
reported when a review is attempted. A child that exits mid-review fails the
call; there is no automatic restart.

### Protocol subset

Using the v2 method names:

- `initialize`, carrying the client name `counterpoint` and its version,
  then the `initialized` notification before any thread call. A response
  missing the handshake's required fields is reported as incompatible.
- `thread/start` for a new workflow, `thread/resume` for a known one, both
  with the `never` approval policy, the sandbox mode for the round, and the
  working directory: the caller's worktree for a read-only round or the
  disposable checkout for a build-capable one. A thread may alternate
  between the two across rounds. The response is validated: the thread id
  must be present and, on resume, the one requested; the effective working
  directory, approval policy, and sandbox must be exactly what was asked
  for (see "Sandbox"); anything else fails closed.
- `thread/name/set` after `thread/start`, naming the thread
  `Counterpoint review: <worktree basename> <branch>` for the Codex UIs. A
  refusal is logged and reported as a fixed-string warning, not a failure.
- `thread/unarchive` once, when `thread/resume` is refused by the server,
  followed by one more `thread/resume` (see "Recovery").
- `review/start` with inline delivery and a custom target carrying the
  compiled instructions, so the review runs on the persistent thread.
- `item/completed` for the `exitedReviewMode` item, whose `review` field is
  the authoritative review text. When no such item arrives, the completed
  `agentMessage` items are the review, and failing that the streamed message
  deltas. The fallbacks are aggregates bounded at 16 MiB; an overflowed
  aggregate is an error, never a truncated review.
- `turn/completed` as the terminal event, honoring `completed`, `failed`,
  and `interrupted`. A `failed` turn returns the turn's error message and,
  when present, the Codex error code; an `interrupted` turn returns the
  reason Counterpoint interrupted it. Neither is persisted.
- `turn/interrupt` for cancellation, with five seconds' grace for the
  terminal event.
- An explicit bounded response to every server-originated request.

`review/start` is used rather than `turn/start` because it carries Codex's
native review behavior and a dedicated review result. It runs the review in
a fresh sub-thread that sees only the compiled instructions, so cross-round
context comes from the ledger quoted into the prompt, not from the thread;
the thread is the audit log the Codex UIs show and the anchor for locking
and recovery.

### Sandbox

Every thread is started or resumed with network access disabled and the
`never` approval policy. A read-only round uses the read-only sandbox on the
caller's worktree. A build-capable round uses the workspace-write sandbox
on the disposable checkout, with configuration overrides on the child's
command line: the workflow's cache and temp directories as the only extra
writable roots, both implicit temp roots (`/tmp` and `$TMPDIR`) excluded so
a repository under either stays read-only, and `TMPDIR` plus
`COUNTERPOINT_CACHE_DIR` set for the reviewer's commands. The effective
policy the thread call reports must match exactly: type, network off, both
temp roots excluded, the writable roots equal to those two directories, and
the working directory equal to the checkout. Counterpoint sets no
language-specific variables; the prompt recommends `GOCACHE` under the cache
directory and `GOPROXY=off` as the Go example. There is no Docker or other
service access and no switch to add one: the reviewer acts on untrusted
input, and the sandbox is what keeps a review inside its checkout
(ADR 0001).

As defense in depth, every server-originated request still receives an
explicit bounded response: command and file-change approvals are
answered `decline`, which lets the turn continue, never `cancel`, which
would interrupt it; the legacy approval methods, whose response shape is a
`decision` object rather than a string, are answered with a `denied`
decision carrying a rejection message; permission requests receive an
empty grant; user-input requests receive empty answers; and any other
request receives a JSON-RPC method-not-found error. Each is recorded as a
warning and in the log.

### Model and reasoning effort

Counterpoint does not select a model; Codex's configured default applies.
It does fix the reasoning effort at `xhigh`, passed as a
`model_reasoning_effort` override on the child's command line, so the
reviewer runs at a deliberate level regardless of the user's interactive
setting. This is a fixed policy chosen by DR, not a rule that the highest
advertised level is used: the catalog advertises levels above `xhigh` on
some models, including one that enables automatic delegation a reviewer
should not adopt implicitly. If the configured model rejects the constant,
the review fails with the app-server's error rather than retrying lower.
The effective model and effort the thread call reports are logged beside the
configured effort, because the reported effort is nullable and
`thread/resume` has been observed to omit it while the override remained in
force.

### Transport

The client has one stdout reader that distinguishes responses,
notifications, and server-originated requests and dispatches them by
request, thread, and turn id, with a 16 MiB bound on one JSONL line in
either direction; a single writer goroutine with a bounded outbox of 64
messages, so a child that stops reading fails callers through their
contexts instead of blocking them on the pipe.

Counterpoint is developed against a recorded Codex CLI version, currently
`codex-cli 0.153.1`, and does not enforce an exact version; `make schema`
regenerates the app-server schema from the installed CLI so protocol claims
can be checked. An incompatible response to `initialize` or a required
method fails the call clearly.

## Build-capable reviews

With `build: true`, after validation and inside the workflow lock,
Counterpoint prepares a disposable checkout and makes it the thread's
working directory instead of the user's worktree
(`docs/design/disposable-checkout.md`):

1. The scratch root is `os.UserCacheDir()/counterpoint/checkouts`, or the
   absolute path in `COUNTERPOINT_CHECKOUT_DIR`. It is canonicalized before
   anything is created, and the review fails if it lies inside the
   repository's worktree or common Git directory or contains either, so
   scratch and cache can never land inside the reviewed repository.
2. One directory per workflow, named by the first 16 hex characters of the
   workflow key's SHA-256, holds `checkout/` and `tmp/`, both recreated
   every round; `cache/`, kept between rounds; an empty `hooks/` directory;
   a `used` stamp; and the scratch lock file. The directory and the cache
   are checked not to be symlinks, since the cache persists and is handed
   to the reviewer as a writable root.
3. The checkout is a shared, no-checkout clone of the common Git directory
   with the commit checked out detached, made with the user's global and
   system Git configuration ignored, no templates, and hooks pointed at the
   empty directory, so a global hooks path, a post-checkout hook, or a
   filter such as Git LFS named by a tracked `.gitattributes` cannot run
   before the sandbox exists. The clone reads objects through an alternates
   link and writes nothing into the source repository. Leftovers from a
   crashed round are replaced.
4. `tmp/` is the reviewer's `TMPDIR`. It sits beside the checkout, not
   inside it, because a temp directory under a Git worktree is discovered as
   part of that repository.
5. After the turn, tracked files in the checkout are compared with the
   commit. Any change, whether a lockfile rewrite or an edit despite
   instructions, is reported as a warning because the reviewer's results may
   then not describe the commit; untracked build output is expected. A
   failed comparison is reported the same way.
6. On every exit path the checkout, temp, and hooks directories are removed
   and the cache is kept. Removal is confined to direct children of the
   workflow directory and never follows a symlink.

Each build-capable review stamps its workflow directory as used and sweeps
the root (`docs/design/cache-sweep.md`): a workflow unused for 72 hours
loses its cache, checkout remnants, temp, hooks, and stamp, leaving the
directory and its lock file as a tombstone. A workflow's lock file is the
proof that Counterpoint made the directory; entries in use elsewhere are
skipped without waiting; lookalike and foreign entries are ignored;
removal is descriptor-relative, never follows a link, and refuses
directories on another device; a failed removal is logged and does not stop
the sweep or fail the review.

The clean-worktree requirement is unchanged. The reviewer does not read the
user's checkout in this mode, so the author's only obligation during a
build-capable review is not to rewrite the branch.

## The review prompt

The instructions compiled into the binary identify the immutable target and
carry the notes without summarizing them. Every prompt begins with a
headline naming the round and branch and tells the reviewer it is running
non-interactively in a sandbox: read-only, never modifying files, refs, or
the index; or, for a build-capable round, that its working directory is a
disposable checkout where building and testing are allowed, where the cache
directory and its variable are, that lint tooling is probably unavailable
and should be reported as not run, and that the original repository is
read-only and not the place to run anything. In both modes it must not
request permissions or input, must complete autonomously, and must report
any material limitation in the review itself.

The prompt then names the worktree, the checkout when there is one, the
branch, the commit, the primary branch and ref, the merge base, and the Git
commands for the complete branch diff and its history. Round context says
this is the first round, names the previously reviewed tip and the delta to
concentrate on, or says history was rewritten and asks for a complete
review with earlier findings still standing until resolved or withdrawn.

Earlier rounds, when any are retained, are quoted next: each verdict
verbatim between delimiters it cannot forge, introduced as this reviewer's
own historical output and untrusted input whose approvals are not binding,
to be re-validated against the current commit, with checkout paths mapped
to repository paths; a placeholder for a verdict that was too large to
retain; and a count of rounds that were evicted, whose findings' disposition
the current notes carry.

The rules ask the reviewer to inspect the commit with Git rather than the
working tree alone; to treat the notes as claims to verify; to prioritize
concrete correctness, robustness, security, and maintainability defects
over style and label findings P0, P1, or suggestion; to cite files and
lines; and to return findings ordered by severity with reasoning, or to say
explicitly that nothing blocks and approve the commit by its full object
id. The project instructions follow when the commit has them, then the
branch notes, delimited and marked as untrusted.

Delimiters are `<<<LABEL>>>` and `<<<END LABEL>>>`; when the quoted text
contains either, a tag derived deterministically from the text's SHA-256 is
appended and lengthened until neither marker occurs in it, so the prompt is
reproducible and the text cannot forge its own end.

## Time bounds

Two fixed phase budgets apply inside a call, both of awake time, since Go's
timers stop while the machine sleeps:

- **Setup**, sixty seconds, covers launching the app-server, its handshake,
  thread start or resume including any unarchive and naming, and the
  sandbox validation. A stall anywhere in setup fails the call, closes the
  child, and releases the workflow lock.
- **The review turn**, twenty minutes, from `review/start` to the terminal
  event.

Lock acquisition, Git validation, the disposable checkout, persistence, and
cleanup (up to five seconds for the turn to interrupt and five for the
child to exit before it is killed) are outside both budgets, so the budgets
are not a bound on the whole call. Observed reviews rarely exceed five
minutes. The budgets are not configurable.

For the life of a call, from the request's arrival to its result,
Counterpoint sends `notifications/progress` every thirty seconds carrying
the request's progress token and a count of heartbeats sent, so the client's
idle timeout does not fire on a review that is merely slow; a request
without a token gets no heartbeats and a log line saying so. At the same
cadence it measures the call's silence toward the client, the wall-clock
time since the last heartbeat sent or since the request when none was, and
ends the call once that reaches the client's default idle timeout of thirty
minutes less one interval, in whatever phase the call is: the turn is
interrupted, the child reaped, the lock released, and the tool error names
the silence and its reason. A review that has already completed and been
recorded when the bound is reached is returned as a success, since the next
identical request would replay it. Every client check before Counterpoint's
next tick sees at most the silence that tick measures, so the client's
timer cannot fire first. With heartbeats, silence grows only while the
machine sleeps: a sleep shorter than twenty-nine minutes is survived with
the budgets intact, and a longer one ends the call within thirty seconds
of wake. Without a token the bound applies to the call's wall-clock length,
sleep or not (`docs/design/sleep-survival.md`).

On a budget, an MCP cancellation, closure of Counterpoint's stdin, or
process termination, Counterpoint sends `turn/interrupt`, waits briefly for
the terminal event, and returns an error; nothing is persisted, and a retry
starts a new round on the same thread, the interrupted turn remaining in
Codex's history. A client that gives up without cancelling, as Claude Code's
idle timeout does, does not end the review; the silence bound is what keeps
such a review from running on. After a sleep long enough to carry the
silence past the client's timeout in one step, the client may abort on wake
before Counterpoint's next tick and discard the error it is sent; the lock
is free within about forty seconds of wake and a retry starts a fresh
round.

## Recovery

Codex allows one writer per thread. The Codex app holds the writer while a
thread is open there and offers no way to close a thread, only to archive
or delete it; archiving releases the writer. Counterpoint therefore treats
an archived thread as the human's handoff: when the app-server refuses
`thread/resume`, it calls `thread/unarchive` once and, if that succeeds,
resumes again. Unarchiving is not idempotent and itself needs the writer,
so it fails for a thread that is not archived or that another process
holds, and the original refusal stands. The retry does not inspect error
text. A refusal that is a transport or process failure rather than an
app-server answer gets no unarchive attempt, and neither does a review
whose context has already ended, so an aborted call never changes a
thread's archival state. A successful unarchive is reported as a warning and
persisted with the round.

If the thread still cannot be resumed, Counterpoint fails closed: it does
not start a replacement thread. The error names the workflow key and the
state file and tells the human to archive the thread in Codex and retry, or
to remove the workflow from the state file if the thread no longer exists.
Unarchiving in the app is the wrong move, since it reopens the thread there
and holds the writer again. Recovery never deletes or overwrites another
workflow's state.

## Human gate

Counterpoint has no tool for Git mutation, push, pull-request creation,
review submission, approval, or merge. After Codex approves, the authoring
agent stops and presents the reviewed commit and branch notes to the human.
The prompt reinforces the boundary; the absence of mutating tools is the
control.

## Observability

Diagnostics go to stderr so they cannot corrupt the MCP or app-server
streams. Each review is logged with a request correlation id, workflow key,
round, abbreviated thread id, turn id, configured and reported model and
effort, duration, terminal status, warnings count, and any declined
request. Logs do not include branch notes, model output, credentials, or
environment dumps by default.

## Limits

Every fixed bound in the runtime, in one place. The constants are named in
the code; none is configurable.

| Bound | Value |
| --- | --- |
| `branch_notes` | 1 MiB |
| One MCP request line on the wire | 6 MiB + 64 KiB |
| One app-server JSONL message, either direction | 16 MiB |
| Review text assembled from messages or deltas | 16 MiB |
| Queued outbound app-server messages | 64 |
| Warnings per review | 32 entries, 8 KiB, plus one omitted-count entry |
| Identifier quoted in a warning | 64 characters |
| `COUNTERPOINT.md` | 16 KiB, UTF-8, regular file |
| State file | 16 MiB |
| Ledger records before the newest review | 2 |
| One ledger record's verdict | 24 KiB, else a placeholder |
| Verdict text quoted into a prompt | 48 KiB |
| Setup budget | 60 s awake |
| Review turn budget | 20 min awake |
| Grace for `turn/interrupt`, then for child exit | 5 s each |
| Progress heartbeat and silence check | every 30 s |
| Silence toward the client that ends a call | 29 min 30 s wall-clock |
| Workflow, scratch, and start-of-review state lock wait | 2 s |
| Final-save state lock wait | 30 s |
| Unused build cache retention | 72 h |
| Git stdout captured; stderr captured; stderr quoted | 1 MiB; 64 KiB; 512 B |
| Lock file and scratch directory name | 16 hex characters of SHA-256 |

Environment variables Counterpoint reads: `COUNTERPOINT_STATE_FILE` and
`COUNTERPOINT_CHECKOUT_DIR`, both absolute paths. Variables it sets for the
reviewer's commands in a build-capable review: `TMPDIR` and
`COUNTERPOINT_CACHE_DIR`.

## Testing

`make check` runs gofmt, `go vet`, `golangci-lint`, and `go test -race`,
and is what CI runs. Automated tests exercise the runtime against a fake
app-server subprocess that mirrors `codex-cli 0.153.1`'s protocol answers
and echoes the sandbox policy it parses from its own command line; live
Codex runs are manual and need DR's approval because they spend model
capacity and use local credentials. The test files are the inventory of
cases. What the suite must protect, and what a change that weakens any of
these must restore, is:

- validation rejects every target this document says it rejects, before
  any child is started;
- a completed review is persisted atomically, replayed only on an identical
  request, and never overwritten by a save that would clobber another
  writer's record or resurrect evicted history;
- the ledger's retention bounds, placeholders, migration, and refusal of
  every malformed shape, and verbatim quoting with forged delimiters
  defeated;
- the locks' spans, waits, and errors, including two workflows in flight at
  once and a second round of one workflow refused;
- the child's lifetime inside the workflow lock on every outcome, and an
  explicit response to every server-originated request;
- the effective sandbox validated exactly, the disposable checkout isolated
  from hooks and filters, removed on every exit path, and swept only when
  stale and unlocked;
- the budgets, heartbeats, and silence bound behaving as "Time bounds"
  states, including a sleep short of the bound survived and a longer one
  ending the call;
- interleaved and oversized app-server messages, and every terminal status.

A regression test that cannot fail for the defect it names is not a
regression test (`CLAUDE.md`, "Security and testing").

## Out of scope

Not implemented, and not to be added without an accepted design:

- A resident daemon or network listener; durable completion after the MCP
  client disconnects, including persistent in-flight state and crash
  reconciliation, which the completed-only persistence and request-identity
  rules are meant to make straightforward.
- Background jobs, polling, or a cancellation UI.
- Per-workflow state files; multiple review conversations on one branch;
  multiple reviewers or role selection.
- Automatic implementation of findings; structured verdict enforcement or
  finding databases.
- An explicit thread reset operation.
- Configurable prompts, timeouts, or budgets; model selection; per-model
  effort selection from the catalog; per-repository policy beyond
  `COUNTERPOINT.md`.
- Branch lifecycle management, state garbage collection, or a size bound on
  the scratch root beyond the 72-hour sweep.
- Exact Codex CLI version enforcement.
- Remote app-server hosts, containers, Docker access, or cloud execution.
- Push, pull-request, CI, approval, or merge automation; scheduling; general
  orchestration.

Deferred product work is tracked in GitHub Issues, not here.

## History

This document replaced `docs/MVP.md`, the specification that carried the
project from its design review through the accepted MVP and the seven
features that followed it. The last commit containing that file is
`43670ef` on `main`; its section structure and the acceptance scenario it
recorded are recoverable from there. The design records in `docs/design/`
now point here for the contract; their "Documentation" sections still
describe the edits each made to that file at the time, as history.
