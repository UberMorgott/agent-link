# Durable delivery lease (PR-B)

Goal: a message this node must deliver is never stuck on a dead or abandoned
session and never needs a person to nudge delivery. Every hand-off to a
session, a launch or a seat is a lease with a deadline; only proof that the
turn started makes it `running`; everything else falls back to `pending`
and goes to the next candidate. Code: `internal/node/lease.go`.

## Record

One record per message per recipient on this node: key `<msg id>` for the
node's own sessions, `<seat>/<msg id>` for a seat's pending message.
Persisted in `leases.json` (data dir) next to `sessions.json`.

| field | meaning |
| --- | --- |
| `state` | `pending`, `leased`, `running`, `acked`, `retry`, `failed` |
| `owner` | session id, `launch <area>`, or `seat:<id>` |
| `via` | `hook`, `inbox`, `queue`, `waiter`, `launch`, `seat` |
| `token` | wake token the prompt carries (`WakeMarker`) |
| `attempts` | automatic leases so far (every via but `hook`) |
| `fails` | failed leases per owner |
| `deadline` | leased: proof due by; running: ack due by |
| `next_at` | retry: earliest next node wake (bounded backoff) |

## Transitions

| from -> to | trigger |
| --- | --- |
| pending/retry -> leased | a claim is granted (`claimLocked`: hook claim, waiter claim, node wake), `launchClaim`, `claimTurn` (seat) |
| leased -> running | proof: the owner acks it (hook saw its wake token in prompt/transcript, PR #16), a desktop/seat first turn reports its session (`started`), or `LeaseStart` (Codex app-server `turn/started`, PR-D) |
| running -> acked | ack of the message (`Ack`); the lease is idempotent by key: acking again changes nothing |
| leased/running -> retry | `LeaseRevoke`: wake/queue post failed, session ended (`EndSession`) or no longer live, deadline passed without proof (`leaseSweep`; never for `inbox`/`queue`/`waiter`, see below), launch failed before its turn started |
| retry/leased(other) -> running | late proof of an owner whose lease was revoked, while nobody acked it |
| running(hold) -> failed | a launch's ack kept failing for `leaseHoldMax` (1 h): ack job and claim dropped |
| running -> failed | a launch's first turn started and failed (may have acted in part: `needs_human`) |
| retry -> failed | `attempts >= maxLeaseAttempts` (5): `needs_human` reported once |
| leased -> failed | `queued_stale`: a queued/inbox/waiter wake its session stayed silent on for `queuedOwnerMax` (accepted, never proven: no automatic requeue, `needs_human`) |
| leased(queue pointer) -> leased(hook) -> acked | the session's fetch (`pointerFetch`: its own `unread` listing claims as a hook and acks at once what its answer returns) or its hook's delivery |
| (none) -> leased(queue pointer) without a prompt | a message that came while a pointer is in the session's queue, not shown yet: leased under that pointer's token (one pointer at a time) |
| leased(queue pointer, `shown`) -> retry | `pointer_unfetched`: the pointer's marker was in the rollout for `pointerFetchGrace` (2 min) and nothing fetched; `held:wake_unfetched`, a new pointer may follow (`handedTo` no more) |

Rules:

- `running` only on proof. Queue/RPC/inbox accept is `leased`, never more.
- A wake that put the message into a session (`inbox` post accepted, `codex
  queue`, waiter output) cannot be withdrawn: the lease stays that session's
  (`routeOf` routes to it) until proof, or evidence the channel dropped it
  (session end/gone, post/queue/proxy error, thread gone: `LeaseRevoke`).
  A deadline never reassigns it; waking the same session again is no failure.
  Only a session silent on it for `queuedOwnerMax` (30 min without a hook
  event since the lease, heartbeats aside) loses it (`queued_stale`: a
  failure of that owner, the claim and token kept for a late proof), so a
  session whose waiter hit its wake cap but keeps heartbeating cannot hold it
  for the whole registration. The lease is then `failed` (`needs_human`), not
  `retry`: the prompt was accepted and never proven, and `codex queue` has no
  idempotency key, so queuing it again may run it twice. A hook still shows
  the message at a session's next event.
- A `queue` wake is not queued behind a running turn: a Codex thread whose
  hooks are silent stays `idle` for the node, so before a prompt is queued
  its rollout is read (`queueHeld`, `rollout_turn.go`: the last of the
  `event_msg` records `task_started` / `task_complete` / `turn_aborted`). A
  start newer than the last hook event that said idle (`Session.IdleAt`), in
  a rollout written to within `rolloutTurnQuiet` (10 min), holds the wake; the
  messages of the turn go into one wake at its end, and their authors hear
  `held:busy_turn` meanwhile (once per message, `Session.HeldNoted`). The
  first turn end after an unproven prompt was queued holds the next wake for
  `inboxWakeGrace` from that end (the thread runs the prompt next); later ends
  hold nothing, so the hold is bounded by `wakeDue` plus one grace. No rollout
  or no such record: woken as before. `queued_stale` counts a running turn
  (rollout written to within `rolloutTurnQuiet`) as the owner's activity
  (`queuedTurns`), and a prompt that failed `queued_stale` is observed again
  while the thread is in a turn (`observeLateStarts`): its late start is
  proven and acknowledged.
- A `queue` prompt is a pointer (`pointer.go`, token `p-…`), no message: the
  queue cannot be inspected or cancelled, so a prompt consumed late must not
  present what was read meanwhile. Its marker in the rollout proves the
  pointer ran (`Lease.Shown`, wake count ended), never a read; the session's
  fetch delivers what is unread for it then. "Handed" (`handedTo`) for a queue
  = a pointer was queued for the message and is not known consumed, or its
  fetch grace runs: no message has two prompts in a queue at once, and one
  whose pointer ran unfetched may get a new one (its attempts and backoff as
  ever). A token without `p-` is a prompt of v0.6.75 or older, which carried
  the messages: its marker still starts and acknowledges them, and its
  messages stay `woken` for the hooks.
- One pointer at a time (owner decision, issue #40). While a pointer is in a
  session's queue and its rollout has not shown it (`pendingPointer`: a lease
  of the session by a `p-` token, not `shown`, that is `leased`, or `retry`
  `stopped`/`session_gone`, and no lease of that token `failed`
  `queued_stale`; `pointerPending` also reads the rollout for the marker when
  no observer ran), `wakeIdle` queues
  nothing more for it: the messages a wake is due for are leased under that
  pointer's token (`wakeClaim` with `pending`), counted as a wake of the idle
  period, reported `wake_requested`. Bounds are those of a queued wake: each
  lease fails `queued_stale` after `queuedOwnerMax`, `maxIdleWakes` +
  `maxStaleWakes` on `staleWakeWait`, then `noteWakeCap` (`needs_human`).
  The marker covers every lease of the token (`leaseBook.shown`), whenever it
  was taken. A pointer whose messages were all delivered some other way
  (acked) is not pending: a later message gets a new pointer, and the old one
  costs a trivial turn when the thread gets to it.
- A lost pointer blocks nothing (second review of issue #40; before, a pointer
  Codex never ran kept every later message of the thread leased to it, so no
  prompt was queued for that thread again). A pointer is pending only while
  the node may still expect it to run. It counts as lost:
  - once a lease of it `failed` `queued_stale` (the session silent for
    `queuedOwnerMax` with it queued; a turn its rollout shows running is no
    silence). Its messages stay `failed` (`needs_human`, no wake is for them
    again, as in v0.6.76), and `pendingPointer` passes over its token;
  - once the session's hooks reported a turn after it was queued
    (`Session.TurnAt`), that turn's end (`Session.IdleAt`), and
    `pointerLostWait` (2 min: the thread runs a queued prompt right after the
    turn it waited behind, and nothing is queued or leased for it meanwhile)
    later its rollout still has no marker of it (`pointerTurned`,
    `pointerPending`, `lostPointers` in the sweep). Its leases go back to
    `retry` with reason `pointer_lost` (a failure of the owner, their backoff,
    `failed` at `maxLeaseAttempts`), which `handedTo` does not count as handed.
    An idle event alone (the app opened again) loses nothing, and neither
    does the wait while the rollout shows a turn running that the hooks did
    not report (`turnRunning`): the pointer is late behind that turn;
  - once a lease of it that went back to `retry` with the pointer still in
    the queue (`stopped`: the emergency stop; `session_gone`: the registration
    lapsed) stayed so for `queuedOwnerMax` (`leaseBook.orphaned`,
    `orphanPointers`, first in the sweep). Nothing else ends such a lease
    (third review: with silent hooks the message stayed handed for ever, was
    never woken for again and never reached `needs_human`, and a later one
    was leased under the dead pointer until `queued_stale`). Its reason
    becomes `pointer_lost`, and what is leased under the token goes back to
    `retry` with it.

  The next wake that is due then queues a new pointer under a new token, on
  the schedule and caps of every wake (`wakeDue`, `staleWakeWait`,
  `maxIdleWakes` + `maxStaleWakes`, `noteWakeCap`): for the newer messages,
  for a `pointer_lost` one after its backoff, and its fetch delivers whatever
  is unread for the session then, a `queued_stale` message too. The invariant
  is: a message id is never named by two unconsumed pointers (a lost one does
  not count; if the thread runs it after all, its fetch takes what waits then
  or nothing, as any late pointer).
- The fetch (`UnreadListing` + `Deliver`, `pointerFetch`; over several
  contexts `controlUnread` → `deliverUnread`) lists and fills one answer
  (`FetchFill`: at most the page limit, within `FitUnread`/`FormatBudget`, as
  a hook's batch), claiming and acking only what it returns; `UnreadPage.Note`
  tells the agent that more wait. The fill runs in rounds inside the one call
  (issue #42, v0.6.79): the listed messages that fit beside the granted ones
  are claimed; one `Claim` did not grant (another owner took it after the
  listing) is left out, counts for nothing of the budget, and the next listed
  ones take its room, those of the listing's later pages too
  (`FetchListing`, the fetch's own cursor, never the caller's). Each listed id
  is offered once per call, so the rounds end with the listing or the full
  answer. An empty answer therefore means nothing listed is the session's to
  take now and has no `note`; `note` is set only on an answer that delivered
  something and left listed messages unoffered. Before, a cut to a message
  taken meanwhile answered empty with no note and the claimable ones behind
  it waited for `pointerFetchGrace` and a new wake. A fetch has no cursor (issue #41, v0.6.78):
  what it delivered is read and listed no more, so "call again" is the same
  call, which lists from the start; `after` is ignored for a fetch (in every
  context of it) and its answer never has `next` (the CLI's last line is then
  `{"note","total"}`). A plain listing keeps `after`/`next` as before. Before,
  `next` was the cursor of the last listed message, and a call that followed
  it passed over a message listed before it and not delivered (a failed
  context's). In a context the session is not registered
  in, `Claim` grants the replies to its own messages, so the fetch reads
  those too and they are not listed again. A context whose `Deliver` fails
  does not fail the call: an earlier context acknowledged its part already,
  so the others are delivered all the same, the answer is the delivered part
  (the failed context's messages stay unread; `note` is `FetchPartNote`: a
  part could not be delivered, call again) and the error is logged. The call
  made again lists them first and delivers them. Only when no context
  delivered anything is the call an error (500, `fetchFailed`, the failed
  context named): an empty page would say nothing waits. The MCP
  tool's `session` argument names the session only for a client that knows
  none or is a Codex thread without `_meta`: a Claude Code session always
  asks as itself, so its listing never becomes a queue session's delivery
  (`unreadSession`). Whose the MCP server is, its client says (`clientInfo`
  of the initialize: `claude-code`, `codex-mcp-client`; `callClient`,
  handed to a delegated call as `AGENTLINK_MCP_CLIENT`), and only for an
  unknown client the environment: a Codex server that inherited
  `CLAUDE_CODE_SESSION_ID` still honours the argument.
- Mixed install (v0.6.78): the plugin copy must be refreshed together with
  the node for a pointer to fetch on the first try. An `agentlink mcp` server
  of an older plugin copy that is still running (e.g. 0.6.69) advertises
  `unread` without `session` and with `additionalProperties: false`, so the
  call the pointer asks for (`session: "<id>"`) is refused before it reaches
  the node. The prompt therefore gives three steps: `unread` with `session`;
  if that is refused because of the argument, `unread` without arguments; if
  the tool is absent, or its answer shows messages without `delivered: true`,
  the CLI line. Without the argument a Codex call still delivers when the
  client names its thread (`_meta.threadId` or
  `x-codex-turn-metadata.thread_id`, `mcpCallSession`): an old server that
  hands the call to the updated executable (`delegateMCP`, since before
  0.6.69) passes that thread on as `CODEX_THREAD_ID`, so `_meta` is not lost.
  It does not pass `AGENTLINK_MCP_CLIENT` (newer), so with an inherited
  `CLAUDE_CODE_SESSION_ID` the handed-on call asks as that Claude session.
  With neither argument nor thread id the call is a plain listing: it shows
  the messages, reads nothing, and the pointer ends `pointer_unfetched`
  (`held:wake_unfetched`) unless the CLI line is run.
- The pointer's prompt names no number of messages: the fetch returns what
  is unread for the session when it runs (newly claimed, stale ones, what
  came later), not what the wake was queued for.
- Accepted, not changed (review of issue #40):
  - The ack precedes the transport: the fetch marks read before the HTTP
    answer, the MCP result or the CLI's stdout reached the agent, so an
    answer lost on the way (process killed, tool result dropped) is read
    and unseen. Same risk class as a hook's ack before its injected text is
    shown, which is accepted; the message stays in `history`, and its
    author's request stays visible as unanswered.
  - `session` (query parameter of `GET /unread`, argument of the MCP tool,
    `--session` of the CLI) is not authenticated: any local process that
    reaches the loopback API can fetch as a session, as it can `POST /ack`
    as one. The API is local-only by design; no new exposure.
- `failed` is terminal for automatic paths (`Lease.Failed`): a hook may still
  show the message to a live session, and its lapse returns to `failed`.
- `hook` leases (a session in a turn claiming) do not count as attempts and
  their lapse does not count as an owner failure: the batching hook just
  delivers the rest at its next event.
- Backoff (retry `next_at`): 15 s doubling to 5 min, node wakes only
  (`inbox`, `queue`). Waiters keep their `maxWaiterWakes` cap (now the
  record's `waiter_wakes`, persisted); launches keep `launchDebounce`.
- `failed` blocks every automatic delivery. The message stays unread: a
  session's hooks still deliver it at its next event (a person decides).
- A revoke for a deadline and a `queued_stale` failure keep a lapsed wake claim's token (PR #16: a late
  proof acks instead of delivering twice); a revoke for a gone session drops
  the claim.

## Routing (preference, not ownership)

- An owner with `fails >= maxOwnerFails` (2) for a message is passed over:
  chat affinity naming it is ignored (`routeOf`), a node wake picks another
  idle session.
- Node wake pick per message: sessions in a turn first (their hooks deliver,
  no wake); else the idle wakeable session with fewest failed leases of that
  message, then most recently active (`LastActive`, not registration).
- Orphan: every live session of the area is passed over for the message
  (too many failed leases, or idle, not wakeable and no hook event for
  `AffinityLapse`). The launch ladder then opens a native new session for it
  although sessions are registered (`launch.go`), confirmed only by a live
  session that was not live at the launch.
- Caps kept: `maxIdleWakes` per idle period, `maxWaiterWakes` per message,
  `launchTries`, seat `seatRetry`.

## Persistence and restart

`leases.json` is written on every automatic transition and at the next sweep
after hook-only changes. On start: session wake leases restore their claims
(token kept, deadline unchanged); a `leased` launch/seat lease is revoked
(`restart`); a `running` launch lease is `failed` (its turn died with the
node, it may have acted). Records of messages no longer unread go after
10 min; any record after 7 days.

## API

Go (for PR-D Codex liveness; stable, small):

```go
func (n *Node) LeaseStart(owner, token string, ids []string) []string // leased -> running; token "" matches any
func (n *Node) LeaseRevoke(owner string, ids []string, reason string) // leased/running -> retry|failed
func (n *Node) Leases() []LeaseView                                   // every unread message, pending included
```

HTTP: `GET /leases` -> `[]LeaseView`
`{id, chat_id, seat, state, owner, via, attempts, deadline, next_at, reason}`
for every unread message (UI PR-E).

## Open risks

- A Codex session that died without its hooks ending it keeps a queued lease
  until its registration lapses (TTL 1 h), PR-D reports the thread gone or it
  stays silent for `queuedOwnerMax`.
- Proof for Claude is still the hook's ack at the woken prompt; a session
  whose hooks never run cannot prove and is passed over after two failed leases.
