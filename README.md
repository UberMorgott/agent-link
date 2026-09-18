# agent-link

Direct agent-to-agent messaging between machines over ZeroTier or a plain IP, written in Go.
Each developer runs one `agentlink serve` node; their Claude Code or Codex session sends with
`agentlink send` and gets woken by `agentlink wait`.

`reference/relay/` is AgentWorkforce/relay at tag v12.2.2 (commit f0c5dc1), Apache-2.0, kept for
reading only. It is not part of this repository; fetch it yourself if you want it:

```powershell
git clone --depth 1 --branch v12.2.2 https://github.com/AgentWorkforce/relay.git reference/relay
```

## Desktop app (recommended)

`agentlink-tray.exe` is the whole thing in one program: a tray icon that runs the node, a settings
page and an inbox page in your browser, and an optional agent that answers requests for you.

### Install

```powershell
go build -ldflags "-H=windowsgui" -o bin/agentlink-tray.exe ./cmd/agentlink-tray
```

Copy `agentlink-tray.exe` anywhere and double-click it. Both people need ZeroTier (or another
VPN) joined to the same network.

### First run (both people)

1. Start `agentlink-tray.exe`; the settings page opens. Later use the tray icon, "Open settings".
2. **Ваше имя** is already filled with your Windows name; change it if you like.
3. **Код связи**: one person clicks **Создать код** and tells the other the 6 letters/digits;
   the other types them in (case does not matter). The code must be the same on both sides.
4. **Адрес собеседника**: the other person's ZeroTier IP, e.g. `10.147.20.9` (no port needed).
   The page shows your own address under this field ("Ваш адрес для собеседника") — tell it
   to the other person. One side having the other's address is enough to connect.
5. **Кто отвечает** and **Рабочая папка** (see "Handler agent"): press **Выбрать…** next to the
   folder field and pick the project folder in the Windows folder dialog (the tray app opens it,
   since a browser page cannot see absolute paths; the field stays editable), then **Сохранить**. The top
   bar shows "связь есть: <name>" once both sides are saved; the other person's name comes
   from the connection.

Saving with only some fields filled is fine: the bar then says what is missing ("нет кода
связи", "нет собеседника — впишите его адрес"). **Дополнительно** (collapsed) holds the rest:
your own listen address (default: this machine's ZeroTier IP with port 7420; without ZeroTier
it binds `127.0.0.1` and says "ZeroTier не найден", never `0.0.0.0`), the page address
(default `127.0.0.1:7520`, applies after a restart), shared areas, the peer's name (when set,
another name is refused) and autostart. Every error on the page is one sentence saying what to do.

Configs written by v0.1 (long `secret`, `peer_name`, `listen`) still load and work; typing a
code and saving replaces the secret. Both sides must run v0.2 or later: the handshake changed.

The two web pages are in Russian; every visible string lives in `internal/app/strings.go`, so a
second language means a second map, not a page rewrite.

