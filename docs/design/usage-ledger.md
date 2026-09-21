# Recording the reviewer's token usage

Design record for
[issue 41](https://github.com/SnapdragonPartners/counterpoint/issues/41).

Status: Proposed (Claude, 2026-09-21)

## Problem

Counterpoint spends real money on every review round and keeps no record of
it. There is no token accounting anywhere in the runtime, so a user who wants
to know what a branch has cost has no way to find out. DR adopted
`docs/design/configuration.md`'s effort change on the strength of a guess
that reasoning effort dominates the bill; nothing measures whether that is
true.

The data already arrives and is discarded. The app-server emits
`thread/tokenUsage/updated` carrying a `ThreadTokenUsage` with `last` and
`total` breakdowns, and `turnWatcher.handle` ignores every method it does not
recognise, so the notification never leaves the reader goroutine.

## Decisions taken before design

DR settled three questions:

- **Usage lives in the existing ledger** and is discarded when the work
  records it describes are discarded. Anything needing longer life is
  extracted to real storage outside Counterpoint.
- **Completed rounds only.** A failed or interrupted turn has still spent
  tokens, but persisting a round that produced no verdict cuts against
  completed-only persistence. DR observes a high completion rate and accepts
  the understatement.
- **Not returned to the calling agent.** The review tool's result is the
  reviewer's verdict and stays that way. Usage goes to the log and to a new
  `counterpoint --usage`.

## Invariants that stay

- Only completed reviews are persisted. This change adds no persistence path
  for unfinished work.
- The request identity is unchanged. Usage is an output, never an input, so
  it cannot enter the hash and cannot invalidate a stored replay.
- A replayed round spends nothing and records nothing.
- The ledger's retention bounds still hold, and the eviction estimate must
  still never understate what evicting a record frees.
- Absent is not zero. A round reviewed before this change has unknown usage,
  and reporting it as zero would understate a total.
- State updates stay atomic under the existing locks. No new shared resource
  is introduced, so the lock order is untouched.

## Design

`state.Usage` mirrors the protocol's breakdown: input, cached input, cache
writes, output, reasoning output, and total. All counters are non-negative.

Two homes, matching how the ledger already splits the newest round from the
earlier ones:

- `Workflow.LastUsage` for the newest completed round, beside `LastReview`.
- `HistoryRecord.Usage` for retained earlier rounds.

When a round completes, the superseded round's usage moves into its history
record exactly as its verdict does, and is evicted with it.

Both are pointers, absent in JSON when unset, so a state file written before
this change stays valid and reports unknown rather than zero.

The envelope version is not bumped, and that is a decision with a cost worth
stating rather than a free consequence of the shape being additive. An older
binary reads the file happily, because its decoder ignores unknown fields.
But `Save` re-marshals typed values, so the first review that older binary
runs rewrites the file without `last_usage` or any history `usage`, for
every workflow in it, not only the one it reviewed. Running two versions
against one state file therefore loses recorded usage silently.

Bumping the version would make that loud instead: an older binary would
reject the file outright, which is what the version 1 to 2 bump chose when
the history ledger arrived. It is rejected here because the blast radius is
larger than the defect. An older binary would fail every review until every
session was upgraded, and a rollback would need the state file deleted,
which discards thread associations and review history: a branch would start
a fresh Codex thread and lose its ledger. Those matter to review quality.
Usage does not: it is telemetry DR has already classified as ephemeral, and
the version 1 to 2 precedent was protecting the history the prompt quotes,
which is the axis this case differs on.

So the hazard is documented instead, alongside the existing instruction to
restart sessions after installing a new version, which is the same hazard's
other face.

### Which number is the round's

The notification carries `last` and `total`. The schema documents neither's
scope, and the accepted set of meanings differs in a way that matters: if
`last` is the most recent model request rather than the whole turn, it
understates a turn that made several. Rather than guess, both are recorded.

If `total` is cumulative for the thread, then because Counterpoint resumes
one thread per workflow across rounds the newest round's `total` is the
branch's lifetime spend, and that figure lives on `Workflow.LastUsage` and
survives history eviction. That conditional is the interesting property, and
it is a conditional: the report states the figure and its source and leaves
the inference to the reader.

An earlier draft of this record had the report show per-round spend as the
difference between consecutive snapshots. That was dropped in review: a
difference asserts that `total` accumulates, which is exactly what is not
established. Nothing in the shipped report computes a difference.

The report never presents either breakdown as a measured cost: a `last`
value is labelled a last report, their sum is labelled a sum of last reports
and stated to be a lower bound, and a `total` is labelled as the thread
total reported at that round. Round 1 of review labelled and summed `last`
as whole-round spend, which asserted the very scope this section leaves
open.

**This rests on an unverified protocol claim** — that `total` accumulates
across resumed rounds of the same thread — and no automated test can settle
it, because the fake app-server answers whatever it is told to. It needs one
live run, which needs DR's approval. Until then `--usage` labels the
cumulative figure as reported by the app-server rather than as established
fact, and the limitation is recorded in `docs/SPEC.md`.

### Validation

Usage arrives as child-process output and is validated where it enters, not
only where it is read back, against the fields the generated schema marks
required. Every one of them is a pointer in the wire type, at both levels,
so absent and null are distinguishable from a report of zeros: decoding any
of them into a value type records zeros where the app-server reported
nothing, and absent must stay unknown. `cacheWriteInputTokens` is the only
optional counter and an absent one is its documented zero. A notification
with a negative counter is refused, because persisting one makes every
later round of the workflow fail the load-time check and leaves it
recoverable only by hand-editing the state file. A refused report leaves an
earlier valid one for the same turn standing.

`resolve` does not assume `valid` ran before it. It is the reader
goroutine that decodes these messages, so a nil dereference there would
take the server down rather than lose a counter. Refusals are counted and
logged once per turn rather than surfaced as review warnings: the verdict is
unaffected and the calling agent can do nothing about it.

The same check runs again where usage is converted for saving, so a Reviewer
other than the app-server client cannot write a record the loader rejects.
The round's telemetry is worth less than the workflow, so unusable usage is
dropped and logged rather than failing a review that has already been paid
for.

Usage is also untrusted file content like the rest of the state.
`InvalidHistory` gains checks that no counter is negative. The relationships between counters
are deliberately not validated: which of cached, reasoning, and cache-write
tokens are subsets of which totals varies by model, and a rule invented here
would reject valid files from a model that reports differently.

### Eviction accounting

`recordFramingBytes` bounds a record's fixed fields and framing, and a usage
object adds roughly two hundred bytes of indented JSON. The constant is
raised so the estimate still never understates what evicting a record frees;
the existing test that checks the bound against real encodings covers the new
shape.

### The `--usage` surface

`counterpoint --usage` is a second non-server mode beside `--version`: it
loads the state file, prints, and exits without starting the MCP server. Per
workflow it reports the branch, the rounds it has a record for, the lifetime
total as reported, and per-round spend where consecutive snapshots survive.
The output states that it counts completed rounds only, so the figure is
never mistaken for the whole bill.

## Amendment, 2026-09-21: nothing was recorded against real Codex

The first live round after installing recorded no usage at all. The logs
ruled out the obvious causes: the new binary was running, no report was
refused as malformed, and `dispatch` passes every notification to every
subscriber. The method string is present in `codex-cli 0.153.1`, so the
server can emit it.

A cause was found in this code, and it is a real defect whether or not it
is *the* live cause. `turnWatcher.handle` dropped every notification once
`turn/completed` set `finished`, and `Review` returned as soon as the turn
finished, so a report arriving at or after the completion was discarded
either way.

The tests did not catch it because the fake reported usage *before* the
completion. That ordering was my assumption, encoded into the fake and then
verified against itself. Moving the fake's report to after the completion
reproduces the live symptom.

What that does **not** establish is what happened in the live run. It shows
a report arriving after completion would have been dropped; it does not
show that one arrived. The app-server documentation names both events and
guarantees no ordering between them, so "usage is reported after the
completion" is an assumption, not a fact. Only an instrumented live round
can trace a received event through to a persisted figure, and the counters
below exist to make that round conclusive rather than another explanation
resting on a revised fake.

The fix keeps accepting usage for a finished turn, which no other
notification may revise, and holds the window open for `UsageGrace` after
the completion. The whole window is waited out rather than returning on the
first report, since a turn may report more than once and the last one
stands; returning early took a superseded figure, which the
`usage-superseded` scenario caught.

`UsageGrace` is a heuristic, not a protocol bound, and the tests now pin it:
the `usage-late` scenario reports 150 ms after the completion, and setting
the window to zero fails it. Without that scenario the window was not
load-bearing in any test, so the first version of this fix had a core
mechanism no test could fail for.

Usage notifications are counted on receipt, before any filtering, and the
count is logged with the refusal and misattribution counts beside it. The
line says no usable report was observed before the cutoff rather than that
the server reported none, because the second is a claim about the server
that nothing here supports.

A first version of these diagnostics counted `item/completed` notifications
as usage reports. The guard being edited appears in both handlers and the
edit replaced every occurrence, so the `no-usage` scenario logged usage
reports for a turn that sent none: a diagnostic that manufactured the
evidence it existed to gather. Both reviewers found it. The scenario now
asserts that no usage notification means no usage counted, and that a
filtered report is still counted as received.

## Rejected alternatives

**A separate append-only ledger file.** Survives eviction and keeps the state
file's budget intact, but it is a resource shared across workflows, and the
durable invariant requires serialising on a key matching the shared resource,
which the per-workflow lock does not do. DR rejected it: usage that outlives
the work it describes is of little value, and real durability belongs in real
storage outside Counterpoint.

**A monotonic per-workflow counter covering failed rounds.** Would close the
understatement from failed turns. Not built: DR observes a high completion
rate, and building for a gap that has not been measured is the speculative
extension `CLAUDE.md` warns against. Revisit if `--usage` and the logs
disagree materially.

**Reporting dollars.** The protocol carries no pricing, so a cost figure
means a hardcoded rate table that goes stale silently and is wrong for every
user on a different plan. Tokens are reported; conversion is the user's.

**Returning usage in the tool result.** Rejected by DR. It would put a number
the authoring agent cannot act on into every review, and the agent's job is
to address findings.

## Tests

`internal/appserver` covers the notification being captured for the matching
turn and ignored for another, and a turn that reports no usage at all.
`internal/state` covers the ledger round-trip, a pre-change file loading with
usage absent, and every rejected shape. `internal/review` covers usage
reaching the persisted record, moving into the history record when
superseded, being absent on a replay, and the eviction estimate still
bounding the larger record. `cmd/counterpoint` covers `--usage` against a
state file with records, without records, and absent.

## Documentation

`docs/SPEC.md` records the new state fields, the `--usage` mode, the
completed-rounds-only scope, and the unverified cumulative-total claim as a
known limitation. `README.md` documents the command.
