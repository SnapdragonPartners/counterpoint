# Counterpoint MVP

## Purpose

Counterpoint removes the manual copy-and-paste loop between an authoring coding
agent and Codex. It preserves one Codex review conversation across successive
local commits on a branch and stops when the work reaches the human push/PR
gate.

The MVP proves only this loop. It is not a general agent orchestrator.

## Users and roles

- **Authoring agent:** Claude Code initially, or any later MCP client. It writes
  the change, runs verification, commits locally, writes branch notes, invokes
  Counterpoint, and resolves review findings.
- **Reviewer:** Codex. It inspects the supplied branch snapshot and relevant
  repository context, reports blocking findings, and eventually approves. It
  does not edit the workspace.
- **Human:** Owns intent, resolves non-converging disagreements, approves the
  reviewed result, and authorizes push and pull-request creation.
- **Counterpoint:** Owns deterministic plumbing: MCP exposure, workflow lookup,
  Codex process/protocol handling, persistence, validation, and serialization.

## Success criterion

Starting with an MCP client on a local feature branch, a user can complete at
least two review rounds against the same persistent Codex thread, restart
Counterpoint between rounds, resume that thread by repository and branch, and
receive Codex approval without manually transferring either agent's messages.
No Counterpoint operation pushes, creates a pull request, merges, or edits the
reviewed repository.

## Required workflow

1. The authoring agent changes files on a branch other than the repository's
   primary branch.
2. It runs appropriate verification and creates a local commit, leaving the
   worktree clean.
3. It writes branch notes that summarize:
   - the commit and material changes;
   - verification performed and its outcome;
   - how prior review findings were resolved;
   - important decisions or rejected alternatives; and
   - remaining questions, if any.

   Counterpoint owns the round number and injects it into the review request.
   The author does not need to number rounds in the notes; if it does, the
   Counterpoint round is authoritative.
4. It calls Counterpoint's `review` tool and waits.
5. Counterpoint validates the target, acquires the review lock, starts or
   resumes the Codex thread for the repository and branch, starts the review,
   and waits for completion.
6. Codex inspects the branch snapshot and repository context and returns either
   findings or explicit approval.
7. Counterpoint returns that review verbatim enough to preserve file references,
   priorities, and reasoning.
8. The authoring agent either resolves the findings in another local commit and
   repeats the loop, or stops for human approval after Codex approval.

## MCP interface

The MVP exposes exactly one blocking tool over MCP stdio.

### `review`

Input:

```json
{
  "repo": "/absolute/path/to/repository",
  "branch": "v2/phase_3/prompt-packs",
  "commit": "404b3a2",
  "branch_notes": "This commit carries ..."
}
```

Fields:

- `repo` is an absolute path located within a Git worktree.
- `branch` is a local branch name, given either bare (`feature/x`) or as a full
  local ref (`refs/heads/feature/x`). Counterpoint normalizes both to
  `refs/heads/<branch>`. Remote-tracking names, tags, and other ref namespaces
  are rejected.
- `commit` is any unambiguous commit identifier, resolved immediately to a full
  object ID. It must be the current tip of `branch` and the worktree's `HEAD`.
- `branch_notes` is non-empty author-produced handoff text of at most 1 MiB.
  Counterpoint does not attempt to derive or rewrite it; longer notes are
  rejected before any work starts.

Successful output:

```json
{
  "repo": "/canonical/path/to/repository",
  "branch": "refs/heads/v2/phase_3/prompt-packs",
  "commit": "404b3a2...full object id...",
  "base": "9c1f0e4...merge-base object id...",
  "round": 8,
  "review": "Codex's completed review text",
  "warnings": [],
  "replayed": false
}
```

`replayed` is true when an identical completed request was answered from
state without a new Codex turn.

`review` is text, not a machine-enforced verdict in the MVP. The authoring agent
and human can understand explicit approval or findings without Counterpoint
introducing a second interpretation layer. A structured verdict may be added
after real usage demonstrates a need.

`warnings` lists bridge-level events that did not stop the review but that the
caller should know about, such as declined approval requests. The list is
bounded at 32 entries totalling at most 8 KiB; when any are omitted, one
additional final entry outside those caps reports the omitted count.
Counterpoint never splices warnings into Codex's review text.

Errors are returned as MCP tool errors with operational context and without
credentials or unrelated process output.

## Review target

The review target is an immutable branch snapshot, not a single commit's diff.
Before contacting Codex, Counterpoint must establish all of the following:

1. The resolved commit equals the tip of the supplied local branch.
2. The resolved commit equals the worktree's `HEAD`.
3. The worktree is clean: no staged changes, no unstaged changes to tracked
   files, and no untracked files. Ignored files do not count.
