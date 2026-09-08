+++
title = "ADR 0001: Threat Model"
edit_date = "2026-09-08"
status = "live"
summary = "The adversaries Counterpoint defends against are untrusted inputs and the sandboxed reviewer, not a hostile process running as the user; what that rules in and out for filesystem, state, and lock handling."
+++

# 0001. Threat Model

Status: Accepted (DR 2026-09-08; Codex approval recorded in the review of
the branch that introduced this file)

## Context

`CLAUDE.md` says local software is not trusted software and lists the
controls Counterpoint applies: no command injection, no unsafe filesystem
targeting, no symlink confusion, no protocol spoofing, bounded messages, no
accidental write permissions, no committed secrets. It does not say against
whom. During the design of the build-cache sweep
([issue 15](https://github.com/SnapdragonPartners/counterpoint/issues/15),
`docs/design/cache-sweep.md`), nine review rounds turned on that question:
each mitigation for a filesystem race was answered by a further race that
only a process running as the user, with write access to Counterpoint's own
directories, could create. Such a process can delete anything of the user's
directly, and no lock or descriptor discipline can stop it renaming a
directory it may write. Without a stated threat model the rounds could not
converge.

## Decision

Counterpoint's security controls defend against two adversaries:

1. **Untrusted inputs.** Repository paths, refs, commit strings, branch
   notes, `COUNTERPOINT.md`, JSON state, model output, and child-process
   messages. These may be malformed, oversized, or crafted, and must never
   become a shell command, a path outside Counterpoint's own directories, an
   unbounded allocation, or an instruction the prompt did not intend.
2. **The reviewer.** Codex may hallucinate, misread its instructions, or be
   induced by repository content to act against them. It runs in a sandbox
   whose writable roots are the disposable checkout's cache and temp
   directories and nothing else; it must not be able to modify the reviewed
   repository, escape those roots, or make Counterpoint remove or overwrite
   anything outside them.

A hostile process running as the user is **not** an adversary. Such a
process has the same authority over the user's files as Counterpoint and
can act on them directly; defenses against it would be theater. In
particular, Counterpoint does not defend against a same-user process that
renames, replaces, or mounts over Counterpoint's own directories while
Counterpoint is using them, tampers with the state file or lock files, or
plants entries inside a workflow directory. Concurrent Counterpoint
processes are cooperating peers, serialized by advisory locks, not
adversaries.

Consequences for filesystem handling, which every design record and review
should apply rather than re-derive:

- Removal stays inside directories Counterpoint created, identified by
  markers Counterpoint wrote (a workflow directory's lock file), and by
  fixed names inside them. Names inside such a directory are
  Counterpoint's namespace; a per-entry provenance marker adds nothing.
- Links are never followed when removing or writing, because the reviewer
  can plant links inside its writable roots. Mounts are refused on the
  opened descriptor, because a FUSE mount is within the reviewer's reach on
  some platforms. Descriptor-relative operations are used where the
  reviewer's roots are traversed.
- Races that require the user's own authority to create, such as a
  directory being renamed out of the scratch root between being judged and
  being removed, are out of scope and are not mitigated or tested.
- Advisory locks are for coordination between cooperating Counterpoint
  processes, and their correctness arguments assume peers honor them.

## Consequences

- Design records cite this ADR where a threat is ruled out, and reviewers
  cite it when a finding would require defending against the user.
- `CLAUDE.md` points here from its security section, so the model is one
  sentence there and the reasoning is one place here.
- A future deployment where Counterpoint runs with authority the user lacks,
  or shares directories with other users, would have to supersede this ADR
  before it is safe.
