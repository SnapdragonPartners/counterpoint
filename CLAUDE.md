# CLAUDE.md

Operating instructions for Claude Code in this repository. `AGENTS.md` is a
tracked symbolic link to this file (Git mode `120000`; verify with
`git ls-files -s AGENTS.md`), so edit `CLAUDE.md` only and both entry points stay
in sync. Tooling that enumerates only regular files may not list `AGENTS.md`;
that does not mean it is absent.

## Project and authority

Counterpoint is a small Go MCP server that connects an authoring coding agent to
a persistent local Codex review thread. It automates review handoffs; it is not
a general-purpose orchestrator and must stop at the human push/PR gate.

Use the source appropriate to the question. For current behavior, precedence is:

1. Code and tests.
2. `docs/MVP.md` for the accepted MVP contract.
3. `README.md` for orientation.
4. Issues and discussion as non-binding context.

When code and the MVP specification differ, code describes the current runtime
and the specification describes the intended MVP. Do not silently resolve the
difference: identify it and either fix the implementation or obtain DR's
approval to change the specification.

Keep this file focused on durable operating rules. Put product behavior and
acceptance criteria in `docs/MVP.md`, not here.

## Roles

- Claude authors specifications, documentation, code, and fixes.
- Codex reviews committed artifacts and code.
- DR orchestrates, resolves contention, authorizes external effects, and accepts.
- Work is accepted only after Codex and DR approve it.
- Claude/Codex communication goes through the Counterpoint review tool when it
  is available in the session; otherwise it routes through DR manually. See
  "Submitting for review" below.

If Claude and Codex do not converge after reasoned attempts, preserve both
positions and escalate the decision to DR rather than cycling indefinitely.

## Branch and review workflow

1. Work on a feature or fix branch from the repository's primary branch. Never
   develop directly on `main` or `master`.
2. Never reuse an existing leaf branch name as a namespace prefix; Git refs
   cannot be both a leaf and a directory.
3. Make the scoped change and run verification proportionate to its risk.
4. Commit locally. Never bypass hooks with `--no-verify`; fix failures.
5. Produce branch notes for Codex and submit the exact local commit for review.
6. Address every blocking finding with a fix or a reasoned response. Commit each
   review round locally and submit the new commit with updated branch notes.
7. Continue until Codex has no blocking findings, DR explicitly overrides one,
   or Claude has escalated a disputed finding to DR (see below).
8. Stop for DR approval. Do not push before both Codex and DR approve; push is a
   human gate, not a routine implementation step.
9. After authorization, push and open a pull request to the primary branch.
10. Address every CI or review thread with a fix or reasoned reply, push the
    resolution, mark the thread resolved, and check for new feedback.
11. DR gives final approval and merges. Claude never merges.

Keep at most one feature/development branch open at a time unless DR explicitly
authorizes parallel work. Parallel fix branches are acceptable when they do not
share mutable state.

### Responding to findings

Codex is a reviewer, not an authority. Only blocking findings (P0 and P1 as
defined under "Security and testing") must be resolved before the push gate;
suggestions and style points are addressed at Claude's discretion and their
disposition noted. A finding Claude judges wrong, out of scope, or
disproportionate to the change gets a reasoned response in the next round's
notes rather than a change. If Codex reaffirms it, stop, state both positions,
and bring the decision to DR before another round. Do not implement a change
Claude believes is wrong in order to end the loop.

### Submitting for review

Counterpoint is registered as an MCP server in Claude Code, so the review
handoff is normally a tool call, not a message to DR:

- If the `counterpoint` review tool is available, call it directly with the
  absolute repository path, the branch, the exact local commit, and the branch
  notes. The commit must be the branch tip and the checked-out HEAD of a clean
  worktree, so commit before calling. In a read-only review the reviewer reads
  the worktree, so do not touch it while the review runs; in a build-capable
  review (`build: true`) the reviewer works in a disposable checkout, and the
  only obligation is not to rewrite the branch meanwhile. The call blocks for
  the whole Codex turn, typically several minutes; Claude Code moves it to a
  background task after two minutes and delivers the result as a task
  notification. Submit every round through the tool, and do not ask DR to
  relay when the tool is available.
- Ask for a build-capable review when the change touches behavior that tests
  exercise and a test run is material evidence: code changes, especially to
  concurrency, persistence, protocol handling, or Git interaction. Keep
  documentation-only rounds and small follow-ups read-only; a build review
  costs a cold build per branch plus a test run per round.
- If the tool is not available in the session, write the branch notes and ask
  DR to submit them to Codex manually, then wait for the relayed findings.
- The Codex thread is persistent per repository and branch, but it is an
  audit log, not the reviewer's memory: Codex's review mode starts each
  round fresh, and the reviewer's memory is the last three rounds' verdicts
  that Counterpoint quotes into the prompt from its ledger. Write later
  rounds' notes as a delta, but keep the disposition of every prior finding
  in each round's notes, since older rounds are evicted from the ledger and
  the reviewer is told to re-validate rather than trust its earlier output.
  A new or renamed branch starts a new thread and an empty ledger. If the
  tool reports the thread is unavailable, stop and tell DR rather than working
  around it; the error says what DR can do (usually archive the thread in the
  Codex app so the next call can unarchive and take it over).
- Treat the tool result as Codex's review: address every blocking finding as
  described in the workflow above, and quote the approval or the findings to
  DR when stopping at the push gate. In a read-only review the reviewer cannot
  build or run tests, so its verdict is an inspection of the commit against
  the branch notes and the repository; in a build-capable review it can, and
  its test results are evidence. Either way the author's verification
  claims must be backed by the commands and outcomes in the notes.

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

