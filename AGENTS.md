<!-- quality-gate -->
## Completion gate (mandatory)

After changing files, run `qgate` from the repository root. Use `qgate -All` when
dependencies, build configuration, generated files or several stacks changed.

Exit code 0 means done. Anything else means NOT done: the output names the exact
failures -- fix them and run it again. Do not report completion while the gate is
failing, and never edit or disable the gate to make it pass. Include the command
you ran and its pass/fail result in your final response.

If `qgate` is unavailable, report that as a blocker, do not skip it. It installs with
`irm https://raw.githubusercontent.com/UberMorgott/quality-gate/main/bootstrap.ps1 | iex`

If the gate itself is wrong -- it crashes, blames code that is provably correct,
misses a whole stack, or cannot be satisfied at all -- do not work around it and do
not disable it. Open an issue against the gate and say so in your final response:

```powershell
qgate where   # install path + commit, paste this into the issue
gh issue create --repo UberMorgott/quality-gate --title "<what broke>" --body "<qgate output, the command you ran, the file it blamed, `qgate where` output>"
```
<!-- /quality-gate -->
# agent-link — agent contract

Instructions for an AI coding agent installing, running or changing this repository.
`README.md` is the usage reference (CLI commands, config fields, delivery and security details);
this file is the contract and the install path. Do not duplicate README content here.

## What it is

A network of N machines, one node each, all holding the same code. Every node keeps a TCP
session to every other member (over ZeroTier, the LAN or an external address), gossips the
member table so all members see each other, finds members on local networks by UDP beacon,
and carries messages between the developers' agent sessions (README "Members and discovery"). `agentlink-tray.exe` is the same node plus a
tray icon, a browser settings page, an inbox page and an optional read-only handler agent.

## Prerequisites

- Windows. The tray app, autostart and the `scripts/*.ps1` checks are Windows-only; pwsh 7+.
- A path between members: the same LAN, ZeroTier (or another VPN) joined to one network, or an
  address reachable from the internet (TCP 7420).
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
installed; all state lives under `%APPDATA%\agentlink`. From then on the app updates both files
in that folder from GitHub releases by itself (README "Updates"; `agentlink update` for the CLI).

From source:

```powershell
go build -o bin/agentlink.exe ./cmd/agentlink
go build -ldflags "-H=windowsgui" -o bin/agentlink-tray.exe ./cmd/agentlink-tray
```

`go build ./cmd/...` builds both, but without `-H=windowsgui` the tray app keeps a console window.

## First run

Start `agentlink-tray.exe`; it opens the settings page (later: tray icon → «Открыть настройки»).
Fields: name (prefilled), code `XXXX-XXXX-XXXX` (**Создать код** on one member, typed on the
others), **Участники сети** (the member list, **Добавить участника по адресу** + **Добавить**,
**Удалить**), who answers, working folder; Save. Everything else is under the collapsed
"Дополнительно". README's "First run (every member)" has the walk.

Settings live in `%APPDATA%\agentlink\config.json`, messages in `%APPDATA%\agentlink\data`, the
log in `%APPDATA%\agentlink\agentlink.log`. The tray app writes the code into that config file.
That file is never committed and never copied into a repository, an issue, a log or a chat.

## Pairing the two sides

All members must agree, or the session never authenticates:

- The tray app listens on every interface (port 7420) unless «Мой адрес» names one IP. A member
  is reached by LAN discovery, or by one member adding its address; the table spreads it to
  all. The settings page shows this side's address to pass on.
- The **same** code on every machine (`XXXX-XXXX-XXXX`, 60 bits; case, dashes and spaces do not
  matter; a legacy 6-character code still works but is weak), exchanged out of band (a private
  channel, not this repo, not a PR, not an issue). The CLI reads it (or a legacy 16+ byte
  secret) from the env var named by `secret_env`; the tray app stores it in its config.
- Names are learned from the handshake; a configured peer name, if any, must match. Names are
  unique per network: on a clash the smaller node id keeps the name.
