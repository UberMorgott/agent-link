# agent-link

Direct agent-to-agent messaging between machines over ZeroTier or a plain IP, written in Go.
Each developer runs one `agentlink serve` node; their Claude Code or Codex session sends with
`agentlink send` and gets woken by `agentlink wait`.

`reference/relay/` is AgentWorkforce/relay at tag v12.2.2 (commit f0c5dc1), Apache-2.0, kept for reading only.

## Build

```powershell
go build -o bin/agentlink.exe ./cmd/agentlink
```

## Usage

```powershell
$env:AGENTLINK_SECRET = '<same random 32+ byte secret on both machines>'
agentlink serve --config node.json                         # the node (keep running)
agentlink send  --config node.json --to node-b --body "hi" # prints the message id
agentlink send  --config node.json --to area:dev --body "build is green"
agentlink send  --config node.json --to node-b --body "done" --reply-to <id>
agentlink wait  --config node.json --timeout 0             # blocks; JSON line per message
agentlink inbox --config node.json --limit 20              # recent in/out, non-destructive
```

`wait` exits 0 after printing every undelivered inbound message (they are then marked
delivered), exits 2 with no output when `--timeout` (seconds or a Go duration, `0` = forever)
expires, and exits 1 on errors.

## Config

```json
{
  "node": "alice",
  "listen": "10.147.20.5:7420",
  "api": "127.0.0.1:7520",
  "data_dir": "../.data/alice",
  "secret_env": "AGENTLINK_SECRET",
  "areas": ["dev"],
  "peers": [{ "name": "bob", "addr": "10.147.20.9:7420" }]
}
```

- `listen` — peer TCP listener; bind it to the ZeroTier address.
- `api` — local HTTP control API; must be a loopback address, anything else is refused.
- `data_dir` — outbox, inbox and sent messages as JSON files; relative to the config file.
- `secret_env` — name of the environment variable holding the shared secret.
- `areas` — topics this node subscribes to; `--to area:NAME` fans out to every peer that announced it.
- `peers` — the only node names allowed to connect. Nodes dial each other and keep one session per peer.

Loopback examples: `examples/node-a.json`, `examples/node-b.json`. `scripts/e2e-local.ps1`
builds the binary, starts both, sends a→b, replies b→a and stops them.

## Waking a Claude Code session

Run `wait` as a background shell command. When a message arrives the command exits, the
harness reports the completion, and the agent reads the JSON lines from its output:

```text
agentlink wait --config C:\agentlink\node.json --timeout 0   (run_in_background)
```

After handling the messages (and replying with `send --reply-to`), start `wait` again.

## Delivery

- Sent messages are written to `outbox/<peer>/` first and removed only when the peer ACKs, so an
  offline peer gets them on the next connection; unACKed messages are resent periodically.
- Inbound messages are persisted before the ACK and deduplicated by id, so resends are idempotent.
- `wait` marks messages delivered as it returns them; a `wait` killed mid-response can lose that
  batch from `wait` (it stays visible in `inbox`).

## Security

- Bind `listen` to the ZeroTier (or VPN) IP, not `0.0.0.0`, and firewall the port to the peers.
- Peers authenticate with mutual HMAC-SHA256 challenge-response over fresh nonces; the secret is
  never sent. Unknown node names and bad MACs are rejected. Use a long random secret: an
  attacker who can reach the port can collect MACs for an offline guessing attack.
- The secret lives only in an environment variable; configs are safe to commit.
- There is no TLS: message bodies travel in clear text, relying on ZeroTier's encryption.
- The control API listens on loopback only and rejects browser requests (`Origin` header) and
  non-loopback `Host` headers, but any local process on the machine can use it.