Codex must verify the notes against the commit and repository rather than assume
the author's claims are correct.

## Scope discipline

- Implement the smallest coherent change that satisfies the current accepted
  specification.
- Do not turn Counterpoint into Maestro. Scheduling, general workflow graphs,
  multi-agent planning, forge automation, and autonomous implementation are out
  of scope unless a later accepted design adds them.
- Avoid speculative extension points. Interfaces with one implementation need a
  concrete boundary, testability benefit, or imminent second consumer.
- Record deferred product work in GitHub Issues. Do not hide consequential work
  in untracked TODO comments.
- Commit open documentation work before beginning a disposable spike. Keep spike
  code outside production packages, and do not merge it without converting it
  into supported code with tests.

## Documentation

- `README.md` explains the product and points to authoritative detail.
- `docs/MVP.md` owns MVP scope, requirements, deferrals, and acceptance.
- Update documentation in the same change when behavior or a public contract
  changes.
- Do not claim planned behavior is implemented.
- Prefer focused documents over duplicating protocol fields, package inventories,
  or state flows in multiple places.
- Link consequential decisions to their issue or design document.

## Development and verification

Use the repository's checked-in build automation:

```bash
make check    # gofmt check, go vet, golangci-lint, go test -race; identical to CI
make schema   # regenerate the app-server schema from the installed Codex CLI
```

Run `make install-hooks` once per clone so the pre-commit hook runs the same
checks. Run `make schema` before making protocol claims and verify them against
the generated bundle in `.schema/`.

Do not run live Codex integration tests, paid model calls, or tests using a
developer's real credentials without DR's explicit approval for that run. Use a
fake app-server subprocess for automated integration coverage.

### Verification discipline

- Verify dependency and protocol claims against the pinned version's source,
  generated app-server schema, official documentation, or a focused reproducer.
- For fixes, reproduce the original failure, test the repaired boundary, and
  inspect adjacent call sites for the same defect pattern.
- Before implementing parsers, schemas, validators, persistence, or policy
  checks, enumerate accepted invariants and rejected cases.
- For nontrivial regression tests, temporarily break the protected behavior to
  prove that the test fails, then restore it before committing.
- State important untestable guarantees beside the implementation. Do not imply
  they are covered merely because neighboring tests pass.
- Prefer deterministic fakes at process and protocol boundaries. Use real,
  inexpensive components when they provide materially stronger evidence.

## Durable engineering invariants

These are acceptance requirements unless an authoritative specification changes
them:

- A workflow is identified by canonical Git repository identity plus full local
  branch ref. At most one active review turn may mutate its Counterpoint state.
- Reviews target resolved immutable commits. Never substitute a stale,
  payload-supplied diff for repository inspection.
- The supplied commit must exist and be reachable from the supplied branch.
- Counterpoint and Codex review in read-only mode. Neither edits the reviewed
  repository, pushes, creates pull requests, approves, or merges.
- State updates are atomic. Concurrent operations must be serialized using a key
  matching the shared resource; bare last-writer-wins is a defect.
- App-server stdout is protocol data only. Diagnostics and child stderr must
  never corrupt it.
- The app-server reader must handle interleaved responses, notifications, and
  server-originated requests, as well as messages larger than 64 KiB.
- Every server-originated approval or input request receives an explicit bounded
  response. Never leave the agent hanging indefinitely.
- Treat repository paths, refs, commit strings, branch notes, JSON state, model
  output, and child-process messages as untrusted input.
- Invoke Git and Codex with argument arrays, never shell-composed commands.
- Never log credentials, full environments, or model inputs and outputs by
  default. Errors should include operational context without leaking secrets.
- Recovery must not delete or overwrite another workflow's active state.

## Go and maintainability

- Prefer simple, idiomatic Go and the standard library where it is sufficient.
- Use `any` rather than `interface{}` in new code.
- Handle errors explicitly and add context at process, protocol, filesystem, and
  Git boundaries. Do not silently discard meaningful errors.
- Pass `context.Context` through blocking process and review operations and honor
  cancellation.
- Avoid unbounded goroutines, waits, buffers, retries, and subprocesses.
- Name non-obvious protocol methods, limits, timeouts, environment variables,
  and paths.
- Consolidate duplicated behavior when a shared helper creates a stable seam;
  do not force DRY across concepts that merely look alike.
- Keep wire types separate from domain types. Validate at the boundary rather
  than spreading untrusted JSON shapes through the codebase.
- Treat comments as contract. Remove stale comments and link consequential TODO,
  FIXME, and deprecation notes to an issue.
- Remove unreachable or orphaned code unless a documented build tag or accepted
  plan intentionally retains it.

## Security and testing

Counterpoint is local software, but local does not mean trusted. Block command
injection, unsafe filesystem targeting, symlink confusion, protocol spoofing,
unbounded message growth, accidental write permissions, and committed secrets.
Apply controls proportionately; do not add unrelated enterprise machinery.

Tests should materially reduce risk, especially around JSONL dispatch,
subprocess lifecycle, cancellation, Git identity, ref and commit validation,
atomic persistence, idempotency, concurrency, and regression cases. An automated
test that cannot fail for the protected defect is not a regression test.

Review feedback should be constructive and specific:

- P0: likely data loss or corruption, critical security exposure, or failure
  that makes the result unusable.
- P1: concrete correctness, robustness, architectural, or maintainability defect
  that should block acceptance.
- Lesser improvements are suggestions or questions. Do not block acceptance on
  personal style.

Search code before assuming a documented path or API exists. When stuck or
uncertain about intent, exhaust safe read-only evidence, then ask DR rather than
guessing across a consequential boundary.