4. The base is the merge base between the resolved commit and the primary
   branch. The primary branch is resolved from its local ref when one exists
   and otherwise from its remote-tracking ref.

Codex is asked to review the complete diff from the base through the tip. On
rounds after the first, the request also names the previously reviewed tip so
Codex can concentrate on the delta while still validating the whole branch. If
the previously reviewed tip is no longer an ancestor of the new tip, history was
rewritten; Counterpoint says so and Codex performs a complete review.

Requiring a clean worktree at the tip means the files Codex reads on disk are
exactly the files in the reviewed commit. The prompt still directs Codex to use
Git for history and diffs rather than relying on working-tree state alone.

## Workflow identity and persistence

There may be at most one active Counterpoint workflow for a canonical repository
and branch pair. The workflow key is:

```text
<canonical repository identity>::<full local branch ref>
```

Counterpoint retains the Codex thread ID internally; callers never supply or
receive it.

The state file has a versioned JSON envelope:

```json
{
  "version": 1,
  "workflows": {
    "/canonical/repo::refs/heads/feature/example": {
      "thread_id": "thr_123",
      "last_commit": "full commit object id",
      "last_base": "full merge-base object id",
      "last_request_hash": "sha256 of normalized input",
      "round": 2
    }
  }
}
```

The default location is determined with `os.UserConfigDir` and a Counterpoint
subdirectory. A single environment-variable override may be provided for tests
and unusual installations.

State writes must use a temporary file in the same directory followed by an
atomic rename. A failure must leave either the old complete state or the new
complete state, never truncated JSON. State that fails to parse produces an
error naming the file and is never overwritten.

State records only completed reviews. Counterpoint does not persist in-flight
turns.

The same normalized request is idempotent. The request identity is the
canonical repository, full branch ref, resolved commit object ID, resolved
merge-base object ID, and a hash of the branch notes. If all five match the
last completed request, Counterpoint returns the stored completed review rather
than creating another Codex turn. A changed commit with identical notes is a new
request, and so is an unchanged tip whose merge base moved because the primary
branch advanced, since the reviewed diff differs. The state therefore also
stores the last review text and warnings, although they are omitted from the
abbreviated example above.

## Repository validation

Before contacting Codex, Counterpoint must:

1. Require an absolute repository path and resolve symlinks.
2. Ask Git for the worktree root and common repository directory. Git may report
   the common directory as a relative path; Counterpoint absolutizes it before
   using it as identity.
3. Validate the branch name with `git check-ref-format` and verify that the
   local branch exists.
4. Resolve the commit to a full commit object ID.
5. Apply the review-target checks above.
6. Refuse the repository's primary branch, determined from local repository
   metadata where possible and otherwise defaulting to `main` and `master`.

Git commands use argument arrays, never shell interpolation. Branch names and
paths are untrusted input even though Counterpoint initially runs locally.

Different worktrees belonging to the same clone share a repository identity by
using Git's common directory. When resuming, Counterpoint passes the calling
worktree's canonical path as the thread's working directory so Codex inspects
the right checkout.

## Codex app-server integration

Counterpoint launches a child `codex app-server` process for each review,
using the default JSONL-over-stdio transport, and keeps stderr separate from
the protocol stream.

The child's lifetime is bounded by the review lock: it is started after the
lock is acquired and terminated, with its exit awaited, before the lock is
released. This holds for completed, failed, interrupted, and errored reviews.
Codex keeps per-thread writer locks, so a long-lived child in one Counterpoint
process could block another process from resuming the same thread; ending the
child inside the lock is the ownership handoff. Starting the child costs a few
seconds per review, which is small against observed review durations.

No child runs at MCP startup, so the tool can be listed and inspected when
Codex is missing. If the Codex CLI is absent or unauthenticated, `review`
returns a clear installation or authentication error. Idempotent replays are
served from state without starting a child. Automatic restart of a child that
exits mid-review is not required; the call returns an error.

### Protocol subset

The implemented subset, using the v2 method names, is:

- `initialize` request carrying client name and version, followed by the
  `initialized` notification before any thread call;
- `thread/start` for a new workflow and `thread/resume` for an existing one,
  both configured with the read-only sandbox mode, the `never` approval policy,
  and the worktree path;
- `review/start` with `delivery: "inline"` and a `custom` target carrying the
  compiled review instructions, so the review runs on the persistent thread;
- `item/completed` for the `exitedReviewMode` item, whose `review` field is the
  authoritative review text, with completed `agentMessage` items as a fallback;
- `turn/completed` as the terminal event, honoring `completed`, `failed`, and
  `interrupted` statuses;
- `turn/interrupt` for cancellation; and
- explicit responses to every server-originated request.

A `failed` turn returns the turn error message and, when present, the Codex
error code. An `interrupted` turn returns an error describing why Counterpoint
interrupted it. Neither is persisted as a completed review.

