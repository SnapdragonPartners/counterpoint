# Surviving a machine sleep during a review

Design record for
[issue 35](https://github.com/SnapdragonPartners/counterpoint/issues/35).

Status: Proposed (round 1 of `fix/survive-sleep`, 2026-09-14)

To be settled between DR, Claude, and Codex before the code is written; the
implementation on the same branch follows this document. Once it lands, the
contract lives in the specification and the README, and this file keeps the
evidence, the reasoning, and the rejected alternatives.

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
  client does holds only while the machine stays awake.

Nothing runs through a Mac's sleep, daemon or not. The design goal is to
behave correctly across one: a sleep that leaves time in the budget must not
cost the review, and a sleep that does not must end the review promptly on
wake, with the lock released and a clear error.

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
the specification, and the heartbeat below is what keeps the abort from
happening in the first place.

## Invariants that stay

- The budget numbers do not change: sixty seconds of setup, twenty minutes
  of turn, five seconds each for the interrupt grace and the child's exit.
  Configurable timeouts stay deferred.
- Counterpoint fails before the client's idle timeout with a clear error.
  After this change that holds across any sleep shorter than the client's
  idle timeout less one heartbeat interval, not only while awake; the
  precise statement is under "Outcomes across a sleep".
- Cancellation semantics are unchanged: timeout, MCP cancellation, and
  closure of Counterpoint's stdin send `turn/interrupt`, wait briefly for
  the terminal event, reap the child, and release the workflow lock after
  the child has exited. Nothing is persisted for an interrupted turn.
- Stdout carries protocol only. Progress notifications are protocol;
  diagnostics stay on stderr.
- No unbounded goroutines: one heartbeat goroutine per call and one
  watchdog goroutine per budget, each ending with what it serves.

## Design

### Heartbeats for the life of the call

The MCP handler sends `notifications/progress` for the in-flight `tools/call`
every thirty seconds (`HeartbeatInterval`) from the moment the handler is
entered until the result is returned. That covers every silent phase, not
just the turn: the lock wait, Git validation, preparing a disposable
checkout (a clone of a large repository can take minutes), app-server
setup, the turn, and cleanup and the final save after it.

- The notification carries the progress token the request supplied in its
  `_meta.progressToken`, which is how the MCP protocol and the SDK on the
  client side associate progress with a request. `progress` is the count of
  heartbeats sent, which increases as the protocol requires whatever the
  wall clock does; `total` is omitted, since the length of a review is
  unknown; `message` is a short fixed-form status with the wall-clock time
  elapsed since the call began.
- A request that carries no progress token gets no heartbeats, because a
  progress notification without a token has nothing to attach to, and one
  Info log line says so. That line is also the confirmation that Claude
  Code sends a token on `tools/call`: its abort message names progress
  notifications as what would have kept the call alive, and the MCP SDK it
  embeds routes them by token, but the token itself has not been observed
  on the wire, so the first live call after install is the check.
- The heartbeat is sent on a context detached from the request's
  cancellation, so it continues through cleanup after a cancellation or a
  lifecycle shutdown. A send that fails means the connection is gone; it is
  logged once at Warn and the heartbeat stops rather than logging every
  thirty seconds.
- The go-sdk exposes the token as `req.Params.GetProgressToken()` and the
  send as `req.Session.NotifyProgress`, both on the `CallToolRequest` the
  handler already receives, so the heartbeat lives entirely in
  `internal/mcpserver` and the review package does not learn about MCP.

### Wall-clock budgets

A new package, `internal/wallclock`, provides the one budget primitive the
other packages use:

```go
type Clock struct {
    Now           func() time.Time // nil means time.Now
    CheckInterval time.Duration    // zero means CheckInterval, thirty seconds
}

func (c Clock) WithTimeoutCause(parent context.Context, d time.Duration, cause error) (context.Context, context.CancelFunc)
```

The returned context ends, with `cause` as its cause, at the earlier of two
moments: when `d` has elapsed on the monotonic clock, or when `d` has
elapsed on the wall clock. The wall clock is read with the monotonic reading
stripped (`Time.Round(0)`), so the comparison is not silently monotonic
again. A single goroutine holds a runtime timer for the monotonic bound and
re-arms a second timer for the smaller of the remaining wall-clock time and
`CheckInterval`, so after a sleep the wall-clock check runs within thirty
seconds of wake and, while awake, the budget expires at the same instant it
does today. Cancelling the parent or calling the returned cancel function
ends the goroutine.

- The monotonic bound is a cap, not a fallback: a backward step of the wall
  clock (a time-zone change does not move it, but an NTP correction can)
  cannot extend a budget beyond `d` of awake time. A forward step shortens
  it, which errs on the safe side.
- The cause is what callers already branch on: the review service checks
  `context.Cause` for `ErrSetupTimeout` and `ErrTimeout`. What changes is
  `ctx.Err()`, which is `context.Canceled` on expiry rather than
  `context.DeadlineExceeded`, because the standard library offers no way to
  end a context with the deadline error from outside its own timer. The
  app-server client therefore reports `context.Cause(ctx)` rather than
  `ctx.Err()` in the interrupted-turn error, so a timed-out turn reads
  "review turn interrupted: review timed out after 20m0s" instead of
  "context canceled". Caller-supplied deadlines are unaffected: for a plain
  `context.WithTimeout` the cause is `DeadlineExceeded`, and the existing
  tests that assert it keep passing.
- The review service holds a `wallclock.Clock` so tests inject a clock that
  jumps; production uses the zero value. The setup budget and the turn
  budget are created through it. The app-server client's two cleanup
  budgets, the interrupt grace and the child's exit grace, use the same
  primitive with the real clock. For a five-second budget the check
  interval clamps to the budget itself, so the primitive degenerates to
  today's monotonic timer plus one wall-clock check at its end; a sleep
  inside a five-second window extends that window by the sleep and by no
  more, which is the behavior those budgets have now.

### Outcomes across a sleep

With heartbeat interval H = 30 s, wall-clock check interval C = 30 s, the
client's idle timeout I = 1800 s, and R the budget remaining in the current
phase when the machine sleeps for S seconds:

| case | what happens on wake |
|---|---|
| S < R | The phase continues. The next heartbeat goes out within H of wake, so the client saw silence for at most S + H, which is under I whenever R + H is: the turn budget leaves R at most 1200 s, so a sleep of up to twenty minutes in the turn, or a minute in setup, costs nothing but the sleep. |
| R ≤ S < I − C | The budget has expired. Within C of wake the watchdog ends the phase: `turn/interrupt` is sent, the child is reaped, the lock is released, and the caller gets `ErrTimeout` (or `ErrSetupTimeout`) naming the budget. The client, still waiting, delivers that error to the agent. |
| S ≥ I − C | The client may abort on wake before the first heartbeat, since its own check also runs on a cadence; whether it does depends on which timer fires first. Counterpoint still ends the phase within C of wake and releases the lock; if the client has already aborted, it discards the response, as it did in the incident. A retry starts a fresh round, and the busy hint's "do not retry a backgrounded call" advice is right for at most C plus cleanup after wake. |

In every row the lock is free within about C plus ten seconds of cleanup
after wake, and no review runs on unobserved. The guarantee "Counterpoint
fails first" therefore holds for every sleep shorter than I − C; beyond that
the two sides fail together and the client's copy of the error is lost,
which the client's own design makes unavoidable from the server side.

### What does not change

- The idle-timeout abort itself. Counterpoint cannot make the client send
  a cancellation; it can only keep the call alive so the abort does not
  fire, and end the review promptly when the budget is gone.
- The workflow-busy hint. A call the client moved to the background is
  still running and will deliver its result; that advice is now also right
  after a short sleep, since the call survives it.
- Whether Counterpoint should hold an idle-sleep assertion during a review
  ([issue 36](https://github.com/SnapdragonPartners/counterpoint/issues/36)).
  Lid-close sleep stays the user's choice; this record makes the sleep
  survivable rather than preventing it.

## Rejected alternatives

- **Raising `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT`.** Per-user configuration
  that only widens the window; the budget would still outlive it across a
  long enough sleep, and the review would still run on unobserved.
- **`context.WithDeadline` with a wall-clock deadline.** It arms a
  monotonic timer with `time.Until`, so it expires on awake time exactly as
  `WithTimeout` does. This is why a new primitive is needed at all.
- **A ticker-only wall-clock budget without the monotonic cap.** A
  backward clock step would extend the budget by the size of the step, with
  no bound. Keeping the runtime timer costs one channel in the select.
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
- **Sending a heartbeat immediately on detecting a wake.** The watchdog
  could notice a wall-clock jump and heartbeat at once, but the client's
  own check runs on its own cadence, so this narrows the last row of the
  outcomes table by at most one interval and adds a second reason to send.
  Not worth it.
- **Persisting in-flight state so an aborted call's result survives.**
  That is durable completion after client disconnect, which the
  specification defers, and it addresses the discarded result rather than
  the lost heartbeat. Out of scope here.

## Tests

- `internal/wallclock`: the budget expires at `d` of monotonic time with
  the cause; it expires before `d` of monotonic time when the injected wall
  clock jumps past the deadline, within one check interval; a parent
  cancellation ends it with the parent's cause; the returned cancel
  function ends it with `context.Canceled`; a backward wall-clock jump does
  not extend it past `d` of monotonic time; the goroutine ends on every
  exit.
- `internal/review`: the setup budget across an injected clock jump during
  a stalled thread start and a stalled resume fails with `ErrSetupTimeout`,
  closes the reviewer, and releases the workflow lock, well before the
  budget's monotonic length; the turn budget across a jump during a
  blocked turn fails with `ErrTimeout` and `ErrTurnInterrupted`; the
  existing timeout and cancellation tests are unchanged.
- `internal/mcpserver`: over the in-memory transport, a call whose params
  carry a progress token receives heartbeats at the injected cadence, at
  least three during a blocked review, each with the request's token, a
  strictly increasing `progress`, and the status message; a call without
  a token receives none and the log says why; the heartbeat runs through
  cleanup and stops when the result is returned; a client that cancels the
  call's context (the go-sdk client sends `notifications/cancelled`) ends
  the review's context and gets an error result. With the fake app-server
  subprocess in its stalled-turn scenario, driven through the real server
  and client: heartbeats arrive while the turn is stalled, and the
  cancellation reaches the child as `turn/interrupt` in the fake's event
  log.
- `internal/appserver`: the existing cancellation and grace tests are
  unchanged. The two cleanup budgets use the shared primitive with the real
  clock and are not exercised across a clock jump in place; the
  primitive's own tests cover the jump, and the notes say so.
- Every regression test is checked by a real behavioral mutation before it
  is committed: a heartbeat that never sends, a budget that ignores the
  wall clock, a cancellation that does not reach the review context.

## Documentation

The README's "Timeouts" section states the budgets as wall-clock, the
heartbeat cadence, what the client does on its idle timeout, and the
outcomes across a sleep in the terms above; the advice to keep the client's
default idle timeout stays, with the heartbeat as the reason it no longer
needs margin over the budgets. The tool description's "blocks until the
review completes" sentence mentions the heartbeat. The specification's
concurrency section states the same contract, its client-configuration
section drops "the MVP does not send progress notifications", its deferral
list drops progress notifications, its required tests gain the cases
above, and its status list gains this issue.
