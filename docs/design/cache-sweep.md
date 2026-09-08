# Sweeping stale build caches

Design record for
[issue 15](https://github.com/SnapdragonPartners/counterpoint/issues/15).
Settled between DR, Claude, and Codex on 2026-09-08 before the code was
written; the implementation on the same branch follows this document. Once it
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
  modification time is the last-use time. The stamp is an owned regular
  file and is never written through a link: it is inspected with `Lstat`
  first; a missing stamp is created with `O_CREATE|O_EXCL`; an existing
  regular file is opened with `O_NOFOLLOW` and rewritten through that
  descriptor, which updates its modification time; anything else (a
  symbolic link, a directory) fails `Prepare` with `ErrUnexpectedSymlink`
  or a clear error, since the scratch tree has been tampered with. Readers
  apply the same `Lstat` rule and ignore a stamp that is not a regular
  file. A stamp costs a few system calls and no walk of the cache.
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

For each direct child of the canonical root:

1. Skip anything whose name is not sixteen lower-case hex characters, a
   symbolic link, or not a directory.
2. Skip the current workflow's directory.
3. Require proof that Counterpoint made the directory: its `lock` file must
   already exist as a regular file. Every workflow directory Counterpoint
   has ever created has one, because `Prepare` creates it before anything
   else. A hash-shaped name is not provenance: `COUNTERPOINT_CHECKOUT_DIR`
   may point at an existing or shared directory, and a foreign entry there
   must never be touched, nor have a lock file created in it. The sweep
   therefore never creates a lock file; it only opens one that exists.
4. Read the stamp's modification time (an owned regular file, as above),
   falling back to the `cache` directory's; skip when there is neither
   (nothing to sweep) or when the time is within 72 hours of now.
5. Try the entry's existing scratch lock without waiting (a zero wait). If
   it is held, another process is reviewing that workflow: skip. Take it
   otherwise, so no review can start there during the removal.
6. Under that lock, re-read the stamp and fallback time and skip if it is
   now within 72 hours: a `Prepare` in another process may have refreshed
   it between step 4 and the lock, and the age decision is only valid
   under the lock.
7. Under that lock, for each of the entry's `cache`, `checkout`, `tmp`,
   `hooks`, and `used`, apply the owned-child, not-a-link rule, then rename
   the item to `trash-<random>` inside the same workflow directory. A
   rename is one atomic system call, so the item is out of use the instant
   it is decided, whatever happens next.
8. Remove every `trash-*` entry in the workflow directory with a
   context-aware walk: files are unlinked and directories removed
   post-order, checking the request context between entries, so a
   cancelled request leaves `Prepare` within a bounded number of
   operations instead of holding two scratch locks for a traversal of a
   multi-gigabyte cache. A walk interrupted by cancellation leaves a
   `trash-*` directory behind, which any later sweep of that entry removes
   the same way; a tombstone with trash is therefore still a candidate.
   Release the lock.

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
- A stamp that is a symbolic link: `Prepare` refuses with
  `ErrUnexpectedSymlink` and the link's target is neither truncated nor
  retimestamped; the sweep ignores such a stamp as absent.
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
- A removal error on one entry (a symlinked `cache` inside it) is logged
  and does not fail `Prepare` or stop the sweep of other entries.
- The sweep honors context cancellation between entries.

## Documentation

`docs/MVP.md` "Build-capable reviews" item 2 states the stamp and the 72-hour
sweep; the "Explicitly deferred" cache line and the issue-15 pointer in
"Status" go; "Required tests" gains the cases above. `README.md` replaces
"The cache has no eviction yet" with the policy, and keeps the note that
deleting directories under the scratch root by hand is safe between reviews.