`review/start` is selected over a generic `turn/start` because it carries
Codex's native review-only behavior and a dedicated review result while still
running on the persistent thread. The live acceptance run must confirm that a
later round can see the earlier round's review; if it cannot, the mechanism
falls back to `turn/start` with the same compiled instructions.

### Sandbox and approvals

Counterpoint configures a read-only sandbox with network access disabled and an
approval policy of `never` on both `thread/start` and `thread/resume`. With this
configuration Codex should never ask for additional access. As defense in depth,
every server-originated request still receives an explicit bounded response:

- command execution and file change approvals are answered with `decline`,
  which lets the turn continue, never `cancel`, which would interrupt it;
- permission requests receive an empty grant;
- user input requests receive empty answers; and
- any other server request receives a JSON-RPC error.

Each declined request is recorded in the response `warnings` array and in the
stderr log.

### Model and reasoning effort

The MVP does not select a model. Codex's configured default model applies, so
the reviewer model is whatever the user's Codex configuration names.

Counterpoint does set reasoning effort, so the reviewer runs at a deliberate
level regardless of the user's interactive setting. The MVP hardcodes the
effort as a single named constant, `xhigh`, chosen by DR as the fixed review
policy for the MVP, and passes it as a `model_reasoning_effort` configuration
override on the child command line. This is a fixed choice, not a rule that
Counterpoint selects the highest available level. Effort strings are
model-advertised rather than a fixed enum, and `model/list` reports each
model's supported levels; the installed catalog advertises levels above
`xhigh` on some models, including `ultra`, which enables automatic task
delegation that a reviewer should not adopt implicitly. Changing the constant
is a one-line edit; selecting a level per model from the catalog is deferred.

The effective model and effort reported by `thread/start` or `thread/resume`
are logged alongside the configured effort, because the reported effort is
nullable in the protocol and `thread/resume` has been observed to omit it
while the configured override remained in force. If the configured model does
not accept the constant, the review fails with the app-server's error rather
than retrying at a lower level. Model
selection and per-repository effort settings are deferred.

### Reader

The app-server client has one stdout reader. It distinguishes responses,
notifications, and server-originated requests, then dispatches them by request,
thread, and turn ID. It must raise the scanner/reader limit above Go's default
token size because reviews and protocol items may exceed 64 KiB.

### Protocol compatibility

Counterpoint is developed and tested against a recorded Codex CLI version,
currently `codex-cli 0.153.1`, but does not enforce an exact version. A
development helper regenerates the app-server JSON schema from the installed CLI
into a gitignored directory so protocol claims can be checked against the
version in use. Counterpoint fails clearly when `initialize` or a required
method returns an incompatible response.

## Review request

Each review identifies the immutable target and includes the branch notes
without summarizing them away. The instructions direct Codex to:

- review the complete diff from the named base to the named tip, using Git for
  history and diffs;
- on later rounds, concentrate on the delta since the previously reviewed tip
  while still validating the whole branch, or perform a complete review when
  told that history was rewritten;
- treat branch notes as the author's claims, not verified facts;
- review only and never modify files;
- carry unresolved findings from earlier rounds until resolved or explicitly
  withdrawn, and state the disposition of each prior finding;
- prioritize concrete correctness, robustness, security, and maintainability
  defects over stylistic preferences;
- cite precise files and lines when possible; and
- return actionable findings ordered by severity, or explicit approval when no
  blocking findings remain.

The instructions also tell Codex that it is running non-interactively in a
read-only sandbox: it must not request additional permissions or user input,
should use the best available read-only approach, complete the review
autonomously, and report any material limitation in the review itself. This
guidance lives in the prompt, not in the protocol-level declines.

The instructions include the Counterpoint round number. Branch notes are
delimited clearly as untrusted author input.

The instructions may be compiled into the binary for the MVP. Configurable
prompt paths and templates are deferred.

## Concurrency and cancellation

Reviews are serialized across all Counterpoint processes with a single advisory
file lock held for the entire review, from validation through the state write
and the child's exit.
Multiple MCP clients each spawn their own Counterpoint process, so a
process-local lock is insufficient. Acquisition uses a short bounded wait and
then fails with a clear "another review is in progress" error rather than
queueing behind a full review. Serializing reviews for different branches is an
accepted MVP limitation.

Two fixed phase budgets apply. Setup has sixty seconds covering the
app-server launch, its handshake, and thread start or resume; a stall anywhere
in setup fails the call, closes the child, and releases the lock. The review
turn then has twenty minutes. Lock acquisition (two seconds), Git validation,
persistence, and cleanup (up to five seconds for the turn to interrupt and
five for the child to exit before it is killed) are outside both budgets, so
the budgets are not a bound on the whole call. They sit well below the MCP
client's default idle timeout so Counterpoint fails first with a clear error.
Observed reviews rarely exceed five minutes. Configurable timeouts are
deferred.

