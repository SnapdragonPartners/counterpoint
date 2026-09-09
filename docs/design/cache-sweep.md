# Sweeping stale build caches

Design record for
[issue 15](https://github.com/SnapdragonPartners/counterpoint/issues/15).

Status: Accepted (Codex round 12 of `feat/cache-sweep` and DR, 2026-09-08)

Settled between DR, Claude, and Codex on 2026-09-08 over twelve review
rounds before the code was written; the implementation on the same branch
follows this document. Once it
lands, the contract lives in `docs/MVP.md` and this file keeps the evidence,
the reasoning, and the rejected alternatives.

## Problem

A build-capable review keeps a per-workflow cache directory under the scratch
root, `os.UserCacheDir()/counterpoint/checkouts/<hash>/cache`, so the
reviewer's build cache survives between rounds. The checkout beside it is
removed after every review; the cache is not, and nothing else removes it.
Branches are merged and their workflows go stale, but their caches stay.

Measured on DR's machine on 2026-09-08, after four days of use:

| workflow directories | total | largest | smallest |
|---|---|---|---|
| 13 | 10 GB | 1.5 GB | 66 MB |

Every one of those branches but two is merged. The growth is linear in the
number of branches ever reviewed with `build: true`, and a Go build cache for
a mid-sized module is around a gigabyte, so the root reaches tens of
gigabytes within weeks of normal use.

## Invariants that stay

- Counterpoint never removes anything outside its own scratch root, never
  follows a symbolic link when removing, and removes only paths it created.
  The existing `removeOwned` rule, that a removal target is a direct child of
  a workflow directory and not a link, holds for the sweep.
- Recovery must not delete or overwrite another workflow's active state. A
  cache in use by a review in another process is active state for the
  length of that review.
- Locks are never waited on out of order. The scratch directory lock is
  taken under the review's workflow lock; a sweep must not wait on another
  workflow's scratch lock while holding its own.

## Design

### Policy: age since last use

A workflow's cache is kept while the workflow has been used within the last
72 hours and swept afterwards. Use means a build-capable review of that
workflow reached `scratch.Prepare`. Nothing else bounds the root.

- A stamp file, `<workflow dir>/used`, is written by `Prepare` once the
  workflow's lock is held, before anything else is created. Its
  modification time is the last-use time. The stamp is never written in
  place, so no existing file is ever opened for writing: `Prepare` creates
  a fresh file exclusively (`O_CREATE|O_EXCL`) under a temporary name in
  the workflow directory, which is already known not to be a link, and
  renames it over `used`. A rename replaces the directory entry whatever it
  was: a symbolic link is replaced, not followed, and a hard link to a file
  outside the tree is unlinked from this directory while the outside file
  keeps its content and its other names. Tampered scratch state therefore
  cannot make the stamp write reach anything outside the workflow
  directory, and no descriptor-level validation is needed for writing.
  Readers `Lstat` the stamp and use its modification time only if it is a
  regular file; a tampered stamp can only make a cache look fresher or
  staler than it is, which changes when Counterpoint's own cache is
  rebuilt and nothing else. A stamp costs a few system calls and no walk
  of the cache.
- A workflow directory from before this change has no stamp. Its last use is
  approximated by the modification time of its `cache` directory, which is
  close to the workflow's first build rather than its last, so an active
  cache that predates the upgrade may be swept once and rebuilt cold. That
  is a one-time cost of a cold build per active branch, and the stamp
  written on that rebuild prevents a repeat.

72 hours is chosen against DR's usage: a branch is typically opened,
reviewed over hours, and merged within a day or two, so a workflow idle for
three days is nearly always merged or abandoned. A branch resumed after
longer pays one cold build, the same cost as a new branch. The number is a
constant, not configuration; the MVP defers configuration, and one number
that matches the working rhythm is enough.

### Mechanism: sweep at the start of each build-capable review

`scratch.Prepare` sweeps the root after the current workflow's directory is
locked and stamped and before the clone, so a crashed or cancelled round
cannot skip it and the root is bounded whenever it is being used. The sweep
is bounded by the request context and by the number of entries in the root;
it reads one directory and stats two files per entry, without walking
caches, so it costs milliseconds and never blocks on a lock.

The root is opened once as a descriptor, `open(root, O_RDONLY|O_DIRECTORY|
O_NOFOLLOW|O_CLOEXEC)`, and its entries are listed through it. For each
entry, every operation from the ownership check to the last removal is
performed relative to a descriptor for that entry, so nothing that happens
to the path in the meantime can substitute a different directory:

1. Skip anything whose name is not sixteen lower-case hex characters.
2. Skip the current workflow's directory.
3. Open the entry relative to the root descriptor with
   `openat(root, name, O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC)`; a link
   or a non-directory fails to open and is skipped. `fstat` the descriptor
   and keep its device and inode: this descriptor, not the name, is the
   candidate from here on. If the entry is renamed away and replaced after
   this point, the walk still refers to the directory it opened, and the
   replacement is a fresh entry for a later sweep.
4. Require proof that Counterpoint made the directory: `fstatat(entry,
   "lock", AT_SYMLINK_NOFOLLOW)` must report a regular file. Every workflow
   directory Counterpoint has ever created has one, because `Prepare`
   creates it before anything else. A hash-shaped name is not provenance:
   `COUNTERPOINT_CHECKOUT_DIR` may point at an existing or shared
   directory, and a foreign entry there must never be touched, nor have a
   lock file created in it. The sweep never creates a lock file.
5. Decide what the entry qualifies for, reading through the descriptor
   with `fstatat(..., AT_SYMLINK_NOFOLLOW)`. It qualifies for **trash
   removal** if it contains any `trash-*` entry, whatever its age. It
   qualifies for **sweeping** if its last use, the stamp's modification
   time when the stamp is a regular file or, with no stamp, the `cache`
   directory's, is more than 72 hours ago. An entry that qualifies for
   neither is skipped; a tombstone with nothing in it costs one directory
   read. Trash alone never authorizes renaming a live path: an entry with
   trash but a fresh stamp has only its trash removed.
6. Open the lock relative to the entry descriptor with
   `openat(entry, "lock", O_RDWR|O_NOFOLLOW|O_CLOEXEC)` and no `O_CREATE`,
   validate the opened descriptor with `fstat`: a regular file with a link
   count of one and the device and inode step 4 saw. Then try
   `flock(LOCK_EX|LOCK_NB)`. If the open fails, validation fails, or the
   lock is held, skip: the entry is either being reviewed by another
   process or has been tampered with, and in neither case is it
   Counterpoint's to touch now. This is a new `state` helper beside
   `AcquireLock`, which keeps its create-and-wait behavior for callers that
   own the path.
7. Under that lock, repeat step 5's reads and decide again: a `Prepare` in
   another process may have refreshed the stamp before the lock was taken,
   and the age decision is only valid under the lock. Trash removal needs
   no re-check.
8. Under that lock, if the entry qualifies for sweeping, for each of
   `cache`, `checkout`, `tmp`, `hooks`, and `used` that `fstatat` reports
   as present and not a link, rename it to `trash-<random>` with
   `renameat(entry, name, entry, trash)`. A rename is one atomic system
   call that does not follow links, so the item is out of use the instant
   it is decided, whatever happens next.
9. Under that lock, remove every `trash-*` entry with the descriptor
   traversal described below, starting from the entry descriptor, checking
   the request context between entries, so a cancelled request leaves
   `Prepare` within a bounded number of operations instead of holding two
   scratch locks for a traversal of a multi-gigabyte cache. A walk
   interrupted by cancellation leaves a `trash-*` directory behind, which
   step 5 makes a candidate for the next sweep of any build-capable
   review. Release the lock and close the descriptor.

`Prepare`'s own stamp write and lock stay path-based: `Prepare` creates the
workflow directory, verifies it is not a link, and holds its lock before
writing the stamp, and the stamp write is an exclusive create plus a rename
inside that directory. The sweep is the only code that judges directories
it did not just create, which is why it alone works through descriptors.

The threat model is ADR 0001 (`docs/adr/0001-threat-model.md`): untrusted
inputs and the sandboxed reviewer, not a hostile process running as the
user. The descriptor discipline above is what the reviewer's reach
requires, since it can plant links and, on some platforms, mounts inside
its writable roots; races that need the user's own authority, such as a
workflow directory renamed out of the root between being judged and being
removed, are out of scope per that ADR and are neither mitigated nor
tested.

Ownership is established once, for the workflow directory, by step 4. Inside
a directory Counterpoint created, the fixed names `checkout`, `tmp`,
`hooks`, `cache`, `used`, and `trash-*` are Counterpoint's namespace, and
the sweep removes them by name exactly as `Prepare` and `Close` already
remove `checkout`, `tmp`, and `hooks` by name on every round. A marker
proving that a particular trash entry was Counterpoint's would be a file
written into that same directory and could be forged by anyone able to
plant a trash entry there, so it would prove nothing more than the lock
file already does. The reviewer's sandbox cannot plant anything: its
writable roots are `cache` and `tmp` only, never the workflow directory
itself. What the rules guarantee, for a planted entry as for a real one, is
that removal never leaves the workflow directory, by two mechanisms:

- **Descriptor traversal.** The trash walk never resolves a path more than one
  component long. Starting from a descriptor for the workflow directory, each
  child directory is opened with `openat(parent, name,
  O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC)`, so a name that has become a
  symbolic link, wherever it points, fails to open and is unlinked as a link
  instead of being entered; entries are listed through the opened descriptor;
  files and links are removed with `unlinkat(parent, name, 0)` and directories,
  post-order, with `unlinkat(parent, name, AT_REMOVEDIR)`, each relative to the
  descriptor of the directory that was listed. Nothing in the walk can be
  redirected by a link planted or swapped while it runs, inside or outside the
  workflow directory, because no step follows one. This is the strategy
  `os.RemoveAll` itself uses on Unix, with two additions: the request context
  is checked between entries, and the device check below runs on each opened
  descriptor, with the entry descriptor from step 3 as the baseline. `os.Root`
  is not used for the walk: its sub-root open follows a link that stays inside
  the root, which is exactly the swap that must not be followed. The primitives
  come from `golang.org/x/sys/unix`, already an indirect dependency of the
  module, and the walk is Unix-only like the lock implementation beside it.
- **Device boundary, bound to the handle.** After each `openat`, the walk
  `fstat`s the opened descriptor, confirms it is a directory, and compares its
  device id with the entry descriptor's from step 3. Only if they match does
  the walk list and remove through that descriptor; otherwise it is closed and
  the directory is left in place and logged. Checking the opened descriptor
  rather than a path closes the window between a check and an open: a mount
  placed over the directory after the descriptor was opened does not change
  what the descriptor refers to, and one placed before it is what the `fstat`
  sees. This refuses every mount a same-user process can make visible to
  Counterpoint: a FUSE or disk-image mount has its own device id, and a bind
  mount made in an unprivileged user namespace on Linux exists only in that
  namespace's mount table and is invisible to Counterpoint's process. A bind
  mount in Counterpoint's own mount namespace needs privileges the user does
  not have and is outside the threat model, as it is for every file operation
  in this package.

The same-device decision is a predicate the walk takes, called with the
`fstat` result of the opened handle and nothing else, so tests exercise the
refusal without creating a mount and can verify the ordering: the predicate
sees the inode of the directory that was opened, and a directory it
refuses is never listed. The mapping of that predicate to the real device
id is stated beside the implementation as the untestable part. The
existing `removeOwned` keeps `os.RemoveAll` for the small per-round
directories; making it rooted is a separate cleanup and is tracked in the
issue for this change.

A sweep failure on one entry is logged and the sweep continues; a sweep
never fails the review, except that cancellation of the request ends it
like every other stage. The log records the number of workflows swept.
Sizes are not reported, since measuring them would mean an extra walk.

The scratch package's existing `removeOwned`, built on `os.RemoveAll`,
stays for the per-round removal of the checkout, temp, and hooks
directories, which are small; the sweep's trash removal is the only
context-aware traversal, because caches are the only large trees.

### What the sweep does not do

- It does not bound the root by size. With the age policy, the root holds
  at most the caches of workflows built in the last three days, which at
  DR's pace is a handful of gigabytes; a size cap would need a walk of every
  cache on every review, or bookkeeping that is wrong as soon as the
  reviewer writes to a cache directly.
- It does not touch the state file. Stale workflow records there are small
  and are a separate matter (a future reset operation).
- It does not run for read-only reviews, which touch no scratch, nor at exit.

## Rejected alternatives

- **Remove a workflow's scratch directory when its state entry is
  cleared.** There is no reset operation yet, and merged branches leave
  their entries in place; this would evict nothing in practice.
- **A size cap with least-recently-used eviction.** Needs the size of every
  cache on every review, a walk of millions of small files across the root,
  or a bookkeeping file the reviewer's own writes would invalidate.
- **One cache per repository shared by its branches.** Halves the growth for
  repositories with several live branches but makes concurrent reviews of
  two branches write the same cache, which Go's build cache tolerates but
  other toolchains may not, and it still needs an age policy for
  repositories that go quiet.
- **Sweeping at the end of a review.** Skipped by a crash or cancellation,
  and gives the reviewer's process a longer tail after its verdict.
- **Deleting the whole workflow directory including the lock file.** The
  unlink race above; the tombstone is cheaper than the inode check that
  would make deletion safe.

## Tests

- A workflow directory with a stamp older than 72 hours: `cache`,
  `checkout`, `tmp`, `hooks`, and `used` removed; the directory and its lock
  file remain; a later `Prepare` for that workflow succeeds with a fresh
  cache and a new stamp.
- A lookalike: a sixteen-hex directory under the root with an old `cache`
  and no lock file is untouched, and no lock file is created in it.
- A stamp that is a symbolic link, and a stamp that is a hard link to a
  file outside the tree: `Prepare` replaces the entry with a fresh regular
  file and the link's target keeps its content, modification time, and
  link count minus one; the sweep treats a link stamp as absent.
- A lock file replaced by a symbolic link, or hard-linked to an outside
  file, between the ownership check and the lock: the sweep skips the
  entry, opens nothing outside the tree, and removes nothing.
- A candidate directory renamed away and replaced by another directory,
  or by a link, after it was listed: the sweep either works on the
  directory it opened or skips the entry; the replacement is untouched.
- Cancelled trash is reachable: an entry with leftover `trash-*` and no
  stamp or cache is still visited, and only its trash is removed.
- The device boundary: a directory inside trash for which the same-device
  predicate is false is left in place with its contents, its siblings are
  still removed, and the walk reports what it skipped. The predicate is
  called with the `fstat` of the opened handle, checked by asserting the
  inode it sees is the directory's, and a refused directory is never
  listed.
- A symbolic link inside trash pointing outside the workflow directory is
  unlinked and its target is untouched; so is one pointing at a sibling
  subtree inside the workflow directory, such as the live `cache`, which
  keeps its contents.
- Freshness under the lock: a stamp refreshed after the age check but
  before the lock is taken leaves the entry untouched.
- Cancellation during the removal of a large candidate returns promptly,
  leaves a `trash-*` directory and the tombstone, releases both locks, and
  a later sweep removes the trash.
- A stamp younger than 72 hours: untouched.
- No stamp: the `cache` directory's modification time decides; neither
  stamp nor cache: untouched.
- The current workflow: untouched however old its stamp.
- An entry whose lock is held elsewhere: untouched, and the sweep does not
  wait for it.
- A symbolic link in the root, an entry with a non-hex name, and a regular
  file: untouched.
- `Prepare` writes the stamp before creating the checkout and refreshes it
  on every round.
- A removal error on one entry is logged and does not fail `Prepare` or
  stop the sweep of other entries. The failing boundary is deterministic: a
  `trash-*` directory whose mode denies write permission, so `unlinkat` of
  its contents fails for the unprivileged test process; the test restores
  the mode afterwards. A symlinked item cannot serve here, since the rename
  step skips links before anything can fail.
- The sweep honors context cancellation between entries.

## Documentation

`docs/MVP.md` "Build-capable reviews" item 2 states the stamp and the 72-hour
sweep; the "Explicitly deferred" cache line and the issue-15 pointer in
"Status" go; "Required tests" gains the cases above. `README.md` replaces
"The cache has no eviction yet" with the policy, and keeps the note that
deleting directories under the scratch root by hand is safe between reviews.
