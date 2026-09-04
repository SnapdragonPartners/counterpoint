# Build-capable reviews in a disposable checkout

Plan for
[issue 12](https://github.com/SnapdragonPartners/counterpoint/issues/12).
This document is the first commit on its branch and is reviewed before any
code. When the implementation lands, the durable parts move into
`docs/MVP.md` and this file records only what was decided and why.

## Goal

Let the reviewer build and run the test suite on the exact commit under
review, because a verdict backed by `go test` is materially stronger than one
from inspection alone, while keeping every existing invariant: Counterpoint
and Codex never write to the user's repository, its index, refs, or Git
metadata, and network access stays off.

Non-goals: network for the reviewer, model-driven fixes, writing anything
back into the user's repository, and general build orchestration.

## Decisions already taken with DR

- Lint stays a developer and CI concern; it can modify code and its tool is
  fetched over the network. The reviewer runs what is available offline and
  reports lint as not run.
- The clean-worktree requirement at the branch tip is unchanged. The rule in
  `CLAUDE.md` about not touching the worktree during a review relaxes to
  "commit before calling", because the reviewer no longer reads the user's
  checkout.
- The feature is opt-in per call (see "Switch"). The default remains today's
  read-only review of the user's checkout.
- No copying of ignored tool directories into the checkout. That would put
  uncommitted executables into a review that claims to be commit-exact.

## Switch

`review` gains one optional boolean input, `build`, default `false`.

- `build: false` is exactly the current behavior: thread cwd is the user's
  worktree, sandbox `read-only`, no scratch directory.
- `build: true` selects the disposable checkout described below.

The flag is part of the request identity, so the same commit and notes with
the other flag is a new round rather than a replay. A thread may alternate
between modes across rounds; both cwd and sandbox are sent on every
`thread/start` and `thread/resume`. The prompt states which mode is in force.

## Mechanism

With `build: true`, after validation and inside the review lock:

1. Resolve the scratch root: `os.UserCacheDir()/counterpoint/checkouts`,
   created `0700`. `COUNTERPOINT_CHECKOUT_DIR` overrides it for tests and
   unusual installations, mirroring `COUNTERPOINT_STATE_FILE`. The root is
   canonicalized (symlinks resolved) before anything is created, and the
   review fails if the root lies inside the repository's worktree or common
   Git directory, or contains either of them. Otherwise an override could
   make Counterpoint write its scratch, and leave its persistent cache,
   inside the reviewed repository.
2. Derive one directory per workflow key, `<root>/<sha256 of key, 16 hex>`,
   with three children: `checkout/`, recreated every round; `cache/`,
   persistent across rounds; and `lock`, an advisory file lock on the
   directory itself.
3. Acquire `lock` with the same bounded wait as the review lock, and hold it
   through cleanup. The review lock lives beside the state file, and two
   installations with different `COUNTERPOINT_STATE_FILE` values but the
   default scratch root would hash a workflow to the same directory; the
   lock is keyed to the resource actually shared. A held lock fails the call
   with a clear message rather than queueing.
4. Remove any existing `checkout/` (a crash remnant from an earlier round),
   then populate it with Git configuration isolated from the user's:
   `GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM` pointed at `/dev/null`,
   `--template=` empty, and `core.hooksPath` set to an empty directory under
   the workflow directory. `git clone --shared --no-checkout <common dir>
   checkout` is followed by `git -C checkout checkout --detach <commit>`.
   The isolation matters because checkout runs before Codex's sandbox
   exists: a global `post-checkout` hook, a hooks path, or a filter such as
   Git LFS named by a tracked `.gitattributes` would otherwise execute
   arbitrary code, possibly with network access, on Counterpoint's behalf.
   With no filter definitions, attribute-declared filters are inert and
   pointer files stay pointer files. Measured on this repository: 0.1 s and
   500 KB, with no write into the source repository. A shared clone reads
   objects through an alternates link to the source object store; the
   dependency is documented and only lasts for the review. `--dissociate`
   would copy the store and is rejected for cost. `git worktree add` is
   rejected because it writes into the source repository's `.git`.
5. Create `checkout/.counterpoint-tmp` with mode `0700` for the reviewer's
   `TMPDIR`. Setting the variable does not create the directory, and Go and
   most test runners fail with `ENOENT` without one. If the commit tracks a
   path of that name, the review fails rather than reuse it. The
   implementation test writes a temporary file through the configured path.
6. Start or resume the thread with cwd `checkout/` and sandbox
   `workspace-write`, with the policy overrides below.
7. Run the review turn.
8. After the turn, run `git status --porcelain --untracked-files=no` in the
   checkout. Any modified tracked file means the reviewer's results may not
   describe the immutable commit; the count is reported as a warning and
   logged. Untracked files, including build output and the temp directory,
   are expected and ignored.
9. On every exit path, including timeout, cancellation, and errors after step
   4, remove `checkout/` and the hooks directory. `cache/` is kept. Release
   `lock` last.

A stable per-workflow path is chosen over a unique directory per turn. The
resource lock serializes use of the directory across processes and
installations; the thread keeps one cwd across rounds; and removing
`checkout/` before creating it cleans up a crashed round without a separate
garbage collector. Removal only ever targets a path Counterpoint composed
under its own canonical root, after checking that no component is a symlink.

## Sandbox policy and its validation

Config overrides passed on the child command line, as
`model_reasoning_effort` is today:

- `sandbox_workspace_write.writable_roots = ["<workflow>/cache"]`
- `sandbox_workspace_write.exclude_slash_tmp = true`
- `sandbox_workspace_write.exclude_tmpdir_env_var = true`
- `shell_environment_policy.set.TMPDIR = "<checkout>/.tmp"`

The effective `SandboxPolicy` reported by `thread/start` and `thread/resume`
must be exactly: type `workspaceWrite`, `networkAccess` false,
`excludeSlashTmp` true, `excludeTmpdirEnvVar` true, and `writableRoots` equal
to the single cache path. Anything else fails closed with `ErrPolicyMismatch`,
as the read-only check does today. Excluding the implicit temp roots means a
repository that happens to live under `/tmp` or `$TMPDIR` stays read-only;
the reviewer's temp files go under the checkout instead. The effective cwd
must equal the checkout path.

Whether `-c` accepts these nested keys and whether the server echoes the
excluded-root flags are assumptions to be confirmed by the live spike.

## Environment for the reviewer's commands

Counterpoint sets, through `shell_environment_policy.set`:

- `TMPDIR` inside the checkout (above).
- `COUNTERPOINT_CACHE_DIR` = the persistent cache directory.

The prompt tells the reviewer that it may build and test in its cwd, that
network is off, that `$COUNTERPOINT_CACHE_DIR` is the one writable location
outside the checkout and is meant for build caches, and gives Go as the
example: `GOCACHE=$COUNTERPOINT_CACHE_DIR/go-build GOPROXY=off`. With the
proxy off, Go's default read-only module mode fails fast on a missing
module instead of hanging. `-mod=mod` is deliberately not suggested: it lets
the toolchain rewrite `go.mod` and `go.sum` in the writable checkout, so
tests could pass against a tree that differs from the commit. Step 8 above
catches any such rewrite anyway and reports it.
Counterpoint does not set Go variables itself, so it stays language-neutral;
the recommendation lives in the prompt where it can be wrong without breaking
anything. Open question for review: whether setting `GOCACHE` directly is
worth the specificity, given that this project's own reviews are the first
consumer.

Measured on this repository: a cold-cache `go vet` plus `go test -race` in
the shared clone takes 73 s, a warm one about 30 s, against a 20-minute turn
budget.

## Prompt changes

With `build: true` the sandbox paragraph says: the cwd is a disposable copy
of the commit under review, the reviewer may build and run tests there,
network is off, lint tooling is probably unavailable and should be reported
as not run, and the original repository at `<worktree>` is read-only and is
not the place to run anything. The rules keep "inspect with Git" and add that
test results from the checkout are evidence the reviewer may cite. With
`build: false` the prompt is unchanged.

## Request identity and state

`state.Request` gains the `Build` flag. It enters the hash only when true,
as one extra length-prefixed field after the notes, so every hash computed
before this change is unchanged for `build: false` and an identical retry of
a pre-upgrade round still replays instead of starting a paid turn. A test
pins the legacy encoding with a fixture hash computed by the current code.
The persisted workflow record does not need the flag; the mode of the last
round is not used by the next one. State version stays 1 because no stored
field changes.

## Failure modes

- Scratch root overlaps the repository, is a symlink, or cannot be created:
  the review fails before cloning.
- The workflow directory's lock is held by another process: the review fails
  with a clear message, before cloning.
- Clone or checkout fails (for example a missing object): the review fails
  before contacting Codex, with the Git error, and the partial checkout is
  removed.
- The commit tracks a path named `.counterpoint-tmp`: the review fails
  before contacting Codex.
- Policy mismatch after start or resume: fail closed, remove the checkout.
- Reviewer cannot build (missing module offline, toolchain absent): the
  reviewer reports the limitation in its review, as the prompt asks; the
  verdict is still returned.
- Scratch root not creatable or a symlink: fail before cloning.

## Tests

Fake app-server: a `workspace-write` scenario that echoes the requested
policy from the config overrides it receives on its command line, and a
`workspace-wrong-roots` scenario that echoes an extra root, so the exact-set
check is exercised. The end-to-end test runs a `build: true` round and
asserts: the clone exists during the turn at the expected path (the fake
records its cwd), it is gone afterwards, the cache directory survives, the
source repository is byte-identical (`git status`, refs, and `.git` mtimes),
and a following `build: false` round resumes the same thread with the
worktree cwd.

Unit tests: scratch path derivation, symlink refusal, and rejection of a
root inside or containing the worktree or common directory; clone and detach
against a test repository; hook and filter isolation, with a fixture whose
global config sets `core.hooksPath` to a `post-checkout` hook and defines a
filter named by a tracked `.gitattributes`, asserting that neither runs and
the filtered file keeps its committed bytes; temp directory creation and a
temporary file written through it; the tracked-file check after a turn that
modified a file; lock contention between two processes on one workflow
directory; cleanup on cancellation and on a failed turn; the hash unchanged
for `build: false` against a fixture value and changed for `build: true`;
and prompt text for both modes.

Mutation checks before commit: cleanup skipped, policy check loosened, flag
appended to the hash unconditionally, hooks isolation removed, overlap check
removed.

## Live spike acceptance

Run once with DR's approval, on a fixture repository and then on this one:

1. `workspace-write` reports precisely the expected effective policy,
   including the excluded temp roots and the single writable root.
2. Tests create artifacts inside the disposable checkout and the cache.
3. An attempted write into the source repository from the reviewer fails.
4. Round two resumes the same thread with the checkout cwd and retains
   round-one context; round three with `build: false` returns to the
   worktree cwd.
5. The source worktree, index, refs, and Git metadata are unchanged.

## Documentation

`docs/MVP.md`: review target, sandbox, protocol subset, request fields, and
required tests. `README.md`: the `build` input, the scratch location, the
cost note. `CLAUDE.md`: the relaxed worktree rule and when to ask for a
build-capable review.

## Deferred, tracked separately

Cache retention. `cache/` persists per workflow with no eviction, and unlike
the state file it can be large. An issue is opened when this lands, so it is
not forgotten: candidates are removal when a workflow's state entry is
cleared, a size cap, or reuse of one cache across workflows of the same
repository.
