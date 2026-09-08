+++
title = "Architecture Decision Records"
edit_date = "2026-09-08"
status = "live"
summary = "Index of Counterpoint ADRs: cross-cutting decisions that later design work and reviews build on rather than re-litigate."
+++

# Architecture Decision Records

This directory holds Counterpoint's architecture decision records, in one
numbered sequence. An ADR records a decision that cuts across issues, such as
the threat model or documentation authority, so that later design records,
reviews, and code build on it instead of re-deriving it. Per-issue design
work stays in `docs/design/`; the accepted product contract stays in
`docs/MVP.md`.

Lifecycle: Proposed, then Accepted with Codex and DR approval, then
Superseded or Rejected. A superseding ADR names the one it replaces. An ADR
is amended in place when a decision is refined rather than replaced, with the
amendment dated in its status line.

Front matter follows the Maestro convention: `title`, `edit_date`, `status`
(`live`, `deprecated`, or `archive`), and a one-paragraph `summary`.

## ADRs

| ADR | Title | Status | Summary |
| --- | --- | --- | --- |
| [0001](0001-threat-model.md) | Threat Model | Proposed | The adversaries Counterpoint defends against are untrusted inputs and the sandboxed reviewer, not a hostile process running as the user; what that rules in and out for filesystem, state, and lock handling. |
