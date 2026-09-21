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

## Outcome, 2026-09-21: unavailable on the inline review path

This section replaces two earlier amendments that recorded hypotheses as
they were tested. Those hypotheses are in the Git history; what a reader
needs is the established cause, the limitation, the response, and the
condition for reopening.

### Established cause

`review/start` does not run the model work on the parent thread. It spawns
a review delegate session, and in pinned `rust-v0.153.1` the delegate's
`forward_events` discards `EventMsg::TokenCount` along with several session
and startup events before forwarding the rest to the parent. The
app-server's `handle_token_count_event` is what turns a `TokenCount` into
`thread/tokenUsage/updated`, and it never receives the review's. The usage
is produced and recorded; it is filtered out one layer below the protocol.

- `codex-rs/core/src/codex_delegate.rs`, `forward_events`
- `codex-rs/core/src/tasks/review.rs`, `ReviewTask` and `process_review_events`
- `codex-rs/app-server/src/bespoke_event_handling.rs`, `handle_token_count_event`

The same exclusion is present in the `0.155.1` source, so upgrading is not
a demonstrated fix.

### Evidence

One authorized live review, 2026-09-21, Codex Desktop/0.153.1,
`gpt-6-astra`, high effort. It completed in about 144 s with a substantive
verdict.

| observation | result |
| --- | --- |
| `thread/tokenUsage/updated` on the stream, any id, incl. 2.58 s after completion | none |
| Counterpoint's own receipt counters | zero received, refused, misattributed |
| delegate rollout `token_usage_record` entries | 18 |
| parent rollout usage records | 0 |

Parent `01a0c5bb-206c-7f51-9061-98467a5177a2`; delegate
`01a0c5bb-22cc-7843-b1f3-c8c05a74ac50`, whose session metadata names that
parent and `{"subagent":"review"}`. The delegate's `turn_token_usage` for
the round: 826,307 input of which 772,736 cached, 3,380 output, 1,431
reasoning, 829,687 total. Those are reported token counters, not a cost
measurement, and one round is not a basis for conclusions about spend.

### Read-only probes, and exactly what they showed

Four requests against a fresh app-server, no thread resumed and no turn
started:

| request | result |
| --- | --- |
| `account/usage/read` `{threadId: <delegate>}` | `threadUsage: null` |
| `account/usage/read` `{threadId: <parent>}` | `threadUsage: null` |
| `account/usage/read` no params | works: `lifetimeTokens`, six `dailyUsageBuckets` |
| `thread/read` `{threadId: <delegate>}` | works, returns `parentThreadId` |
| `thread/list` `{pageSize: 50}` | 25 threads, none carrying `parentThreadId` |

The pinned source maps a backend 403 or 404 to `threadUsage: null`, so the
per-thread query fails for the delegate as well as the parent, and asking
about the wrong id was not the reason.

**The `thread/list` result does not establish that delegate discovery is
impossible.** That probe used default parameters: it requested no
subagent or review source filter and did not paginate beyond the first
page. It shows only that sub-agent threads did not appear in a default
first page. Anyone reopening this should not cite it as proof.

### Response

Park the feature. The ledger and `--usage` stay on `main` and stay capable
of recording a valid report; on the inline review path they report `not
recorded`, because a round of unknown cost displayed as a round that cost
nothing would be worse.

Two alternatives were considered and declined. A reader for Codex's
on-disk rollouts would give per-round accuracy — the records carry the
parent's id as `session_id` and group a round by `root_turn_id`, so
discovery would not even need the delegate id — but it takes an ongoing
compatibility burden on an undocumented private format, and it inverts the
property that Counterpoint learns about reviews only through the protocol.
Account-wide daily buckets work and are protocol-only, but they answer a
different question: what an account spent in a day, never what a round
cost. Neither is needed to keep reviews working.

The client-side work is retained on its own merits: usage accepted at or
after a turn's completion without letting any other notification revise
terminal state, the bounded grace window, and diagnostics that distinguish
absent from malformed from misattributed. The scenarios covering before,
after, late, superseded, absent, and unrelated notifications protect real
client behaviour whatever Codex does.

### Reopening condition

A supported Codex path exposes review usage with reliable round
attribution. At that point, test the retained client against it rather than
rebuilding: the handling, the validation, and the scenarios are already in
place, and what is missing is only the channel.

### Upstream report

To be filed against `openai/codex`, keeping the two observations separate
since no evidence connects them:

> **Inline review usage never reaches the parent thread**
>
> On `codex-cli 0.153.1` (Codex Desktop/0.153.1, `gpt-6-astra`, high
> effort), an app-server client that runs `review/start` and listens for
> `thread/tokenUsage/updated` receives nothing, although the review
> completes normally and produces a verdict.
>
> `review/start` spawns a review delegate session; `forward_events` in
> `codex-rs/core/src/codex_delegate.rs` discards `EventMsg::TokenCount`
> before forwarding the delegate's remaining events to the parent, and
> `handle_token_count_event` in
> `codex-rs/app-server/src/bespoke_event_handling.rs` is what emits the
> notification. The same exclusion is present in `0.155.1`.
>
> Observed on 2026-09-21: no notification on the stream for any id,
> including 2.58 s after completion; the delegate's rollout holds 18
> `token_usage_record` entries and the parent's holds none.
>
> Separately, `account/usage/read` returns `threadUsage: null` for both
> the parent and the delegate, which the pinned source maps from a
> backend 403 or 404; the suppressed status is unknown. Whether that
> shares a cause with the filtering is not established.
>
> A fix would need to expose review usage with correct thread and turn
> attribution. Simply removing `TokenCount` from the delegate drop-list
> looks insufficient: the delegate's totals must not overwrite or be
> double-counted into the parent's cumulative totals.

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
