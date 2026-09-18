# agent-link — agent contract

Instructions for an AI coding agent installing, running or changing this repository.
`README.md` is the usage reference (CLI commands, config fields, delivery and security details);
this file is the contract and the install path. Do not duplicate README content here.

## What it is

Two machines, one node each. A node keeps a TCP session to its peer over ZeroTier and carries
messages between the two developers' agent sessions. `agentlink-tray.exe` is the same node plus a
tray icon, a browser settings page, an inbox page and an optional read-only handler agent.

## Prerequisites

- Windows. The tray app, autostart and the `scripts/*.ps1` checks are Windows-only; pwsh 7+.
- ZeroTier (or another VPN) joined to the same network on both machines, and each side's own
  ZeroTier IP known.
- Go 1.27+ — only to build from source or run the tests. A release binary needs no Go.
- Claude Code or Codex, logged in — only if this side answers requests automatically. With
  the handler set to «Никто, отвечаю сам» neither is needed. Any install works and need not be
  on the tray's `PATH`: Codex desktop app (its `%LOCALAPPDATA%\OpenAI\Codex\bin\<hash>\codex.exe`
  shares the app's login), Codex installer / npm / winget / `CODEX_CLI_PATH`, Claude Code native
  installer / npm / winget, Claude desktop app (`%APPDATA%\Claude\claude-code\<ver>\claude.exe`),
  or a VS Code / Cursor / Windsurf extension's bundled binary. The location table and its sources
  are in README "Handler agent" and `internal/settings/agent.go` (`locations`); every hit is
  checked with `--version`. **Программа агента → Найти заново / Указать…** re-runs the search
  or picks any other program (stored as `agent_path` in the config).

## Install

Release asset (preferred): download `agentlink-tray.exe` (and `agentlink.exe` for the CLI) from
the GitHub release, put them in a folder of your choice, run the tray exe. Nothing else is
installed; all state lives under `%APPDATA%\agentlink`.

From source:

```powershell
go build -o bin/agentlink.exe ./cmd/agentlink
go build -ldflags "-H=windowsgui" -o bin/agentlink-tray.exe ./cmd/agentlink-tray
```

`go build ./cmd/...` builds both, but without `-H=windowsgui` the tray app keeps a console window.

## First run

Start `agentlink-tray.exe`; it opens the settings page (later: tray icon → "Open settings").
Five fields: name (prefilled), 6-character code (**Создать код** on one side, typed on the
other), the peer's ZeroTier IP (port optional), who answers, working folder; Save. Everything
else is under the collapsed "Дополнительно". README's "First run (both people)" has the walk.

Settings live in `%APPDATA%\agentlink\config.json`, messages in `%APPDATA%\agentlink\data`, the
log in `%APPDATA%\agentlink\agentlink.log`. The tray app writes the code into that config file.
That file is never committed and never copied into a repository, an issue, a log or a chat.

## Pairing the two sides

Both sides must agree, or the session never authenticates:

- Each side listens on **its own** ZeroTier IP (the tray app finds it; default port 7420) and at
  least one side has the other's IP as the peer address. The settings page shows this side's
  address to pass on.
- The **same** 6-character code on both machines (case-insensitive), exchanged out of band (a
  private channel, not this repo, not a PR, not an issue). The CLI reads it (or a legacy 16+
  byte secret) from the env var named by `secret_env`; the tray app stores it in its config.
- Names are learned from the handshake; a configured peer name, if any, must match.
- The code is short and brute-forceable offline, so the private ZeroTier network is the security
  boundary (README "Security").
- `areas` must overlap for `--to area:NAME` fan-out to reach the peer.
- Both sides run v0.2 or later (v0.2 changed the handshake MAC). From there on versions mix:
  since v0.4 `hello` announces `proto` and `caps`; unknown frames and fields are skipped, never
  an error that drops the session. Rule for changes: the wire format only grows. Never make a
  new field required, never reject an unknown one, and gate every new feature on a new cap
  (`node.Capabilities`, `Node.PeerHas`); a peer without caps is an older version.

## Waking an interactive session

An interactive Claude Code (or Codex) session does not poll. It starts

```text
agentlink wait --config <path> --timeout 0     (as a background command)
```

The command blocks until a message arrives, then exits and prints one JSON line per message; the
harness announces the completion and the session reads them from that output, replies with
`send --reply-to <id>`, and starts `wait` again. Exit codes and what `wait` does and does not
return are in README ("Usage").

## Handler policy — read-only, and it answers a stranger

The handler agent runs with read-only tool permissions (`claude` limited to Read/Grep/Glob,
`codex` with `--sandbox read-only`), the prompt over stdin, never through a shell. It cannot edit
files or run commands. It can still read files outside the working folder, and its answer is sent
to the other person. Consequences, which are rules:

- Only pair with someone you trust with read access to this machine's files.
- Never widen the handler's tool set, add a write mode, or route the prompt through a shell.
  Streaming output (`--output-format stream-json --verbose`, `codex exec --json`) is only parsed
  for activity lines and the answer; it does not change what the agent may do.
- Every run is bounded: `max_jobs` (1–4, default 2) agents at once, a 10-minute hard timeout and
  a 3-minute idle timeout (no stdout line), both kill the process tree.
- Never make the handler echo secrets, tokens or config contents into a reply.

## Validation

Run from the repository root; all must pass before a commit.

```powershell
go test -race ./...
pwsh -File scripts/e2e-local.ps1     # two CLI nodes on loopback: send, reply, stop
pwsh -File scripts/e2e-worker.ps1    # two headless tray apps + fake agent; -RealClaude / -RealCodex [-AgentPath]
pwsh -File scripts/e2e-parallel.ps1  # max_jobs 2, streamed activity at the sender, idle-timeout kill
pwsh -File scripts/e2e-tray.ps1      # both people on one machine through the web UI; restarts, retries
qgate                                # quality gate; qgate -All when deps or build config changed
```

`qgate` exit 0 means done. Anything else names the failure — fix it; never bypass or edit the gate.

## Hard rules

- Never commit a secret, a token, or a real `config.json` / node config with a secret in it.
  `examples/*.json` are templates and carry no secret.
- `listen` binds to the ZeroTier IP, never `0.0.0.0`, and the port is firewalled to the peer.
- The control API stays loopback-only; a non-loopback `api` value is refused on purpose — do not
  relax that check.
- There is no TLS. Confidentiality comes from ZeroTier alone; do not describe the link as
  encrypted end to end by agent-link.
- Do not commit binaries (`bin/`, `*.exe`) or node data (`.data/`); release assets are built and
  attached, not tracked.
- Release binaries are always stripped and UPX-packed: build them only with
  `pwsh -File scripts/release.ps1 -Version <x.y.z> [-Publish]` (`-trimpath -ldflags "-s -w"`,
  `upx --best --lzma`, `upx -t`), never attach a plain `go build` output.
- `reference/` is an untracked third-party checkout (see README). Read it, never edit it, never
  add it back to git.
- Conventional commits with explicit paths.
