+++
title = "ADR 0002: Evidence of Acceptance Within a Branch"
edit_date = "2026-09-08"
status = "live"
summary = "Within a branch, the strongest evidence that DR accepted an ADR or design record is its status line in a commit an earlier review round approved; the merge is the independent record. No in-branch artifact can distinguish DR's hand from Claude's, because commits carry DR's identity and keys either way."
+++

# 0002. Evidence of Acceptance Within a Branch

Status: Proposed (DR decided 2026-09-08; awaiting Codex approval in the
review of the branch that introduces this file)

## Context

`CLAUDE.md` accepts work only after both Codex and DR approve it, and the
rule introduced with this ADR makes accepted ADRs and design records
binding on later review rounds. During the review of that rule, Codex asked
three times for an auditable artifact of DR's acceptance before a record
could bind, on the ground that a status line saying "DR accepted" is
written by the author. The request could not be met, and the reason is
structural rather than a matter of effort.

## Decision

Within a branch, the evidence that DR accepted an ADR or design record is
its status line naming Codex and DR with a date, present in a commit that
an earlier review round approved. That is the strongest evidence that can
exist in a branch:

- Every commit on Claude's branches carries DR's author identity and is
  made with DR's keys, because Claude runs as DR's user. A commit DR makes
  by hand is indistinguishable from one Claude makes. No signature,
  authorship, or trailer check can separate them, and adding one would be
  theater.
- The reviewer runs in a sandbox without network access, so an issue
  comment, a pull request approval, or any other out-of-repository record
  of DR's decision is invisible to it.
- ADR 0001 places the user outside the threat model. The reviewer does not
  distinguish the user from the agent, and is not asked to.

The independent record of DR's acceptance is the merge: DR alone merges,
and everything on the primary branch has passed that gate. A record on the
primary branch is accepted; a record only on a branch is accepted for the
purposes of that branch's later rounds once its status line has been
through an approved round, and is finally accepted when the branch merges.

What the status-line standard does guarantee is sequencing: the claim of
acceptance was in front of the reviewer, as repository content, at least
one round before it could bind anything, so a reviewer that doubts it can
say so before the record silences a later finding.

## Consequences

- `CLAUDE.md` states the status-line convention and cites this ADR for
  why nothing stronger exists in a branch.
- Reviewers verify the status line and its presence in an earlier approved
  commit; they do not ask for an artifact of DR's acceptance beyond it.
- If Counterpoint ever runs with an identity distinct from the user's, or
  gains a way for the reviewer to see out-of-repository records, this ADR
  should be revisited.
