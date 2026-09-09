# Per-workflow review locks

Design record for
[issue 30](https://github.com/SnapdragonPartners/counterpoint/issues/30).

Status: Accepted (Codex round 2 of `feat/per-workflow-locks` and DR,
2026-09-08)

Settled between DR, Claude, and Codex on 2026-09-08 before the code was
written; the implementation on the same branch follows this document. Once it
lands, the contract lives in `docs/MVP.md` and this file keeps the reasoning
and the rejected alternatives.

## Problem

The MVP serializes every review on one advisory file lock, `state.json.lock`
beside the state file, held from validation through the Codex turn, the state
write, and the child's exit: up to about twenty-one minutes. A second review
from any repository fails after two seconds with "another review is in
progress". The specification accepted this for the MVP and deferred
per-workflow locks.

DR's stated use case for Counterpoint is several projects advancing at the
same time with no human involvement except at the gates. On 2026-09-08 a
build-capable review of one repository blocked a review of another for the
length of its turn. Cross-repository contention is therefore a defect, not a
limitation, and this record removes it before v1.

## Invariants that stay

From `CLAUDE.md`:

- At most one active review turn may mutate a workflow's state. Two rounds on
  the same repository and branch must still be refused, and refused fast.
- State updates are atomic and serialized on a key matching the shared
  resource. The state file is one JSON document holding every workflow, so it
  is a shared resource of its own; last-writer-wins on it is a defect.
- Recovery never deletes or overwrites another workflow's active state. The
  size-pressure eviction in `saveEvictingHistory` reads and rewrites other
  workflows' history and must keep running under whatever protects the file.
- The child process exits before the lock that represents thread ownership
  is released, because Codex's per-thread writer lock is handed over that way
  (`docs/MVP.md`, "Codex app-server integration").

## Design

One lock becomes two, with different keys and different spans.

### The workflow lock

Keyed by the workflow, held for the whole review exactly where the single
lock is held today: from before the first state read, through validation,
the disposable checkout, the child's lifetime, and the final state write,
until after the child has exited.

- Path: `<state dir>/locks/<hash>.lock`, where `<state dir>` is the directory
  holding the state file and `<hash>` is the first sixteen hex characters of
  the SHA-256 of the workflow key. This is the scheme `internal/scratch`
  already uses to name a workflow's checkout directory, so the two are
  visibly the same workflow. The hash keeps untrusted path and ref text out
  of file names. `COUNTERPOINT_STATE_FILE` moves the locks with the state
  file, which is what tests and unusual installations rely on.
- Wait and failure: unchanged, two seconds then `ErrLocked`. The message
  says which workflow: "another review of this branch is in progress".
- Files are created on first use with mode 0600 in a 0700 directory and are
  never deleted; one small file per workflow ever reviewed. Deleting an
  unheld lock file is harmless on advisory locks and nothing needs to.

### The state lock

The existing `state.json.lock`, now held only around each read or
read-modify-write of the state file, each of which takes milliseconds and
runs no Git or Codex:

1. At the start, under the workflow lock: acquire, `Load`, copy this
   workflow's record, release. Everything that follows in setup, including
   `ValidateTarget` with its Git subprocesses, the request hash, the replay
   decision, and reading `COUNTERPOINT.md`, runs under the workflow lock
   alone. The copy is stable because the workflow lock excludes any other
   round of this workflow; nothing else may change this record. The wait is
   the existing two seconds.
2. At the end, after the Codex turn: acquire, `Load` again, reconcile (below),
   `Put` this workflow's completed record into the fresh state, run the
   size-pressure eviction if needed, `Save` atomically, release. The wait
   here is separate and longer, thirty seconds bounded by the request
   context, because a review has been paid for by then and the holder may
   legitimately be another process finishing at the same moment: encoding,
   syncing, renaming, and up to two eviction retries.

Reloading before the final save is what protects entries other reviews
wrote in the meantime.

#### Reconciling the final read

Two things may legitimately differ between the record copied at the start
and the one read at the end, and one may not:

- The record's **history** may have been cleared by another review's
  second-stage size-pressure eviction, which clears every workflow's history
  to fit a completed review into the file. That eviction is compliant with
  the invariants: history is derived convenience data, replay fields are
  never touched. The final save therefore builds this round's history from
  the **fresh** record's history plus the record of the round just
  superseded, not from the copy taken at the start, so an eviction is
  honored rather than resurrected. If the result still does not fit, this
  review's own eviction runs as today.
- Records of **other workflows** may have been added, replaced, or had
  their history cleared. They are carried as read.
- This workflow's **replay fields** (thread id, last commit, base, request
  hash, round, last review, last warnings) may not differ. A difference means
  a writer bypassed the workflow lock, a foreign process or an older
  Counterpoint, and the save is refused with `ErrStateInvalid` rather than
  overwriting. Losing the completed review is the same outcome as any other
  failed save today, and this path is reached only in a mixed-version or
  hostile situation.

