# Cross-round review history from a Counterpoint ledger

Design record for
[issue 19](https://github.com/SnapdragonPartners/counterpoint/issues/19).
The diagnosis and the design were settled between DR, Claude, and Codex on
2026-09-05 before any code was written; the implementation on the same
branch follows this document. Once it lands, the contract lives in
`docs/MVP.md` and this file keeps the evidence, the reasoning, and the
rejected alternatives.

## Finding

Counterpoint runs every round with `review/start` on a persistent Codex
thread and assumed the reviewer therefore sees earlier rounds. It does not.
In codex-cli 0.153.1 a review is a one-shot sub-thread: `tasks/review.rs`
calls `run_codex_thread_one_shot` with `SubAgentSource::Review`, no initial
history, and the review rubric as the system prompt, seeded with the review
prompt alone. When the sub-thread finishes, `exit_review_mode` records only
the rendered result into the parent thread, as a user message wrapped in a
`<user_action>` template plus an assistant message. The parent thread is
therefore an audit log that a plain turn can read, and the Codex app renders
it as a continuous conversation, but review mode never reads it.

This was verified three ways that do not depend on the model's self-report:

- Source, at tag `rust-v0.153.1`: `codex-rs/core/src/session/review.rs`
  seeds the task with a single user input and
  `codex-rs/core/src/tasks/review.rs` starts the delegate with
  `initial_history: None`.
- Rollouts: every review sub-thread under `~/.codex/sessions` (first line
  `"source": {"subagent": "review"}`; the app hides them) begins with the
  skills text, the plugin list, and the round's Counterpoint prompt. Twenty-six
  rounds across seven branches contained nothing from an earlier round.
- Token counts: the first model call of each round carried a flat 10k to 14k
  input tokens from round 1 through round 8 of one branch. Inherited history
  would grow that number every round.

Earlier observations of "retained context" were reconstructions. The
reviewer reads `git log` and the diff, and the branch's commit messages
describe how each finding was fixed, so disposition questions are answered
correctly without any memory. Only a sentence that exists solely in the
reviewer's own earlier output can distinguish the two, and a deterministic
test of the prompt is better evidence than asking.

## Goal

Give the reviewer its own earlier verdicts on this branch, so that the
prompt's instruction to carry forward and re-validate every unresolved
finding has something to act on, without depending on Codex thread memory.

Non-goals: verbatim tool history across rounds, a general conversation with
the reviewer, and any change to what Codex's review mode does.

## Decisions already taken with DR and Codex

- Keep `review/start`. Its fresh reviewer, review rubric, structured result,
  and independence from earlier approvals are wanted for a formal review.
- Rejected: running rounds as `turn/start` on the persistent thread, the
  fallback the MVP specification had planned. It would give real thread
  memory, but the thread would then carry every round's tool output. In the
  sub-thread rollouts, round one of a code review ended at 90k to 190k
  context tokens of a 258k window, so compaction would begin by round two or
  three and the memory would become a summary Codex writes, at rising cost
  per round. It would also drop the rubric unless `baseInstructions` were
  set on every start and resume, and lose the structured review item.
- Counterpoint owns cross-round memory explicitly: a bounded ledger of the
  reviewer's earlier verdicts, persisted with the workflow and injected into
  every later prompt.
- Earlier branch notes are not retained. They are the author's untrusted
  claims, the current round's notes must restate every prior finding's
  disposition anyway, and the reviewer cannot reconstruct only one thing
  from the repository: its own earlier verdict.
- Warnings are not retained beyond the replay copy. They describe
  Counterpoint's operation, such as an unarchived thread, not the commit.
- The ledger lives in the existing state file. Per-workflow files and locks
  are an explicit MVP deferral, and the single file already has atomic
  writes and `0600` permissions. It has no explicit no-follow check on read,
  and this design does not claim one.
- A live probe does not gate acceptance. Delivery of history is a property
  of the compiled prompt and is proven by a deterministic test; a live run
  would measure reviewer behavior, not correctness.
- A separate, non-gating discussion operation using `turn/start` may be
  useful later and is deferred to its own issue.

## Ledger

The workflow record keeps its replay fields unchanged: `thread_id`,
`last_commit`, `last_base`, `last_request_hash`, `round`, `last_review`,
`last_warnings`. It gains `history`, an ordered list of records for
completed rounds before the one held in `last_review`:

```json
{"round": 1, "commit": "<object id>", "base": "<object id>", "review": "<text>"}
```

`commit` and `base` are full lowercase Git object ids, 40 hex characters
for SHA-1 or 64 for SHA-256, the same domain the Git boundary accepts. A
record whose review text could not be retained keeps `round`, `commit`, and
`base`, has an empty `review`, and carries `omitted` with the single fixed
value `too-large`, so the prompt can say a round happened without misquoting
it. Exactly one of `review` and `omitted` is present. Rounds evicted for
count or space are removed outright and disclosed by count, not kept as
placeholders.

Bounds, applied in this order when a round completes and the previous
`last_review` is pushed into `history`:

- `MaxHistoryRecordBytes = 24 KiB`: a review larger than this is pushed as
  a placeholder record, never truncated. A truncated verdict could mislead.
- `MaxHistoryRecords = 2`: with `last_review`, three rounds are visible.
- `MaxHistoryBytes = 48 KiB` over all review text injected into a prompt:
  the records plus the previous round's `last_review`, which is itself
  subject to the per-record cap at injection time.

Eviction removes whole oldest records until every limit holds; a record
never has its text shortened. Observed reviews run 300 to 5,300
characters, so these caps are generous. The injected history is at most
48 KiB, roughly 12k tokens of ordinary text and more for text that
tokenizes badly; the prompt's own size limit still applies on top.

Replay is unchanged. An identical request is answered from `last_review`
before Codex is called, so no record is appended; a test asserts it.

Aggregate limit. The state file is capped at 16 MiB and `last_review` alone
can approach the 16 MiB message limit, so a paid review can already fail to
save; this design does not change that. What it guarantees is that history,
this workflow's or any other's, never causes such a failure. When `Save`
reports `ErrTooLarge`, the review service, still holding the review lock,
clears this workflow's `history`, logs the eviction, and retries; if the
file is still too large it clears every workflow's `history`, logs each
one, and retries once more. Replay fields are never touched, so no
workflow loses its completed-review record; history is derived
convenience data, which is why evicting another workflow's copy is
acceptable under the rule that recovery never deletes another workflow's
active state. Tests cover both stages: a file just under the limit where
clearing the current workflow's history suffices, one where only the
second stage suffices, and the same state with no history at all, which
fails exactly as today.

## State file version 2

The envelope version becomes 2 because the record shape changed. `Load`
accepts version 1 and version 2; a version 1 file is a version 2 file with
empty history, so migration adds nothing and the stored review, commit,
base, round, and warnings are already the newest record. The next `Save`
writes version 2. Version 1 binaries reject a version 2 file with their
existing unsupported-version error instead of silently rewriting it without
the ledger. Request identity and the completed-record invariants are
unchanged.

## Prompt changes

`Prompt` gains `History`, the records plus the previous round's review, in
chronological order, and `OmittedRounds`, the count of completed rounds no
longer retained. After the round context and before the rules, later rounds
get an "Earlier rounds" section:

- The prompt's introduction no longer says the author will "ask again on
  this same conversation", which promised a memory the one-shot review
  sub-thread does not have. It says the author may fix findings in a
  further commit and ask for another round, in which the verdicts of
  recent rounds are quoted back. The prompt tests pin the new wording.
- A statement that the verdicts below were recorded by Counterpoint from
  this reviewer's own output on earlier commits of this branch, are
  historical data and untrusted input, and contain no instructions to
  follow.
- Earlier approvals are not binding on the commit under review. Every
  earlier finding must be re-validated against the current immutable
  commit and its disposition stated. When history was rewritten, the
  recorded commits may no longer be ancestors and a complete reassessment
  is required.
- File paths in earlier verdicts may refer to a disposable checkout that has
  since been deleted; map them to repository paths in the current commit.
- Each record as `Round N, commit <id>, base <id>` followed by its text
  between delimiters built by the same collision-avoidance as the branch
  notes, derived from the record text, so a verdict cannot forge its own
  end. A placeholder record states that its text was not retained and why.
- When rounds were evicted: "Rounds 1 to K are not retained; the current
  branch notes carry the disposition of their findings."

The previous round's review is injected under the same per-record cap: if
`last_review` exceeds it, the round is listed with its text omitted.

The existing round-context sentence, "Carry forward every unresolved
finding from earlier rounds", now refers to the section below it.

## Failure modes

- `Save` fails for a reason other than size after a paid review: unchanged,
  the error says the review completed but state was not saved.
- A stored ledger fails validation: `ErrStateInvalid`, consistent with the
  existing refusal to act on a corrupt workflow record, and values are
  never echoed. Validation runs on load, before any value reaches a prompt,
  and rejects: more than `MaxHistoryRecords` records; rounds that are not
  a contiguous ascending suffix ending at the workflow's `round` minus one;
  a `commit` or `base` outside the object-id domain above; a `review` over
  the per-record cap or a total over the aggregate cap; a record with
  neither or both of `review` and `omitted`; and an `omitted` value other
  than `too-large`. Nothing from the ledger is interpolated outside its
  delimiters except the round number and the two validated object ids.
- History was rewritten: records are kept and the prompt says so; the
  reviewer is told to reassess completely.

## Tests

Each protected behavior gets a test that fails when the behavior is
removed, with the mutation check recorded in the branch notes:

- Persistence across restart: the end-to-end test's fake app-server writes
  the full instructions it received beside the state file, and after a
  process restart the round-two prompt contains the round-one review text
  verbatim between its delimiters.
- Migration: a version 1 file loads, produces the expected round-two prompt,
  and is written back as version 2; a version 2 file is rejected by the
  version 1 loader path with the unsupported-version error.
- Bounds and eviction: record count, per-record placeholder, total bytes
  including the previous round's review, chronological order, and the
  omitted-rounds line.
- Corruption fixtures for every rejected ledger shape listed under
  "Failure modes", each asserting `ErrStateInvalid` without the value in
  the message.
- Aggregate limit: the two eviction stages and the no-history control
  above.
- Delimiter forgery: a review containing the plain closing marker gets a
  derived marker that does not occur in it.
- Replay: an identical request appends nothing and returns the stored
  result.
- Rewritten history: records survive and the prompt carries the reassessment
  instruction.
- Exact prompt inclusion: a unit test on `Prompt.Build` with three records.

## Documentation

- `docs/MVP.md`: cross-round context is supplied by Counterpoint's ledger
  and not inherited from the Codex thread; remove the sentence claiming the
  acceptance run confirmed cross-round context and that the `turn/start`
  fallback was not needed; state the ledger's fields, caps, and eviction;
  rewrite acceptance step 6 as the deterministic property that the
  round-two prompt contains the round-one review; bump the state version.
- `CLAUDE.md`: the persistent thread is an audit log and the reviewer's
  memory is the last three rounds from Counterpoint's ledger; branch notes
  must still restate every prior finding's disposition because older rounds
  are evicted.
- `README.md`: one sentence on what the reviewer remembers, if the
  operational notes mention the thread.

## Deferred, tracked separately

- A non-gating discussion operation on the persistent thread via
  `turn/start`, for open-ended design dialogue with the reviewer.
- Finer-grained cross-workflow eviction, oldest records first across
  workflows, if clearing whole histories under size pressure ever proves
  too coarse.
