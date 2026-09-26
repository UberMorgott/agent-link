# Codex shared app-server delivery

Status: implementation contract for durable AgentLink delivery leases.

## Facts verified on Codex 0.155.0-alpha.16.4

- Codex Desktop 26.917.9434.0 starts a private `codex app-server` over stdio.
- A second app-server can `thread/read` that Desktop thread, but reports
  `status: notLoaded` even while the Desktop turn is active.
- `codex queue --thread ...` proves that a prompt was stored. It does not prove
  that a live client owns the thread or started a turn.
- The CLI supplies the native shared path: `codex app-server daemon` owns one
  app-server and `codex app-server proxy` connects stdio clients to it.

Therefore rollout modification time is only a fallback activity hint. A
separate app-server's `notLoaded` result must not mark a Desktop thread dead,
and queue command success must not acknowledge a delivery.

## Lease contract

For Codex, map delivery state only from these proofs:

| Lease transition | Required proof |
| --- | --- |
| `pending -> leased` | a selected live hook session, or a shared app-server thread whose status is `idle` or `active` |
| `leased -> running` | matching app-server `turn/started`, or the target session's hook claims the wake token |
| `running -> acked` | the hook acknowledges the message IDs |
| `leased -> retry` | queue/proxy failure, thread `notLoaded` on the shared server, lease deadline, or session removal |
| `running -> retry` | turn ends without an AgentLink acknowledgement before the running deadline |

The shared app-server connection is authoritative only for threads loaded by
that same daemon. `thread/status/changed`, `turn/started`, and
`turn/completed` update the lease owner; rollout freshness may extend a short
probe window but never acknowledges or assigns ownership.

## Runtime path

1. The non-elevated AgentLink app starts the native managed daemon once.
2. Direct Codex launches and liveness monitoring connect through
   `codex app-server proxy`; no custom WebSocket transport is needed.
3. AgentLink subscribes to the proxy stream before queueing, reads the target
   with `thread/read`, and retains the connection through lease completion.
4. If daemon/proxy is unavailable, keep the existing standalone launch and
   hook/rollout fallback. Do not infer Desktop liveness from a separate
   app-server.
5. Codex Desktop must use the same daemon before its thread status can be
   authoritative to AgentLink. Until Desktop exposes that connection path,
   Desktop-owned threads remain hook-authoritative.

`turn/start` returning successfully is not enough to enter `running`.
`internal/node.CodexTurn` enforces this now by invoking its `started` callback
only for the matching `turn/started` notification.