Failure to take the state lock uses the same `ErrLocked` sentinel with the
message "the state file is busy". At the start, contention means an older
Counterpoint holding the lock for a full review, or a stuck process. At the
end, the longer wait covers concurrent completions.

### Ordering

The workflow lock is the root: it is always taken first and released last.
The scratch directory lock is taken while holding the workflow lock and,
as today, held until the checkout is removed at the very end, which is after
the final state write. The state lock is therefore taken while holding the
workflow lock and, in a build-capable review, the scratch lock too. It is a
leaf: it is never held while acquiring any other lock, and it protects only
the state file. A process waiting on the state lock holds locks nobody else
can be waiting for while holding the state lock, so no cycle is possible.
That is the partial order the implementation relies on, not a total order.

### What runs in parallel

Two reviews of different workflows now run concurrently: two Counterpoint
processes, two `codex app-server` children, two Codex threads. Codex's writer
lock is per thread, so they do not contend. Two app-servers on one machine is
already the normal state of DR's desk, where the Codex desktop app's own
app-server runs beside Counterpoint's. The cost is the caller's: twice the
tokens and CPU for the overlap.

## Rejected alternatives

- **One state file per workflow.** Removes the shared document but makes
  "all workflows" operations (size pressure, a future reset, listing) a
  directory walk, changes the on-disk format, and needs a migration. The
  short state lock gets the same concurrency with the format unchanged.
- **A long wait on the global lock instead of failing.** Queues reviews
  behind each other for up to twenty minutes each, which is the problem
  restated.
- **Lock the repository, not the workflow.** Coarser than needed: two
  branches of one repository are independent reviews on independent threads,
  and the scratch checkout is already per workflow.
- **Deleting lock files when a review ends.** Racy against a concurrent
  acquirer opening the same path; the files are tiny and bounded by the
  number of workflows.

## Upgrade note

An older Counterpoint holds `state.json.lock` for its whole review. A new one
running beside it fails cleanly at the state lock ("the state file is busy")
at the start, and at the end would fail to save a completed review. MCP
servers start with the client session, so after `make install` or a brew
upgrade every Claude Code session must be restarted before reviews are run
from it; `README.md` already says so, and the release notes will repeat it.

## Tests

- Two reviews of different workflows in flight at once through the fake
  reviewer, the first held in its turn while the second completes; both
  records present and correct in the state file afterwards.
- A second review of the same workflow refused with `ErrLocked` while the
  first is in its turn, spawning nothing.
- Another workflow's record written to the state file while a review is in
  its turn survives that review's final save.
- This workflow's record changed under the workflow lock by a foreign writer
  makes the final save fail with `ErrStateInvalid` and leaves the file as the
  foreign writer left it.
- Concurrent size pressure: another review's second-stage eviction clears
  this in-flight workflow's history; the final save honors it (no history
  resurrected), records the round, and does not report a conflict.
- The existing lock tests move to the workflow lock: bounded wait and clear
  failure, and the child terminated before release.
- The state lock held elsewhere at the start fails the review before
  anything is spawned, with the busy message; held at the end for less than
  the final wait, the save still succeeds.
- The start-phase state lock is released before `ValidateTarget` runs
  (observed through a Git operation that checks the lock is free).

## Documentation

`docs/MVP.md` "Concurrency and cancellation" describes the two locks and
their spans; the "accepted MVP limitation" sentence and the "per-workflow
locks" deferral go, while per-workflow state files stay deferred; "Required
tests" gains the cases above. `README.md` "Operational notes" gains a
sentence on parallel reviews and the two failure messages, and "Timeouts"
names both lock waits.