On timeout, MCP request cancellation, or closure of Counterpoint's own stdin,
Counterpoint sends `turn/interrupt`, waits briefly for the terminal event, and
returns an error. On shutdown it terminates the child process. A retry after
cancellation starts a new turn on the same thread; the interrupted turn remains
in Codex's history.

## Human gate

Counterpoint has no tools for Git mutation, push, pull-request creation, review
submission, approval, or merge. After Codex approval, the authoring agent must
stop and present the reviewed commit and branch notes to the human.

Prompt instructions reinforce this boundary, but absence of mutating tools is
the primary control.

## Recovery

If `thread/resume` fails for a stored thread ID, Counterpoint fails closed. It
does not start a replacement thread, because silently discarding retained review
context would defeat the product. The error names the workflow key and the state
file so a human can clear the entry deliberately. An explicit reset operation is
deferred until real recovery cases are understood.

## Minimum observability

Diagnostic logs go to stderr so they cannot corrupt MCP or app-server protocol
streams. Logs include workflow key, round, request ID, thread ID in abbreviated
form, turn ID, duration, terminal status, and any declined requests. They do
not include full branch notes, full model output, credentials, or environment
dumps by default.

## Client configuration

Claude Code's per-call MCP tool timeout defaults to many hours, but its idle
timeout for stdio servers aborts a call that produces no response and no
progress notification for thirty minutes. Counterpoint's phase budgets total
twenty-one minutes plus unbudgeted Git and cleanup time, and the MVP does not
send progress notifications, so the thirty-minute default should be kept as
margin rather than lowered. MCP input is bounded to one complete JSON value
per line of at most 6 MiB plus 64 KiB on the wire. The README names the
client settings for users who have lowered them.

## Required tests

Unit tests cover:

- repository and branch normalization, including bare and full-ref branch input
  and rejection of remote-tracking names;
- rejection of malformed commits and of commits that are not the branch tip or
  the worktree `HEAD`;
- rejection of a dirty worktree, including untracked files;
- merge-base resolution against local and remote-tracking primary branches;
- rewritten-history detection when the previous tip is no longer an ancestor;
- stable workflow-key construction across worktrees;
- JSON state load, atomic replacement, and malformed-state failure without
  overwrite;
- request hashing and idempotent replay, including a changed commit with
  identical notes and an unchanged commit with a moved merge base;
- JSON-RPC response, notification, and server-request dispatch;
- interleaved app-server events;
- oversized JSONL messages;
- declining approval, permission, and user-input requests, and surfacing them
  as warnings;
- extraction of the review text from the review-mode item with agent-message
  fallback;
- `failed` and `interrupted` terminal handling;
- turn timeout and cancellation issuing `turn/interrupt`;
- cross-process lock acquisition, including bounded wait and clear failure; and
- child process termination before lock release on every outcome.

An integration test uses a fake app-server subprocess to exercise
initialization, new-thread review, persisted restart, thread resume, and a
second review round. Live Codex tests are manual and require explicit human
approval because they use model capacity and local credentials.

## Acceptance scenario

The MVP is accepted when a clean local demonstration can:

1. Configure Counterpoint as a Claude Code stdio MCP server.
2. Review commit A on a feature branch and return findings.
3. Persist the returned Codex thread association.
4. Stop and restart Counterpoint.
5. Review descendant commit B using only the same repository and branch inputs.
6. Demonstrate retained context: the round-two review states the disposition of
   at least one finding from round one.
7. Return explicit Codex approval.
8. Leave the repository unmodified and unpushed.

## Explicitly deferred

- A resident daemon or network listener.
- Durable completion after MCP-client disconnect, including persistent
  in-flight state and crash reconciliation. This is the first likely extension;
  the request-identity and completed-only persistence rules above are intended
  to make it straightforward.
- MCP progress notifications, which would be the remedy if reviews ever need to
  run longer than the client idle timeout.
- Background jobs, polling, and cancellation UI.
- Per-workflow state files and per-workflow locks.
- Multiple simultaneous review conversations on one branch.
- Multiple reviewers or author/reviewer role selection.
- Automatic implementation of Codex findings.
- Structured verdict enforcement or finding databases. The app-server's
  output-schema option on turn start is a candidate mechanism.
- An explicit thread reset operation.
- Configurable prompts, model selection, per-model reasoning-effort selection
  from the catalog, and per-repository policy files.
- A configurable review timeout.
- Branch lifecycle management and automatic state garbage collection.
- Exact Codex CLI version enforcement.
- Remote app-server hosts, containers, and cloud execution.
- Push, pull-request, CI, approval, or merge automation.
- General-purpose orchestration.