- The handshake is a PAKE (CPace): no transcript allows offline guessing, and failed attempts
  back off per source. After it every frame is sealed (ChaCha20-Poly1305 records,
  internal/node/wire.go). What stays exposed (a legacy peer's MAC and plain session, the beacon
  tag, the hellos) is in README "Security".
- `areas` must overlap for `--to area:NAME` fan-out to reach the peer.
- Both sides run v0.2 or later (v0.2 changed the handshake MAC). From there on versions mix:
  since v0.4 `hello` announces `proto` and `caps`; unknown frames and fields are skipped, never
  an error that drops the session. Rule for changes: the wire format only grows. Never make a
  new field required, never reject an unknown one, and gate every new feature on a new cap
  (`node.Capabilities`, `Node.PeerHas`); a peer without caps is an older version. One
  exception, for security: a peer without `pake` authenticates only from a private address.

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
- Agents run detached and outlive the app (quit, restart, update, settings save); the next start
  reattaches, or resumes the agent's session if it died mid-run. Sessions are therefore
  persisted by the agent CLI (no `--no-session-persistence` / `--ephemeral`). A resume must keep
  exactly the launch's read-only limits (`Command.ResumeArgs`).
- Never make the handler echo secrets, tokens or config contents into a reply.

## Validation

Run from the repository root; all must pass before a commit.

```powershell
go test -race ./...
pwsh -File scripts/e2e-local.ps1     # two CLI nodes on loopback: send, reply, stop
pwsh -File scripts/e2e-worker.ps1    # two headless tray apps + fake agent; -RealClaude / -RealCodex [-AgentPath]
pwsh -File scripts/e2e-parallel.ps1  # max_jobs 2, streamed activity at the sender, idle-timeout kill
pwsh -File scripts/e2e-tray.ps1      # both people on one machine through the web UI; agent survives restarts, retries
pwsh -File scripts/e2e-update.ps1    # self-update 0.0.1 -> 0.0.2 from a fake releases API while an agent job runs
qgate                                # quality gate; qgate -All when deps or build config changed
```

`qgate` exit 0 means done. Anything else names the failure — fix it; never bypass or edit the gate.

## Hard rules

- Never commit a secret, a token, or a real `config.json` / node config with a secret in it.
  `examples/*.json` are templates and carry no secret.
- The peer listener binds every interface only because members must be reachable over the LAN
  and external addresses; the handshake is the gate there. Never add an unauthenticated peer
  path, never put the code, key or a reversible form of it in a beacon (only the Argon2id `net`
  tag), never answer an unauthenticated hello with a value keyed by the code alone (the legacy
  MAC goes only to private addresses, `Node.legacyAllowed`), never weaken the per-source
  backoff, and keep «Мой адрес» able to restrict the listener to one IP.
- The control API stays loopback-only; a non-loopback `api` value is refused on purpose — do not
  relax that check.
- A PAKE session is always sealed (records keyed from the CPace ISK and the hello transcript);
  never add a plain or downgradable path for a peer that announced pake, never reuse a nonce,
  and close the connection on any record that fails to open. A legacy session is plain: do not
  describe every link as encrypted by agent-link.
- Do not commit binaries (`bin/`, `*.exe`) or node data (`.data/`); release assets are built and
  attached, not tracked.
- Release binaries are always stripped and UPX-packed and carry their version: build them only
  with `scripts/release.ps1` (`-trimpath -ldflags "-s -w -X …/selfupdate.Version=<x.y.z>"`,
  `upx --best --lzma`, `upx -t`, `checksums.txt`), never attach a plain `go build` output. A
  release is cut by pushing a `vX.Y.Z` tag: `.github/workflows/release.yml` vets, tests, runs
  that script, attests provenance and publishes. `pwsh -File scripts/release.ps1 -Version <x.y.z>
  -Publish` is the manual fallback. Asset names and `checksums.txt` are what self-update reads
  (`internal/selfupdate.AssetName`); renaming one breaks every installed app's update.
- Self-update never skips the SHA-256 check, never downgrades, and restarts the app through its
  normal quit, never `Worker.CancelAll`: running agent jobs must survive an update.
- `reference/` is an untracked third-party checkout (see README). Read it, never edit it, never
  add it back to git.
- Conventional commits with explicit paths.
