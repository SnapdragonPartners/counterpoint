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
- **Reviewer:** Codex. It inspects the supplied commit and relevant repository
  context, reports blocking findings, and eventually approves. It does not edit
  the workspace.
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
2. It runs appropriate verification and creates a local commit.
3. It writes branch notes that identify the round and summarize:
   - the commit and material changes;
   - verification performed and its outcome;
   - how prior review findings were resolved;
   - important decisions or rejected alternatives; and
   - remaining questions, if any.
4. It calls Counterpoint's `review` tool and waits.
5. Counterpoint validates the target, starts or resumes the Codex thread for the
   repository and branch, submits the review request, and waits for completion.
6. Codex reads the commit and repository context and returns either findings or
   explicit approval.
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
  "branch_notes": "Round 8 carried ..."
}
```

Fields:

- `repo` is an absolute path located within a Git worktree.
- `branch` is a local branch name. Counterpoint normalizes it to
  `refs/heads/<branch>`.
- `commit` is any unambiguous commit identifier, resolved immediately to a full
  object ID.
- `branch_notes` is non-empty author-produced handoff text. Counterpoint does
  not attempt to derive or rewrite it.

Successful output:

```json
{
  "repo": "/canonical/path/to/repository",
  "branch": "refs/heads/v2/phase_3/prompt-packs",
  "commit": "404b3a2...full object id...",
  "round": 8,
  "review": "Codex's completed review text"
}
```

`review` is text, not a machine-enforced verdict in the MVP. The authoring agent
and human can understand explicit approval or findings without Counterpoint
introducing a second interpretation layer. A structured verdict may be added
after real usage demonstrates a need.

Errors are returned as MCP tool errors with operational context and without
credentials or unrelated process output.

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
complete state, never truncated JSON.

The same normalized request is idempotent. If repository, branch, resolved
commit, and branch notes hash match the last completed request, Counterpoint
returns the stored completed review rather than creating another Codex turn.
The state therefore also stores the last review text, although it is omitted
from the abbreviated example above.

## Repository validation

Before contacting Codex, Counterpoint must:

1. Require an absolute repository path and resolve symlinks.
2. Ask Git for the worktree root and stable common repository directory.
3. Verify that the supplied local branch exists.
4. Resolve the commit to a full commit object ID.
5. Verify that the commit is reachable from the supplied branch.
6. Refuse the repository's primary branch, determined from local repository
   metadata where possible and otherwise defaulting to `main` and `master`.

Git commands use argument arrays, never shell interpolation. Branch names and
paths are untrusted input even though Counterpoint initially runs locally.

Different worktrees belonging to the same clone should share a repository
identity by using Git's common directory, while Codex receives the canonical
worktree path it must inspect.

## Codex app-server integration

Counterpoint launches one child `codex app-server` process for its own lifetime
using the default JSONL-over-stdio transport. It keeps stderr separate from the
protocol stream.

The implemented protocol subset is:

- `initialize` request and `initialized` notification;
- `thread/start` for a new workflow;
- `thread/resume` after lookup or process restart;
- `turn/start` for the review request;
- `item/agentMessage/delta` and/or completed agent-message items;
- `turn/completed` as the authoritative terminal event; and
- responses declining unexpected approval or user-input requests so no turn
  can deadlock silently.

Counterpoint uses a read-only sandbox, disables network access, and configures
an approval policy that cannot grant writes or elevated execution. Unexpected
requests for additional access fail the review visibly.

The app-server client has one stdout reader. It distinguishes responses,
notifications, and server-originated requests, then dispatches them by request,
thread, and turn ID. It must raise the scanner/reader limit above Go's default
token size because reviews and protocol items may exceed 64 KiB.

If app-server exits during a review, the MVP returns an error. The caller may
retry; idempotency and the stored thread ID prevent a completed review from
being intentionally duplicated. Automatic process restart within the same tool
call is not required.

## Review request

Each review turn identifies the immutable target and includes the branch notes
without summarizing them away. The prompt directs Codex to:

- inspect the supplied commit and relevant repository context itself;
- treat branch notes as the author's claims, not verified facts;
- review only and never modify files;
- carry unresolved findings from earlier rounds until resolved or explicitly
  withdrawn;
- prioritize concrete correctness, robustness, security, and maintainability
  defects over stylistic preferences;
- cite precise files and lines when possible; and
- return actionable findings ordered by severity, or explicit approval when no
  blocking findings remain.

The prompt may be compiled into the binary for the MVP. Configurable prompt
paths and templates are deferred.

## Concurrency

Calls sharing a workflow key are serialized. A second call for the same
repository and branch waits for the first or fails clearly; it never starts a
parallel turn on the same Codex thread.

Supporting concurrent calls for different branches is not an MVP acceptance
requirement. The design must not corrupt state if they occur; a process-wide
state lock is acceptable initially.

## Human gate

Counterpoint has no tools for Git mutation, push, pull-request creation, review
submission, approval, or merge. After Codex approval, the authoring agent must
stop and present the reviewed commit and branch notes to the human.

Prompt instructions reinforce this boundary, but absence of mutating tools is
the primary control.

## Minimum observability

Diagnostic logs go to stderr so they cannot corrupt MCP or app-server protocol
streams. Logs include workflow key, round, request ID, thread ID in abbreviated
form, turn ID, duration, and terminal status. They do not include full branch
notes, full model output, credentials, or environment dumps by default.

## Required tests

Unit tests cover:

- repository and branch normalization;
- rejection of malformed or unreachable commits;
- stable workflow-key construction across worktrees;
- JSON state load, atomic replacement, and malformed-state failure;
- request hashing and idempotent replay;
- JSON-RPC response, notification, and server-request dispatch;
- interleaved app-server events;
- oversized JSONL messages;
- refusal of approval requests; and
- serialization of calls for the same workflow.

An integration test uses a fake app-server subprocess to exercise initialization,
new-thread review, persisted restart, thread resume, and a second review round.
Live Codex tests are manual and require explicit human approval because they use
model capacity and local credentials.

## Acceptance scenario

The MVP is accepted when a clean local demonstration can:

1. Configure Counterpoint as a Claude Code stdio MCP server.
2. Review commit A on a feature branch and return findings.
3. Persist the returned Codex thread association.
4. Stop and restart Counterpoint.
5. Review descendant commit B using only the same repository and branch inputs.
6. Demonstrate from the review that Codex retained round A's context.
7. Return explicit Codex approval.
8. Leave the repository unmodified and unpushed.

## Explicitly deferred

- A resident daemon or network listener.
- Background jobs, polling, cancellation UI, and progress streaming through MCP.
- Multiple simultaneous review conversations on one branch.
- Multiple reviewers or author/reviewer role selection.
- Automatic implementation of Codex findings.
- Structured verdict enforcement or finding databases.
- Configurable prompts, model selection, and per-repository policy files.
- Branch lifecycle management and automatic state garbage collection.
- Remote app-server hosts, containers, and cloud execution.
- Push, pull-request, CI, approval, or merge automation.
- General-purpose orchestration.
