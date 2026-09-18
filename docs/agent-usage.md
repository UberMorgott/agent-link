# Using agent-link from a coding agent

How an agent on one machine asks the agent on the other machine a question and gets the answer.
Everything goes through the `agentlink` CLI against the node that is already running on this
machine (the tray app or `agentlink serve`).

## The config path

Every subcommand needs `--config <path>` — a **node config** (`node`, `listen`, `api`,
`data_dir`, `secret_env`, optional `areas`/`peers`; see README "Config"). `send`, `inbox` and
`wait` only talk to the local control API, so the one field that must be right is `api`: the
loopback address of the running node (the tray app's default is `127.0.0.1:7520`, from
`%APPDATA%\agentlink\config.json`, key `api`, absent means the default). The remaining fields
must be valid but are unused by these three commands.

If there is no node config on the machine, write one next to the settings, e.g.
`%APPDATA%\agentlink\cli.json`, using `examples/node-a.json` as the template:

```json
{
  "node": "<this node's name>",
  "listen": "127.0.0.1:7420",
  "api": "127.0.0.1:7520",
  "data_dir": ".",
  "secret_env": "AGENTLINK_SECRET",
  "peers": [{ "addr": "127.0.0.1:7421" }]
}
```

No code or secret is needed for `send`/`inbox`/`wait`: the running node holds it.

## Ask and get the answer

```powershell
agentlink send  --config <path> --to <node> --body "<question>"   # prints the message id; without --to: the only peer
agentlink inbox --config <path> --limit 20                        # one JSON object per line
agentlink wait  --config <path> --timeout 0                       # blocks until a message arrives
```

1. `send` prints the request id. Keep it: the answer refers to it as `reply_to`.
2. Start `wait` as a **background command** (`run_in_background` in Claude Code). It blocks,
   then exits 0 and prints one JSON line per message; the harness announces the completion and
   the session reads the answer from that output. Exit 2 with no output means `--timeout`
   expired (seconds or a Go duration, `0` waits forever); exit 1 is an error. `wait` marks what
   it returns as delivered, so start it again after handling a batch.
3. `inbox` is non-destructive and shows progress while the other side is still working: an
   outbound entry carries the latest `job_status` and, once answered, the `answer` text.
4. To answer a request that arrived here, send with `--reply-to <that message's id>`.

## Status message or real answer

- `wait` returns requests and replies only. Handler progress (`kind: "status"`,
  `job_status: queued|running`, empty body) never wakes it.
- A real reply has `reply_to` set to the request id and a body. If it came from a handler agent
  its `job_status` is `completed` (the body is the answer) or `failed` (the body is the error).
- In `inbox`, an outbound request with `job_status` `queued`/`running` and no `answer` is still
  being worked on.

## What the answering side does

If the other machine has a handler agent configured, the request runs there **read-only**:
`claude` limited to Read/Grep/Glob, or `codex` with `--sandbox read-only`, with the prompt on
stdin and never through a shell. It can read and search that machine's files but cannot edit
them or run commands, and its answer is sent back automatically. With the handler set to
"none" a human answers from the inbox page, so the reply may take a while.

Never put secrets, tokens or config contents in a message: the body travels in clear text
inside the VPN tunnel and lands in the other developer's inbox.
