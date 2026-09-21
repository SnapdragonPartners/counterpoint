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
this change stays valid and reports unknown rather than zero. That is the
whole migration: no version bump, since the shape is additive and older
Counterpoints ignore unknown fields.

### Which number is the round's

The notification carries `last` and `total`. The schema documents neither's
scope, and the accepted set of meanings differs in a way that matters: if
`last` is the most recent model request rather than the whole turn, it
understates a turn that made several. Rather than guess, both are recorded.

`total` is cumulative for the thread, and Counterpoint resumes one thread per
workflow across rounds, so the newest round's `total` is the branch's
lifetime spend. That figure lives on `Workflow.LastUsage` and therefore
survives history eviction, which is the property DR's decision otherwise
gives up: per-round detail dies with the records, the branch's lifetime total
does not. Per-round spend is shown as the difference between consecutive
snapshots wherever both are retained.

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
only where it is read back. A notification with no `tokenUsage` object, or
with it null, is refused: decoding it into a value type would record zeros
where the app-server reported nothing, and absent must stay unknown. A
notification with a negative counter is refused, because persisting one
makes every later round of the workflow fail the load-time check and leaves
it recoverable only by hand-editing the state file. Refusals are counted and
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
