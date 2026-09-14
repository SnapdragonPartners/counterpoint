# Surviving a machine sleep during a review

Design record for
[issue 35](https://github.com/SnapdragonPartners/counterpoint/issues/35).

Status: Proposed; approved by Codex in round 3 of `fix/survive-sleep`
(2026-09-14); awaiting DR's acceptance.

Settled between Claude and Codex over three read-only rounds on 2026-09-14
before the code was written; the implementation on the same branch follows
this document. Once it lands, the contract lives in the specification and
the README, and this file keeps the evidence, the reasoning, and the
rejected alternatives.

Round 1 proposed wall-clock phase budgets beside the heartbeat. Codex's
findings on that round (recorded under "Rejected alternatives" and "Review
findings") showed that the unbudgeted phases of a call still needed a
guard of their own, and that once such a guard exists the wall-clock
budgets only make every sleep consume budget. Round 2 kept the budgets and
added one call-wide guard that measured the machine's sleep; Codex found
that its guarantee rested on a progress token the client has not been seen
to send. This round measures the call's silence toward the client instead,
which needs no token to be safe.

## Incident

On 2026-09-13 a build-capable review of `morris` `feat/ds-5b-updated-by`
(commit dd7ab4b) was submitted from a Claude Code session and the laptop lid
was closed during the turn. Claude Code's captured MCP log for that server
(`~/Library/Caches/claude-cli-nodejs/-Users-dratner-Code-morris/mcp-logs-counterpoint/2026-09-08T21-26-12-256Z.jsonl`,
times UTC) shows:

| time | event |
|---|---|
| 01:43:06 | `Calling MCP tool: review`; 30-second `still running` ticks follow |
| 01:51:35 to 02:01:06 | no ticks for 9.5 minutes (asleep) |
| 02:01:36 to 02:03:44, 02:04:14 to 02:08:21 | two more gaps, 2 and 4 minutes |
| 02:13:21 | `aborting: no response or progress notification for 1815s (idle timeout 1800s)` |
| 02:14:04 | the agent retries; 2 s later: `another review of branch ... is in progress (lock held by another process)` |
| 15:31:59 | the rerun of the same round completes in 35 s: round 2 on dd7ab4b, "the resubmitted commit is unchanged" |
| 15:52:57 | a later call; 14 s in, `STDIO connection dropped`, then `Received a response for an unknown message ID` carrying `review turn interrupted: context canceled` for that call |

The ticks are the client's own timer, and the 1815 seconds it counted
include roughly 14 minutes of sleep. Counterpoint's turn budget is twenty
minutes of a clock that stops during sleep, so at the moment the client gave
up the turn had used about 15 minutes of its budget. The review ran on,
holding the workflow lock, until the budget expired on the awake clock some
time before the morning rerun found the lock free. Its result, whatever it
was, went to a request the client had already forgotten, and one paid turn
was lost.

## Diagnosis

Three clocks are involved, and they disagree across a sleep.

- **Go timers run on the monotonic clock.** On Darwin the runtime's
  `nanotime1` reads `mach_absolute_time` (`runtime/sys_darwin.go`), which
  Apple documents as not advancing while the system sleeps (Technical Q&A
  QA1398, cited in that source). `context.WithDeadline` is no different: it
  converts the deadline with `time.Until` and arms a runtime timer
  (`context/context.go`, `WithDeadlineCause`), so a wall-clock deadline
  handed to the standard library still expires on awake time.
- **Claude Code's idle timeout runs on wall-clock time.** It counted 1815
  seconds across the gaps above. Its abort message names "response or
  progress notification" as the two things that would have kept the call
  alive, and the environment variable it names,
  `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT`, defaults to thirty minutes.
- **Counterpoint's phase budgets** (sixty seconds of setup, a twenty-minute
  turn, five seconds each for the interrupt and the child's exit) are all
  monotonic, so the documented guarantee that Counterpoint fails before the
  client does holds only while the machine stays awake. The phases between
  and around them, the lock wait, Git validation, preparing a disposable
  checkout, the checkout integrity check, and the final save, have no
  Counterpoint budget at all.

Nothing runs through a Mac's sleep, daemon or not. The design goal is to
behave correctly across one: a sleep the client survives must not cost the
review, and a sleep the client may not survive must end the call promptly
on wake, in whatever phase it lands, with the lock released and a clear
error, rather than spend a paid turn on a result nobody will receive. The
quantity the client's timer actually measures is silence: wall-clock time
since it last received a response or a progress notification for the call.
The design measures the same quantity from Counterpoint's side.

### What the client does when it gives up

Whether Claude Code's abort cancels the server-side call was checked
against the log and the SDK:

- It did not cancel it. Forty-three seconds after the abort the retry found
  the workflow lock held by the running review, and no response for the
  abandoned request was logged at that time. Had a `notifications/cancelled`
  arrived, the go-sdk would have cancelled the handler's context within the
  read loop (`mcp/transport.go`, the `canceller` preempter, calling
  `jsonrpc2.Connection.Cancel`), Counterpoint would have sent
  `turn/interrupt`, reaped the child, and released the lock within about
  ten seconds, and its error response would have reached the client, which
  logs an unmatched response as a connection error, as the 15:53 entry
  shows it doing.
- The 15:53 entry is the other half of the picture: when the connection
  itself drops, the SDK cancels every in-flight handler
  (`internal/jsonrpc2/conn.go`, the reader's shutdown), the turn is
  interrupted, and the response is written to a client that no longer wants
  it.

So cancellation reaches the review when the client sends it or closes the
connection, and the idle-timeout abort does neither: the review runs on to
its own budget and its result is discarded. That is the behavior to state in
the specification. The heartbeat below keeps the client's silence short
while Counterpoint is awake, and the guard ends the call before the
client's silence can reach its timeout, whether the silence came from a
sleep or from a request that gave Counterpoint no way to send a heartbeat.

## Invariants that stay

- The budget numbers and their meaning do not change: sixty seconds of
  setup, twenty minutes of turn, five seconds each for the interrupt grace
  and the child's exit, all of awake time. A sleep never consumes budget.
  Configurable timeouts stay deferred.
- Counterpoint fails before the client's idle timeout with a clear error.
  After this change that holds across a sleep as well as while awake; the
  precise statement is under "Outcomes across a sleep".
- Cancellation semantics are unchanged: timeout, MCP cancellation, and
  closure of Counterpoint's stdin send `turn/interrupt`, wait briefly for
  the terminal event, reap the child, and release the workflow lock after
  the child has exited. Nothing is persisted for an interrupted turn.
- Stdout carries protocol only. Progress notifications are protocol;
  diagnostics stay on stderr.
- No unbounded goroutines: one watchdog goroutine per call, ending with
  the call.

## Design

One goroutine per call, started when the MCP handler is entered and stopped
after the review has returned, wakes every thirty seconds
(`HeartbeatInterval`) and does two things: it measures the call's silence
toward the client, and it sends a progress heartbeat when it can. Both
cover every phase of the call, not just the turn: the lock wait, Git
validation, preparing a disposable checkout (a clone of a large repository
can take minutes), app-server setup, the turn, the checkout integrity
check, and the final save.

### Heartbeats

- Each tick sends `notifications/progress` for the in-flight `tools/call`,
  carrying the progress token the request supplied in its
  `_meta.progressToken`, which is how the MCP protocol and the SDK on the
  client side associate progress with a request. `progress` is the count of
  heartbeats sent, which increases as the protocol requires whatever the
  wall clock does; `total` is omitted, since the length of a review is
  unknown; `message` is a short fixed-form status with the wall-clock time
  elapsed since the call began.
- A request that carries no progress token gets no heartbeats, because a
  progress notification without a token has nothing to attach to, and one
  Info log line says so and names the bound the guard applies instead.
  Whether Claude Code 2.1.270 sends a token on `tools/call` has not been
  observed on the wire: its abort message names progress notifications as
  what would have kept the call alive, and the MCP SDK it embeds delivers
  a progress notification only to the request whose token it carries, so
  a client that could not see them would have no reason to mention them;
  but the binary is a compiled bundle and no captured log on this machine
  holds a raw request. Observing it takes a Claude Code tool call, which
  is a paid model call, so it is left for DR to authorize after install;
  the log line is where the answer will appear. The design does not
  depend on the answer: the guard is safe with or without the token, and
  the token only decides how much silence a call can afford.
- Heartbeats continue through cleanup after a lifecycle shutdown or the
  guard firing, so a client still waiting gets the error rather than a
  silence. A send that fails means the connection is gone; it is logged
  once at Warn and heartbeats stop rather than logging every thirty
  seconds.
- The go-sdk exposes the token as `req.Params.GetProgressToken()` and the
  send as `req.Session.NotifyProgress`, both on the `CallToolRequest` the
  handler already receives, so the whole mechanism lives in
  `internal/mcpserver` and the review package does not learn about MCP.

### The silence guard

- The call's silence is the wall-clock time since Counterpoint last sent
  the client something for it: the last heartbeat that was sent, or the
  request's arrival while none has been. Each tick reads the wall clock
  with the monotonic reading stripped, so the subtraction compares wall
  time and a sleep counts in full, and subtracts that mark.
- When the silence has reached `MaxSilence`, the guard ends the call: it
  cancels the review's context with a cause, `ErrSilence`, whose text
  names the silence it measured, says whether the machine slept or the
  request carried no token, and tells the agent that the client has
  probably given up and a retry starts a fresh round. Whatever phase the
  call is in ends the way a cancellation does: a turn is interrupted and
  the child reaped; a Git command, a checkout preparation, or a lock wait
  returns on its context; the workflow lock is released after the child
  has exited. The handler wraps the review's error with the cause so the
  tool error says why the call ended. Heartbeats, when there is a token,
  go on through the cleanup.
- `MaxSilence` is the client's default idle timeout less one heartbeat
  interval: 29 minutes 30 seconds. The client aborts when one of its
  periodic checks sees silence of at least its timeout; every check before
  Counterpoint's next tick sees at most the silence that tick measures; so
  as long as the tick measures less than the timeout, the client has not
  aborted, and the interval of margin covers the tick's own lateness. A
  review running on for a client that has aborted is the incident. A
  client configured with a longer idle timeout gets the same rule, since
  Counterpoint cannot see the setting.
- With heartbeats, silence at the first tick after a sleep of S is S plus
  one interval, since the ticker runs on awake time and the previous tick
  sent a heartbeat. So a sleep shorter than `MaxSilence` less one interval,
  29 minutes, is survived, and a longer one ends the call within one
  interval of wake. Without heartbeats, silence is the wall-clock age of
  the call, so the guard bounds the whole call to `MaxSilence`, sleep or
  no sleep; while awake the phases total at most 21 minutes plus Git and
  cleanup, so an awake call reaches the bound only when Git is
  pathologically slow, and a sleep is survived only while the call's
  wall-clock length stays under the bound.

### Outcomes across a sleep

With heartbeat interval H = 30 s, `MaxSilence` M = 29 min 30 s, the
client's idle timeout I = 30 min, and a sleep of S in any phase of the
call:

| case | what happens on wake |
|---|---|
| heartbeats, S < M − H (29 min) | The call continues with its budgets intact: a budget counts awake time, so the phase has exactly the time left that it had before the sleep. The next heartbeat goes out within H of wake, every check the client made saw silence under I, and the review completes normally. The workflow lock stays held for the rest of the review, as for any review. |
| heartbeats, S ≥ M − H | Within H of wake the guard ends the call: the turn, if one is running, is interrupted and the child reaped; a Git command or a lock wait returns; the lock is released after the child has exited, within about H plus ten seconds of cleanup; and the caller gets a tool error carrying `ErrSilence`. A client whose timer had not yet fired delivers that error to the agent; one whose wake-time check ran before Counterpoint's tick, possible only when S + H ≥ I, aborted and discards it, as in the incident; either way a retry starts a fresh round and nothing runs on unobserved. |
| no heartbeats | The same, with the call's wall-clock age in place of S + H: the call survives while its age stays under M, sleep included, and ends within H of the moment its age reaches M, awake or on wake. |

"Counterpoint fails first" therefore holds in every row: the client's timer
cannot fire before Counterpoint's tick measures M, and Counterpoint ends
the call at that tick. The client's copy of the error is lost only when a
sleep carried the silence past I in one step and the client's wake-time
check beat the tick, which the client's design makes unavoidable from the
server side.

### What does not change

- The idle-timeout abort itself. Counterpoint cannot make the client send
  a cancellation; it can only keep the call alive for sleeps the client
  survives, and end the call itself for sleeps it may not.
- The phase budgets, the app-server client, and the review service. The
  guard reaches them through the request context they already honor.
- The workflow-busy hint. A call the client moved to the background is
  still running and will deliver its result; that advice is now also right
  after a sleep the call survives, and wrong for at most H plus cleanup
  after one it does not.
- Whether Counterpoint should hold an idle-sleep assertion during a review
  ([issue 36](https://github.com/SnapdragonPartners/counterpoint/issues/36)).
  Lid-close sleep stays the user's choice; this record makes the sleep
  survivable rather than preventing it.

### Known limitation

The guard ends the call in whatever phase a long sleep lands, including the
final save, where it costs a completed review: the turn is done and paid
for, and the save is refused on its cancelled context. The window is the
save's lock wait, at most thirty seconds, and the next call redoes the
round. Exempting the save would need the review service to tell a sleep
guard's cancellation from the client's, which is more mechanism than the
window is worth.

## Rejected alternatives

- **Wall-clock phase budgets (round 1).** Measure the setup and turn
  budgets against the wall clock as well as the monotonic one, through a
  primitive that expires on whichever elapses first, so a budget that
  slept through its length ends on wake. Codex's round-1 review found
  that the phases without a budget were still uncovered, so a call-wide
  guard was needed regardless, and that fixing a wall-clock deadline at
  phase start makes every sleep consume budget: a nineteen-minute sleep at
  the start of a turn leaves one minute after wake, and a review needing
  two is lost to the sleep alone. With the guard in place the budgets add
  nothing but that cost. The primitive was written and tested and is
  dropped, not kept for later.
- **Raising `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT`.** Per-user configuration
  that only widens the window; a review would still run on unobserved
  after a long enough sleep.
- **`context.WithDeadline` with a wall-clock deadline.** It arms a
  monotonic timer with `time.Until`, so it expires on awake time exactly as
  `WithTimeout` does. Neither it nor any standard-library timer can measure
  a sleep; only comparing wall-clock readings across a tick can.
- **A separate goroutine for the guard and for the heartbeat.** They share
  a cadence and a wake-up; one loop measures the silence and then sends.
- **A guard that measures the machine's sleep (round 2).** The sleep
  between two ticks is the wall-clock gap less the interval, and a guard
  on it survives every sleep the client survives when heartbeats flow.
  Without a token it guarantees nothing: the client's timer counts the
  silence before the sleep as well, and a fifteen-minute turn plus a
  sixteen-minute sleep aborts the call while a sleep guard lets the review
  continue. Silence is what the client measures, so silence is what the
  guard measures; with heartbeats the two guards differ only by one
  interval.
- **Sending a heartbeat immediately on detecting a wake.** The client's
  own check runs on its own cadence, so an immediate heartbeat only
  narrows the race band by at most one interval, and the guard ends the
  call there anyway.
- **Heartbeats from inside the review service, or only during the turn.**
  The service would have to learn a progress callback and the MCP session;
  and setup, checkout preparation, and cleanup are silent phases too. The
  handler already has the request, the token, and the session.
- **Elapsed seconds as the `progress` value.** The protocol requires the
  value to increase with each notification; a backward wall-clock step
  could violate that. A counter cannot.
- **Server keepalive pings (`ServerOptions.KeepAlive`).** Pings are
  requests from server to client, not progress on the in-flight call, and
  the client's idle timer counts responses and progress. They exist to
  detect a dead peer, which is not the problem.
- **Persisting in-flight state so an aborted call's result survives.**
  That is durable completion after client disconnect, which the
  specification defers, and it addresses the discarded result rather than
  the lost heartbeat. Out of scope here.

## Review findings

Codex reviewed round 1 (commit bb8be25) read-only and reported three
findings, all on the wall-clock-budget design:

- P1: long sleeps outside the two budgeted phases were uncovered; a call
  could start or continue a paid review after the client discarded it.
  Resolved by the call-wide guard, which covers every phase, and by a test
  that ends a call from an unbudgeted phase.
- P2: a sleep consumed budget, contradicting "costs nothing but the
  sleep". Resolved by dropping wall-clock budgets: a budget counts awake
  time, so a survived sleep leaves the phase the time it had.
- P2: the lock-release bound was stated universally though a survived
  sleep leaves the lock held for the rest of the review. Resolved by
  scoping it to the row where the call ends.

Codex reviewed round 2 (commit 8cefc10) read-only and reported one:

- P1: the survival guarantee rested on a progress token the supported
  client has not been seen to send, and the documented no-token path could
  still reach the client's idle timeout from silence accumulated before
  and during a sleep. Resolved by measuring silence rather than sleep,
  which is safe with or without the token, and by a test of the no-token
  path ending at the bound; the token's presence is left as an
  observation for DR to authorize, with the design not depending on it.

## Tests

- `internal/mcpserver`, over the in-memory transport with the cadence
  injected: a call whose params carry a progress token receives at least
  three heartbeats during a blocked review, each with the request's token,
  a strictly increasing `progress`, and the status message; heartbeats
  continue through a cleanup stretched over several intervals and stop
  when the result is returned; a call without a token receives none and
  the log says why; a client that cancels the call's context (the go-sdk
  client sends `notifications/cancelled`) ends the review's context and
  gets an error result.
- The guard, with the wall clock injected: during a blocked turn with
  heartbeats, two twenty-minute jumps with heartbeats between them are
  both survived, and a jump of an hour then ends the call within a few
  intervals with a tool error carrying `ErrSilence`, the measured silence,
  and the interrupted turn, the reviewer closed; the same two twenty-minute
  jumps without a token end the call, though neither alone does, with an
  error that says no heartbeat could be sent, since silence accumulates
  when nothing is sent; an hour's jump during the final save, held off by
  a state lock the reviewer takes as it finishes, ends the call the same
  way within seconds instead of after the save's thirty-second lock wait,
  which is the unbudgeted-phase case.
- With the fake app-server subprocess in its stalled-turn scenario, driven
  through the real server and client: heartbeats arrive while the turn is
  stalled, and the client's cancellation reaches the child as
  `turn/interrupt` in the fake's event log.
- The existing timeout and cancellation tests in `internal/review` and
  `internal/appserver` are unchanged.
- Every regression test is checked by a real behavioral mutation before it
  is committed: a heartbeat that never sends, a guard that never fires, a
  guard that resets its mark without sending, a mark that never advances
  after a send, a progress value that does not increase, a tool error that
  does not name the silence, a cancellation that does not reach the review
  context.

## Amendments

**2026-09-14, after Codex's round-4 review of the implementation.** A
review that completes as the guard fires is returned as a success, not
suppressed. Codex asked the handler to check for `ErrSilence` after the
review returns regardless of its error, because a review can complete
concurrently with the watcher reaching `MaxSilence` and the client has
probably discarded the call by then. The guard exists to stop a review
from running on for a client that has given up, and every phase of the
call honors the cancelled context: the turn is interrupted, a Git command
or a lock wait returns, and the final save takes the state lock only while
the context is live. A success after the guard therefore means the review
had already completed and its record had been saved when the guard
fired; nothing ran on. Returning that result costs nothing when the
client is gone, since it discards the response either way, and is right
when the client is still waiting. Replacing it with `ErrSilence` would
tell the agent a round was lost and to start a fresh one, when the round
is persisted and the next identical request replays it. The race window is
the moments between the save and the handler's return, and the outcome
in it is the better one.

## Documentation

The README's "Timeouts" section states that the budgets count awake time,
the heartbeat cadence, the silence guard and its threshold, what the client
does on its idle timeout, and the outcomes across a sleep in the terms
above; the advice to keep the client's default idle timeout stays, with the
heartbeat as the reason it no longer needs margin over the budgets. The
tool description's "blocks until the review completes" sentence mentions
the heartbeat. The specification's concurrency section states the same
contract, its client-configuration section drops "the MVP does not send
progress notifications", its deferral list drops progress notifications,
its required tests gain the cases above, and its status list gains this
issue.
