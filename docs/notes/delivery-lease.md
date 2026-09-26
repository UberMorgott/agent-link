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

Rules:

- `running` only on proof. Queue/RPC/inbox accept is `leased`, never more.
- A wake that put the message into a session (`inbox` post accepted, `codex
  queue`, waiter output) cannot be withdrawn: the lease stays that session's
  (`routeOf` routes to it) until proof, or evidence the channel dropped it
  (session end/gone, post/queue/proxy error, thread gone: `LeaseRevoke`).
  Time alone never reassigns it; waking the same session again is no failure.
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
- A revoke for a deadline keeps a lapsed wake claim's token (PR #16: a late
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
  until its registration lapses (TTL 1 h) or PR-D reports the thread gone.
- Proof for Claude is still the hook's ack at the woken prompt; a session
  whose hooks never run cannot prove and is passed over after two failed leases.
