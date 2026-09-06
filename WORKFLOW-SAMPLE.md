# Counterpoint review workflow (template)

This is template language for the `CLAUDE.md` (or `AGENTS.md`) of a project
that uses Counterpoint. Copy the sections below into that file and edit them:
replace `OWNER` with the name of the human who authorizes pushes and merges,
replace `VERIFY` with the project's verification command (for example
`make check` or `npm test`), and delete anything that does not apply. The
wording assumes Claude Code as the authoring agent and Codex as the reviewer;
adjust the names if your setup differs.

Everything from the next heading on is the template.

---

## Roles

- Claude authors specifications, documentation, code, and fixes.
- Codex reviews committed artifacts and code.
- OWNER orchestrates, resolves contention, authorizes external effects, and
  accepts.
- Work is accepted only after Codex and OWNER approve it.
- Claude/Codex communication goes through the Counterpoint review tool when
  it is available in the session; otherwise it routes through OWNER manually.

If Claude and Codex do not converge after reasoned attempts, preserve both
positions and escalate the decision to OWNER rather than cycling
indefinitely.

## Branch and review workflow

1. Work on a feature or fix branch from the repository's primary branch.
   Never develop directly on the primary branch.
2. Make the scoped change and run verification proportionate to its risk
   (`VERIFY`).
3. Commit locally. Never bypass hooks with `--no-verify`; fix failures.
4. Write branch notes for Codex and submit the exact local commit for review.
5. Address every blocking finding with a fix or a reasoned response. Commit
   each review round locally and submit the new commit with updated notes.
6. Continue until Codex has no blocking findings, OWNER explicitly overrides
   one, or Claude has escalated a disputed finding to OWNER.
7. Stop for OWNER approval. Do not push before both Codex and OWNER approve;
   push is a human gate, not a routine implementation step.
8. After authorization, push and open a pull request to the primary branch.
9. Address every CI or review thread with a fix or reasoned reply, push the
   resolution, mark the thread resolved, and check for new feedback.
10. OWNER gives final approval and merges. Claude never merges.

Keep at most one feature branch open at a time unless OWNER explicitly
authorizes parallel work.

### Responding to findings

Codex is a reviewer, not an authority. Only blocking findings (P0: data loss,
corruption, critical security exposure, or an unusable result; P1: a concrete
correctness, robustness, architectural, or maintainability defect) must be
resolved before the push gate. Suggestions and style points are addressed at
Claude's discretion and their disposition noted. A finding Claude judges
wrong, out of scope, or disproportionate gets a reasoned response in the next
round's notes rather than a change. If Codex reaffirms it, stop, state both
positions, and bring the decision to OWNER before another round. Do not
implement a change Claude believes is wrong in order to end the loop.

### Submitting for review

- If the `counterpoint` review tool is available, call it directly with the
  absolute repository path, the branch, the exact local commit, and the
  branch notes. The commit must be the branch tip and the checked-out HEAD
  of a clean worktree, so commit before calling. In a read-only review the
  reviewer reads the worktree, so do not touch it while the review runs; in
  a build-capable review (`build: true`) the reviewer works in a disposable
  checkout, and the only obligation is not to rewrite the branch meanwhile.
  The call blocks for the whole Codex turn, typically several minutes.
  Submit every round through the tool; do not ask OWNER to relay when the
  tool is available.
- Ask for a build-capable review when a test run is material evidence: code
  changes, especially to concurrency, persistence, protocol handling, or Git
  interaction. Keep documentation-only rounds and small follow-ups read-only.
- If the tool is not available in the session, write the branch notes and
  ask OWNER to submit them to Codex manually, then wait for the relayed
  findings.
- The reviewer starts each round fresh; its memory is the last three rounds'
  verdicts, which Counterpoint quotes into the prompt. Write later rounds'
  notes as a delta, but keep the disposition of every prior finding in each
  round's notes, since older rounds are evicted and the reviewer is told to
  re-validate rather than trust its earlier output. A new or renamed branch
  starts a new review thread.
- If the tool reports the thread is unavailable, stop and tell OWNER rather
  than working around it; the error says what to do (usually archive the
  thread in the Codex app so the next call can take it over).
- Treat the tool result as Codex's review: address every blocking finding as
  above, and quote the approval or the findings to OWNER when stopping at
  the push gate. The author's verification claims must be backed by the
  commands and outcomes in the notes.

### Branch notes

Branch notes are the handoff artifact, not a generic summary. Include:

- review round and exact commit;
- whether the change is code, documentation, or both;
- verification commands and outcomes;
- a concise account of material changes;
- resolution of each prior review finding;
- important design choices and rejected alternatives;
- known limitations and open questions; and
- branch status, local commit count when useful, and confirmation that it is
  unpushed and awaiting approval.

Codex verifies the notes against the commit and repository rather than
assuming the author's claims are correct.