Settings, including the code, live in `%APPDATA%\agentlink\config.json` (per user, never in a
repo); messages in `%APPDATA%\agentlink\data`, the log in `%APPDATA%\agentlink\agentlink.log`.
Autostart is the `agentlink` value under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`.

### Handler agent

When a request arrives (a message that is not a reply) and the handler is not "None", the app
runs the agent in the working folder with the message as the prompt, one request at a time,
and sends the agent's final answer back as a reply. With "Никто, отвечаю сам" you read and
answer in the inbox page, where each question is shown with its answer, its direction
(«Исходящее»/«Входящее») and both node names.

Jobs are durable. Each request is recorded in `data\jobs\<id>.json` before it is acknowledged
and moves `queued` → `running` → `completed` | `failed` (with attempts, timestamps and the
error). The sender gets small status updates (`queued`, `running`) and a final reply whose
`job_status` is `completed` or `failed`; its inbox page shows each request's latest status and
answer. A duplicate request id never runs twice. Queued jobs survive quit, crash and settings
changes and resume in order. A job cut off while running (quit, crash, settings save) runs once
more on the next start; cut off again, it fails with a reply. An agent error or the 10-minute
timeout fails the job at once, without a retry. Switching the handler to "None" fails jobs that
were still waiting, with the reply "no handler configured".

- Claude Code: `claude -p --output-format text --tools Read,Grep,Glob --allowedTools Read,Grep,Glob --permission-mode dontAsk --permission-prompts none --strict-mcp-config --no-session-persistence`
- Codex: `codex exec --sandbox read-only --skip-git-repo-check --ephemeral --color never --output-last-message <tmp> -`

The prompt goes through stdin, never through a shell. **Read-only:** the agent can read and
search but cannot edit files or run commands. It can still read files it can reach (Codex's
read-only sandbox is not limited to the working folder), and its answer goes to the other
person, so only pair with someone you trust with that folder.

`docs/agent-usage.md` is the short version for a coding agent that wants to use the link itself:
how to find the config path, ask the other machine a question and read the answer.

## CLI

### Build

```powershell
go build -o bin/agentlink.exe ./cmd/agentlink
```

## Usage

```powershell
$env:AGENTLINK_SECRET = 'K7Q2MX'   # the same 6-character code on both machines (or a 16+ byte secret)
agentlink serve --config node.json                         # the node (keep running)
agentlink send  --config node.json --to node-b --body "hi" # prints the message id
agentlink send  --config node.json --body "hi"             # no --to: the only known peer
agentlink send  --config node.json --to area:dev --body "build is green"
agentlink send  --config node.json --to node-b --body "done" --reply-to <id>
agentlink wait  --config node.json --timeout 0             # blocks; JSON line per message
agentlink inbox --config node.json --limit 20              # recent in/out, non-destructive
```

`wait` exits 0 after printing every undelivered inbound message (they are then marked
delivered), exits 2 with no output when `--timeout` (seconds or a Go duration, `0` = forever)
expires, and exits 1 on errors. It returns requests and replies only: a handler's `queued` /
`running` status updates never wake it, so a waiting session sees just the final reply (check
its `job_status`: `completed` or `failed`). Progress is visible in `inbox`.

## Config

```json
{
  "node": "alice",
  "listen": "10.147.20.5:7420",
  "api": "127.0.0.1:7520",
  "data_dir": "../.data/alice",
  "secret_env": "AGENTLINK_SECRET",
  "areas": ["dev"],
  "peers": [{ "addr": "10.147.20.9:7420" }]
}
```

- `listen` — peer TCP listener; bind it to the ZeroTier address.
- `api` — local HTTP control API; must be a loopback address, anything else is refused.
- `data_dir` — outbox, inbox and sent messages as JSON files; relative to the config file.
- `secret_env` — name of the environment variable holding the 6-character pairing code (letters
  and digits, case-insensitive) or a legacy shared secret of 16+ bytes.
- `areas` — topics this node subscribes to; `--to area:NAME` fans out to every peer that announced it.
- `peers` — addresses to dial; `name` is optional. A peer without a name is learned from the
  handshake. If every peer has a name, only those names may connect; with a nameless peer (or
  none) any node holding the code may. Nodes keep one session per peer.

Loopback examples: `examples/node-a.json`, `examples/node-b.json`. `scripts/e2e-local.ps1`
builds the binary, starts both, sends a→b, replies b→a and stops them. `scripts/e2e-worker.ps1`
runs two tray apps headless (`-no-tray`) with a fake agent and checks the automatic reply;
`-RealClaude` / `-RealCodex` use the installed `claude` / `codex` instead.
`scripts/e2e-tray.ps1` plays both people on one machine: two tray apps with their own settings
folders (`-config`) and API ports (`-api`), set up, messaged and quit through the web UI
endpoints; it also quits and restarts b in the middle of three slow jobs (all complete, job 1
runs twice, the others once), checks that a save during a job retries it once and a second save
fails it, that no agent processes are left, and that a restart keeps the inbox. `-Address <ip>` binds the peers to e.g. the ZeroTier IP.
With a non-default `-config` the app never touches the autostart entry, and `POST /ui/api/quit`
(token-guarded, the same path as the tray's Quit) exits it.

`scripts/demo-local.ps1` is the same two-people setup but as a live demo, not a test: it starts
`morgott` and `nikita` with their settings under `$env:TEMP\agentlink-demo`, API ports 7530/7531,
peers on loopback, area `demo`, both answering with real Claude Code read-only over `-WorkDir`
(default `E:\DEV\CodeDungeon`). It proves the round trip with one real question, prints both
`/ui/inbox` URLs and leaves the apps running with their tray icons. Stop them from a tray icon's
Quit or with `scripts/demo-local.ps1 -Stop`.

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
- Inbound messages are persisted, and requests recorded as handler jobs, before the ACK; both
  are deduplicated by id, so resends are idempotent. Status updates and handler replies use ids
  derived from the request, so re-sending them after a crash is deduplicated too.
- Status updates are `kind: "status"` messages; a node without that field treats them as empty
  replies, so update both sides together.
- `wait` marks messages delivered as it returns them; a `wait` killed mid-response can lose that
  batch from `wait` (it stays visible in `inbox`).

## Security

- Bind `listen` to the ZeroTier (or VPN) IP, not `0.0.0.0`, and firewall the port to the peers.
- Peers authenticate with mutual HMAC-SHA256 challenge-response over fresh nonces, keyed with
  HKDF-SHA256 of the upper-cased pairing code (context `agentlink/pair-code/v1`); the code is
  never sent. Node names are announced in the handshake and bound by the MACs; bad MACs are
  rejected.
- **The security boundary is the private ZeroTier network, not the code.** A 6-character code
  has about 31 bits: anyone who can reach the port can record one handshake and brute-force the
  code offline in minutes. Keep the ZeroTier network private (only the two of you as members)
  and firewall the port to it. A legacy 16+ byte random secret (CLI `secret_env`, or a v0.1
  tray config) resists that attack if you need it.
- The CLI's code or secret lives only in an environment variable; CLI configs are safe to commit.
- There is no TLS: message bodies travel in clear text, relying on ZeroTier's encryption.
- The control API listens on loopback only and rejects browser requests (`Origin` header) and
  non-loopback `Host` headers, but any local process on the machine can use it.
- The tray app's web pages share that address. Their API calls need a random per-run token that
  is embedded in the page and must come from the same origin, so other websites cannot read or
  change settings or send messages; the pages refuse to be framed.
