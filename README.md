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

`agentlink.exe` is the whole thing in one program. Started without a command (double-click,
the autostart entry, or only flags such as `-config` / `-no-tray`) it is the desktop app: a tray
icon that runs the node, a settings page and an inbox page in your browser, and an optional agent
that answers requests for you. Started with a command (`agentlink.exe members`, `update`, …) it is
the CLI below. It is a console program so terminals wait for commands and get their output and
exit code; the desktop app leaves its console at once (started from a terminal, it starts itself
detached and hands the terminal back). Started from inside a packaged (MSIX) app, such as a
terminal of Claude's desktop app, whose AppData virtualization would hide its settings from
other instances, it starts itself again outside through WMI and exits (`-unsandboxed` marks
that relaunch).

### Install

Download `agentlink.exe` from the latest GitHub release (the one file of the release), or build it:

```powershell
go build -o bin/agentlink.exe ./cmd/agentlink
```

Copy `agentlink.exe` anywhere and double-click it. Members reach each other over ZeroTier
(or another VPN), the local network, or an address reachable from the internet.

### First run (every member)

1. Start `agentlink.exe`; the settings page opens. Later click the tray icon (or its menu, «Открыть в браузере»).
2. **Ваше имя** is already filled with your Windows name; change it if you like. Names must
   differ between members.
3. **Projects** (see "Projects" below): one person creates a project (**＋ Новый проект**: a name,
   optionally a folder) and sends the others its invite (project menu → **Приглашение**); the
   others press **Присоединиться** and paste it. There is no pairing code on the settings page any
   more.
4. **Участники сети** lists the members this node knows: name, «на связи» / «нет связи», version,
   addresses, **Удалить**. Members on the same LAN or ZeroTier network find each other by
   themselves (see "Members and discovery"). Otherwise type one member's IP under **Добавить
   участника по адресу** (e.g. `10.147.20.9`, port optional) and press **Добавить**: this node
   connects (only if that side has the same code), and every other member learns the address
   and connects too. The page shows your own address ("Ваш адрес для других участников").
5. In general settings, configure **Мои агенты** for Claude Code and Codex once, plus
   **Кто отвечает** if you want a fallback worker. Open a project's **Моя папка** to bind its
   working directory, then **Агенты** to add either or both programs to that project. The
   Windows folder dialog can pick the folder; its path remains editable. The top bar shows
   "связь есть: <name>" (or «на связи N из M») once members are connected; names come from the
   connection.

Saving with only some fields filled is fine: the bar then says what is missing («нет ни одного
проекта…», «пока никого — добавьте адрес участника…»). **Дополнительно** (collapsed) holds the rest:
your own listen address (default: every interface, port 7420, so ZeroTier, LAN and external
addresses all work; set one IP, e.g. the ZeroTier one, to restrict it), the page address
(default `127.0.0.1:7520`, applies after a restart), shared areas, **Искать участников в
локальной сети и ZeroTier** (`discovery`, on by default), how many questions the agent answers
at once («Сколько вопросов агент решает сразу», `max_jobs`, 1–4, default 2) and autostart.
Every error on the page is one sentence saying what to do.

Configs written by v0.1 (long `secret`, `peer_name`, `listen`) still load and work as the legacy
network. Both sides must run v0.2 or later: the handshake changed.
The single `peer_addr` / `peer_name` of v0.5 and earlier is migrated on load into the `peers`
list (`[{"name":…, "addr":…}]`, written back on the next save); a `peer_addr` posted to the
settings API is added to that list.

### Projects

The sidebar has two separate chat groups: **«С другими компьютерами»** for projects shared
with network peers, and **«Мои нейросети»** for local Claude Code ↔ Codex discussions. In either
group, a project is one continuing chat. Network projects have an invite, members and shared
name; every member binds its **own** folder. A local
project belongs only to this computer and may use the same folder as a network project. Its
messages never go to network peers. Existing shared messages stay in their network chat; nothing
is moved automatically.

- **Network project create**: **＋ Новый проект** — a name (1–80 characters) and optionally a folder and your own
  alias for it. **Join**: **Присоединиться** — paste the invite `ALP1.<project>.<epoch>.<secret>.<check>`
  (case and spaces do not matter; a typo fails its checksum), optionally a member's address when
  discovery cannot find one. Joining succeeds once the project's key matches; the shared name then
  arrives from the members («Подключение…» until it does). Pasting the invite of a project already
  here just opens it. At most 32 projects and 64 members per project.
- **Local project create**: `agentlink discuss --with codex` (or `--with claude`) creates or reuses the local
  project/chat for the selected folder under **«Мои нейросети»**. It stays there until cleared.
- **Network project menu** `⋯`: **Участники**, **Приглашение** (hidden `••••` until you reveal it; the
  settings page and the other API answers never carry the secret), **Общее имя** (anyone renames it
  for everyone; the last rename wins), **Агенты**, **Автономия агентов…**, **Моя папка** (folder and
  alias, only yours), **Выйти**. Agent presence, autonomy and folder settings belong to this
  project; the general settings page keeps machine-wide programs and the global pause.
- **No folder**: chats work, but no agent runs for you there: requests that ask you get the held
  status «у участника не выбрана папка проекта», and agent sessions cannot register. A folder change
  or a leave waits until the agent has finished that project's requests («Агент ещё выполняет
  запросы…»); a folder change forgets the agent sessions of the old folder.
- **Chat**: the project row in its sidebar group (name, a dot, «⋯») opens its chat. The
  dot counts the computers with an agent session (Claude Code, Codex) open in the project: grey —
  none, yellow — one, green — two or more (the tooltip names them; peers tell it by their presence
  frames, an older peer counts as none). Compact badges beside the chat show how many Claude Code
  and Codex agents are present; hover or open the activity control for people, agents and their
  current work. «⋯» → **Очистить чат** empties the selected chat; its messages stay as a dated,
  read-only snapshot under **История**. For a network chat the clear reaches its members. The
  chat continues empty (live sessions follow it; requests left unread stay deliverable). In a
  network project, the owner (who started it) invites members and removes them in «Участники»: an
  offline member gets the chat when it connects, from then on (not older messages).
- **Profile**: the chip at the foot of the sidebar sets your nickname and chat color. Both travel
  in your member record to every member (an older version shows your name and a derived color);
  your name stays your identity, and agents' `--to`/`ask` accept the name, the nickname or an
  earlier nickname. A nickname another member already has is refused. The palette icon picks the
  theme, accent color and font (Inter, Manrope, IBM Plex Sans — bundled — or the system font).
- **Leave** tells the members, stops the project and moves its data to
  `data\projects\.left\<id>-<time>` (never deleted). Joining again later is a new member identity.
- **Прежняя сеть**: a network from before projects (a `XXXX-XXXX-XXXX` code or an old long secret)
  keeps running as the project «Прежняя сеть» while its code exists; it is not converted. Its code is
  set by pasting it into **Присоединиться** and removed by leaving it (its history stays). The
  working folder and «Проекты» areas on the settings page belong to it only.
- **Agents**: in a local chat's **Агенты** add Claude Code, Codex, or more than one local agent seat,
  then start, pause or remove each seat there. Pending requests wait while a seat is
  paused. A network project takes no new seats (`409 seats_local_only`): its agents are the Claude
  Code or Codex sessions each member opens in the project folder. When no computer of the project
  has one open, its chat warns that agent messages wait until one is. The project's **Автономия агентов…** pause holds that chat's agent delivery independently
  of the tray's global pause. People can keep chatting; queued agent requests arrive on resume.
  The app also runs the optional fallback worker per project with a folder; workers share
  «Сколько вопросов агент решает сразу». An agent it starts for a project gets
  `AGENTLINK_PROJECT_ID`. The CLI (`send`, `wait`, `chat …`, flag `--project`) and folder hooks
  reach the project of the chat, message or folder they name; `discuss` uses the folder's local
  project (see [docs/agent-usage.md](docs/agent-usage.md)).
- **Storage**: bindings (id, secret, alias, folder, typed addresses) are in `config.json`
  (`project_bindings`, settings `version` 2; the first start of this version keeps a copy of the
  old file as `config.v1.bak.json`); each project's data is in `data\projects\<id>\`.

### Members and discovery

A network is everyone holding the same key: a project's invite, or the legacy code; every member sees every other member. A project's beacons are `v: 3` with its own tag (derived from the invite's secret, no Argon2 needed at 128 bits); the rest below holds for projects and the legacy network alike.

- **Member table.** Each node keeps `{name, node id, addresses, version, last seen}` for every
  member, itself included, in `data\members.json`, and exchanges the whole table with each peer
  that announced the `members` capability, on connect and whenever a merge changed it. Records
  are last-writer-wins per member (a nanosecond version, ties broken deterministically). Every
  node dials every live member it learns, trying each known address (full mesh, at most 64
  members, 8 addresses each). An address a node reached is added to that member's record, so an
  address added by hand on one node spreads to all.
- **Removal.** **Удалить** (or `agentlink remove`) writes a tombstone that spreads the same way:
  every node closes its session to that member, stops dialing it and refuses it; the removed
  node is told («вас удалили из сети»). Adding its address by hand again brings it back.
- **Names.** Each node has a random id (`data\node_id`). If two machines use one name, the one
  with the smaller id keeps it on every node; the other is refused and shows «ваше имя уже
  занято другим участником».
- **LAN discovery.** Every 5 s a node sends a beacon to UDP `239.255.74.21:7421` (multicast)
  and to the directed broadcast of each IPv4 network, on every running non-loopback interface,
  ZeroTier included, and listens on UDP 7421 (shared with other instances on the machine). The
  beacon is `{"t":"agentlink","v":2,"net":"<16 hex>","node":"<name>","id":"<node id>","port":7420}`:
  `net` is Argon2id of the session key (64 MiB, 3 passes, 4 lanes, fixed salt
  `agentlink/network-tag/v2`, computed once per key at start), so only nodes with the same code
  react, and the code is not in it. Beacons of older versions (`v` 1, a PBKDF2 tag) are still
  recognised, so a new node dials old ones it hears; a new node never sends one. A node that hears its own network from a member it has no
  session with dials the sender's IP at `port` (once per address per 15 s); the TCP handshake
  still authenticates, so a replayed or forged beacon causes at most a failed dial.
- **Older versions** (without `members`) still connect and exchange messages; they are listed
  as «старая версия», get no table and dial only their own configured peer. A version without
  the PAKE handshake (see "Security") connects only from a private address and is listed as
  «старая версия — вход без защиты кода, только из локальной сети».
- With several members `send` needs a recipient: an empty «Кому» / `--to` fails and lists the
  names. The settings page lists every member with its state; the tray icon's tooltip counts
  them («agentlink — На связи 7 из 12»).

The web UI is in Russian; every visible string lives in `internal/app/strings.go`, so a
second language means a second map, not a page rewrite. It is a single-page app (Vue 3,
Nuxt UI, Tailwind) in `internal/app/web`: a slim sidebar with separate network and local chat
groups, one centered column per page with Nuxt UI chat components for the messages,
light/dark/system theme. It works offline: the Lucide icons are bundled into it and the fonts
are the system's. Its build output
`internal/app/web/dist` is committed and embedded in the binary, so `go build` needs no Node.

Settings, including the project secrets and the legacy code, live in `%APPDATA%\agentlink\config.json` (per user, never in a
repo); messages in `%APPDATA%\agentlink\data`, the log in `%APPDATA%\agentlink\agentlink.log`.
Autostart is the `agentlink` value under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
(the quoted path of the executable). The settings page and the tray menu show it as Windows has
it: an entry switched off in Task Manager (`...\Explorer\StartupApproved\Run`) counts as off, and
turning it on again clears that mark.

The tray icon: a left click opens the app's chats (/ui/inbox), and another click activates the
existing app tab as it is, without reloading it, in the same browser profile when the browser
permits it. A right click shows a
fixed menu — «Запускать вместе с Windows», «Открыть в браузере», «Выход». Members, messages and
updates live on the web pages; the tooltip carries the state and a newer version. Browsers decide
whether a tab opened by the operating system may close itself: when they refuse, the small launcher
tab may remain open, but the named app tab is still reused instead of creating another one.

### Updates

The app updates itself from this repository's GitHub releases. **Обновления** at the bottom of
the settings page shows the version, **Проверить обновления** (the latest
release, or «У вас актуальная версия»), **Обновить до X.Y.Z** when a newer one exists, progress
and errors, and the **Обновлять автоматически** switch (`auto_update` in the config, on by
default; it saves at once and is not part of **Сохранить**). With it on, the app checks a minute
after start and then every 6 hours (±10%) and installs a newer release by itself.

A check reads the latest release's tag from the redirect of
`https://github.com/UberMorgott/agent-link/releases/latest`, without the REST API (whose
unauthenticated limit, 60 requests an hour, is shared by everyone behind one IP address). An
install asks the API once for that release by tag
(`api.github.com/repos/UberMorgott/agent-link/releases/tags/vX.Y.Z`, no token), downloads
`github.com/UberMorgott/agent-link/releases/download/vX.Y.Z/agentlink.exe` and checks the
file's SHA-256 against the `digest` (`sha256:<hex>`) GitHub reports for that asset. It refuses a
mismatch, a release that is older or equal, and a release whose asset has no SHA-256 digest.
When GitHub rate-limits that API call nothing is installed: the page says when to retry
(«GitHub временно ограничил запросы, повторите после HH:MM»), and automatic updates retry
after that time instead of waiting the usual hours. Only then does it swap `agentlink.exe`: the running file is
renamed to a hidden `.agentlink.exe.old` and the new one takes its place; a failure puts the old
one back. The app then starts the new executable
(with `-restarted`, which waits up to 30 s for the API address) and quits the normal way, never
the kill path: running agent jobs stay up and the new app reattaches to them. A Claude Code
background message waiter from the old version keeps waiting (it never wakes an idle session
for the update) until the session's next Stop starts the new one, which asks it to hand over (the
old one then ends; a waiter older than 0.6.57 ignores that and ends with its session or its
24-hour life); an old stdio MCP server keeps serving, runs each tool call in the new executable
and ends when its Claude Code or Codex session disconnects. The app deletes the `.old` files once
those old processes exit (every second for a minute after start, then every ten minutes), logging
what it removed, and at start removes old agent-link plugin copies from the Claude Code and Codex
plugin caches that the client no longer uses (not in `installed_plugins.json`; for Codex, not the
current version), a week after they stopped being used and only when no file in them is open, and an `agentlink-tray.exe` left next to it by an older release
(0.4.x shipped the tray app separately); an autostart entry that still starts
`agentlink-tray.exe` is pointed at `agentlink.exe`. 0.4.x apps cannot update to this layout by
themselves: download `agentlink.exe` once by hand, quit the old tray app and start the new one. A plain `go build` has version `dev` and never updates itself;
`agentlink update` (below) is the same for the CLI.

### Handler agent

The agent has to be installed and logged in on the answering side, in any of the ways below.
The Codex desktop app alone is enough: its own `codex.exe` runs `codex exec` and uses the
app's login (`%USERPROFILE%\.codex`, or `CODEX_HOME`), so no `npm` and no `codex login`.

The app looks for the agent in this order and runs `<program> --version` on each hit (it must
answer and name the agent); the first one that does is used. Inside a versioned folder the
newest version wins (by folder version, else by file time).

| Agent | Install | Program | Source |
| --- | --- | --- | --- |
| both | on `PATH` | `codex` / `claude` (a Store alias in `%LOCALAPPDATA%\Microsoft\WindowsApps` is skipped: it starts the desktop app) | — |
| Codex | `CODEX_CLI_PATH` | the path in that variable | [openai/codex#40700](https://github.com/openai/codex/issues/40700) |
| Codex | installer (`install.ps1`) | `%CODEX_INSTALL_DIR%\codex.exe`, `%LOCALAPPDATA%\Programs\OpenAI\Codex\bin\codex.exe` | [install.ps1](https://chatgpt.com/codex/install.ps1) |
| Codex | npm | `%APPDATA%\npm\codex.cmd` | [@openai/codex](https://www.npmjs.com/package/@openai/codex) |
| Codex | winget `OpenAI.Codex` | `%LOCALAPPDATA%\Microsoft\WinGet\Links\codex.exe` | `winget show OpenAI.Codex` |
| Codex | desktop app (Store) | `%LOCALAPPDATA%\OpenAI\Codex\bin\<hash>\codex.exe` — the app's copy; its `WindowsApps\...\app\resources\codex.exe` cannot be started by the user | seen on a real install, [openai/codex#40700](https://github.com/openai/codex/issues/40700) |
| Codex | VS Code / Insiders / Cursor / Windsurf extension `openai.chatgpt` | `<editor>\extensions\openai.chatgpt-<ver>*\bin\windows-x86_64\codex.exe` | [openai/codex#43701](https://github.com/openai/codex/issues/43701) |
| Claude | native installer | `%USERPROFILE%\.local\bin\claude.exe` | [setup docs](https://code.claude.com/docs/en/setup) |
| Claude | npm | `%APPDATA%\npm\claude.cmd` | [setup docs](https://code.claude.com/docs/en/setup) |
| Claude | winget `Anthropic.ClaudeCode` | `%LOCALAPPDATA%\Microsoft\WinGet\Links\claude.exe` | `winget show Anthropic.ClaudeCode` |
| Claude | Claude desktop app | `%APPDATA%\Claude\claude-code\<ver>\claude.exe` (the app's own `claude.exe` is not the CLI) | seen on a real install, [anthropics/claude-code#62690](https://github.com/anthropics/claude-code/issues/62690) |
| Claude | VS Code / Insiders / Cursor / Windsurf extension | `<editor>\extensions\anthropic.claude-code-<ver>*\resources\native-binary\claude.exe` | seen on a real install |

`<editor>` is `%USERPROFILE%\.vscode`, `.vscode-insiders`, `.cursor` or `.windsurf`.

**Мои агенты** in general settings shows Claude Code and Codex separately. For each,
**Найти заново** searches installed programs and **Указать…** picks an executable in the Windows
file dialog. The paths are saved as `claude_path` and `codex_path`; the older `agent_path` stays
for compatibility with the fallback handler. The tray started from Explorer or autostart may
not see a `PATH` entry an installer just added. Search runs on save when a program is missing,
and again when a saved program disappears after an app update. A project seat uses its
  provider's configured program; adding or pausing a seat is done in that project's **Агенты**.

The primary answerer is a **live session**: a Claude Code or Codex session open in the
«Рабочая папка» or a «Проекты» folder registers with the node through its hooks, reads the
messages that are unread for its folder and answers in the chat (see
[docs/agent-usage.md](docs/agent-usage.md#live-sessions-on-the-node)). The worker below is an
optional fallback: **Автоответ агентом, если сессия не открыта** (`auto_answer`, **off by
default**, also for configs from earlier versions: turn it on to keep automatic answers). With
it on and a handler other than "None", a request (a message that asks this node) for an area
with no live session registered runs the agent in the working folder (or the area's project)
with the message as the prompt, up to `max_jobs` requests at a time (default 2; the rest wait
and start in arrival order, answers may come back in any order), and sends the agent's final
answer back as a reply. A request goes to the worker or to a session, never both. With it off,
messages wait unread until a session opens (the sender sees «доставлено», then «прочитано»).
After 8 agent-to-agent hops without a person's message, the automatic handler stops
replying to that chain. The message reaches the session on its next human turn as
information and can be marked read; the sender sees normal delivery and read ticks.

Jobs are durable. Each request is recorded in `data\jobs\<id>.json` before it is acknowledged
and moves `queued` → `running` → `completed` | `failed` (with attempts, timestamps and the
error). The sender gets small status updates (`queued`, `running`, then `running` with an
`activity` line such as `Read docs/index.md`, `Grep 'Worker' internal`, `Run rg -n Worker`,
`thinking`, `writing answer`) and a final reply whose `job_status` is `completed` or `failed`.
Activity goes out at most once per 3 s and only when it changed (an unchanged one is repeated
every 2 min). The sender's inbox page shows each request's latest status, «сейчас: …» under a
running one, and the answer. A duplicate request id never runs twice. Queued jobs survive quit,
crash and settings changes and resume in order.

A running agent survives the app: it is started detached (its own process group, outside the
app's job object, no console) with stdin, stdout and stderr on files in `data\jobs\<id>\`
(`<attempt>.in`, `.out`, `.err`, `.last`), and the job records its pid, the process start time
(so a reused pid is never mistaken for it), the agent session id and how much output was
already relayed. Quitting, restarting, saving settings or updating the app leaves the agent
running; the next start reattaches to it, keeps relaying its activity and sends its answer as
usual, so it runs once. An agent that finished while the app was down is answered from its
output file. One that died mid-run (a reboot, a crash) is resumed in its own session
(`claude --resume <id>`, `codex exec resume <id>`, same permissions) with a short "continue"
prompt; without a session to resume it starts over once. Died a second time, the job fails with
a reply. A completed job's run files are removed; a failed one keeps them for diagnosis. An
agent error, 10 minutes without any output from the agent («агент завис (нет активности 10 мин)»),
or the 60-minute cap on one attempt (counted from its start, across restarts) kills the agent's
process tree and fails the job at once, without a retry; so does `Worker.Cancel` (the app's
normal stop leaves agents running). A long turn that keeps printing is never cut at 10 minutes.
A request answered on this node another way (a person in the inbox, or an interactive session
that the hook told about it, replying with `--reply-to`) stops its job too: a queued one never
runs, a running agent is killed, and the sender gets a `completed` status instead of a failure.
Switching the handler to "None" fails jobs that were still waiting, with the reply "no handler
configured".

- Claude Code: `claude -p --output-format stream-json --verbose --permission-mode bypassPermissions --disallowedTools <protected paths> --session-id <new uuid>` (resume: the same flags with `--resume <uuid>` instead of `--session-id`) — `assistant` events' `tool_use` / `thinking` / `text` blocks become activity, the `result` event is the answer.
- Codex: `codex exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --color never --output-last-message <file> -` (resume: `codex exec resume --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --output-last-message <file> <thread_id> -`; the thread id comes from `thread.started`) — `item.started|updated|completed` events (`command_execution`, `reasoning`, `agent_message`, …) become activity, the last-message file is the answer, `turn.failed` / `error` is the failure reason.

A network request may not change the local user's agent instructions, memory or config
(`~/.claude/**`, `~/.claude.json`, `~/.codex/**`, any `.claude/` or `.codex/` folder, `CLAUDE.md`,
`CLAUDE.local.md`, `AGENTS.md`, `AGENTS.override.md`). What is enforced and what is not:

- Claude Code: enforced by `Edit(...)` deny rules in `--disallowedTools` (`worker.ProtectedPaths`).
  [Deny rules block in every mode, including `bypassPermissions`](https://code.claude.com/docs/en/permission-modes);
  they cover the Edit/Write tools and the file commands and redirects Claude Code recognizes in
  Bash/PowerShell (`sed`, `tee`, `> file`, `Set-Content`, `Remove-Item`), but
  [not a script or program that opens files itself](https://code.claude.com/docs/en/permissions#read-and-edit)
  (python, node, git). The OS sandbox that would close that gap
  [is not available on native Windows](https://code.claude.com/docs/en/sandboxing). The rules
  also block edits to a project's own `.claude/` or `.codex/` folder.
- Codex: prompt-only. Its path deny rules live in sandboxed
  [permission profiles](https://learn.chatgpt.com/docs/permissions), which
  `--dangerously-bypass-approvals-and-sandbox` turns off; with the Windows `unelevated` sandbox
  Codex refuses to start a profile with deny rules at all (codex-cli 0.155.1).
- Both agents are told to refuse such a request and say so in the reply.

On the sending side an unanswered request is marked «нет вестей от собеседника N мин» when the
peer has sent nothing about it for 5 minutes while connected (a request `queued` behind other
jobs is exempt), or the peer has been disconnected for 5 minutes. It is only a display: the
message is still resent by the normal outbox until the peer ACKs it.

The prompt goes through stdin, never through a shell. **Full capability:** a paired peer's
request is the agent's task: it edits files and runs commands (build, tests, git, gh, network)
without prompts or sandbox, in the working folder or the project mapped to the request's area,
and answers with what it did and the evidence. It refuses only hard-to-reverse actions (force
push, history rewrite, mass delete). Only pair with someone you trust to run commands on this
machine.

`docs/agent-usage.md` is the reference for a coding agent that wants to use the link itself:
which project a command reaches, how to ask in a chat and read the answer, and what the hooks do.

### Claude Code plugin

`plugins/agent-link` is a Claude Code plugin: the hooks (the same entries `agentlink hook install
claude` writes), the `agentlink` MCP server (`agentlink mcp`) and the skill, updated with the
plugin. Install it from this repository's marketplace:

```powershell
claude plugin marketplace add UberMorgott/agent-link
claude plugin install agent-link@agent-link
```

**Codex.** The same folder is a Codex plugin (`.codex-plugin/plugin.json`): it brings the MCP tools
to interactive Codex sessions. The desktop app installs it at start when this machine has Codex
and Codex lists no agent-link plugin yet (a disabled one stays disabled), with Codex's own
commands, touching no other plugin:

```powershell
codex plugin marketplace add UberMorgott/agent-link   # only when the marketplace is missing
codex plugin add agent-link@agent-link
```

A failed install is shown under the working folder in the settings. Codex runs a plugin's hooks
only after you trust them: open Codex once and pick «Trust all and continue» (or `/hooks`), then
restart agentlink. Until then the folder hooks keep delivering; once the plugin's hooks are
trusted the app removes its Codex folder hooks, so no hook runs twice.

**Updating the plugin.** Claude Code refreshes a third-party marketplace only when its auto-update
is on (off by default), so the installed skill, hooks and `.mcp.json` stay at the commit of the
install (`~/.claude/plugins/known_marketplaces.json` shows `lastUpdated`). The desktop app checks
at every start: when the installed copy's `.codex-plugin/plugin.json` version is not the one the
running `agentlink` was built with, it runs these commands itself (Codex:
`codex plugin marketplace upgrade <marketplace>` and `codex plugin add agent-link@<marketplace>`),
never editing the plugin cache; a plugin still stale after that is named at the start of every
session in a line for you (never the model) with the exact commands. By hand, then start a new
session:

```powershell
claude plugin marketplace update agent-link
claude plugin update agent-link@agent-link
```

or turn on auto-update once: `/plugin` → Marketplaces → agent-link → Enable auto-update. A copy of
the skill in `~/.codex/skills/agent-link` is never refreshed: delete it (Codex then uses the
plugin's) or copy `plugins/agent-link/skills/agent-link/SKILL.md` over it again. The `agentlink`
executable updates by itself; an MCP server started before an update runs each tool call in the
updated executable (`agentlink mcp-call`), but the desktop app (the node) runs the old code until it
restarts.

The plugin runs agentlink through its launcher `bin/agentlink.cmd` (cmd.exe built-ins only, no
PowerShell or Git Bash needed), which takes the first of: `%AGENTLINK_EXE%`, the path the desktop
app writes at every start to `%APPDATA%\agentlink\executable.path`, `agentlink.exe` on `PATH`.
The plugin ships no executable: an `agentlink.exe` in its own `bin` (say, one left in a source
checkout and copied along by a reinstall) is never run, so the plugin always runs the desktop
app's self-updated binary.
Start the desktop app once after installing it, or set `AGENTLINK_EXE`. The plugin's hooks and MCP
server are Windows only (the launcher is a `.cmd`); elsewhere use `agentlink hook install claude`.
The one `.mcp.json` serves Claude Code and Codex: it puts the plugin's `bin` first on `PATH` from
the host's `PLUGIN_ROOT` or `CLAUDE_PLUGIN_ROOT` variable and runs `agentlink.cmd mcp`; a host that
sets neither (Codex 0.155) puts the folder of the `agentlink.exe` named in
`%APPDATA%\agentlink\executable.path` first on `PATH`, else gets `agentlink.exe` from `PATH`.
So on Codex 0.155 `AGENTLINK_EXE` is not used for the MCP server: it needs the marker
`%APPDATA%\agentlink\executable.path` naming a file called `agentlink.exe` (start the desktop app
once), or `agentlink.exe` on `PATH`.

**One source of hooks per session.** Claude Code runs a plugin's hooks and the settings' hooks
side by side, so with the plugin enabled remove the others, or every message is handled twice:
after `agentlink hook install claude` delete the agentlink entries (`… hook claude`) from `hooks`
in `~/.claude/settings.json` (and `.claude/settings.json` for `--scope project`), and in the
desktop app do not pick Claude Code in «Кто отвечает» for folders where you use the plugin (the app
writes its own hook into `<folder>/.claude/settings.local.json`, see
[Folder hooks](docs/agent-usage.md#folder-hooks-of-the-desktop-app)).

### Agent skill

`plugins/agent-link/skills/agent-link/SKILL.md` teaches a coding agent agent-link: the MCP tools
first, the CLI as a fallback (projects, chats, `--project`, `--chat`, `--ask`, hooks, routing,
common errors). The Claude Code plugin brings it; without the plugin copy the folder into your
skills:

```powershell
Copy-Item -Recurse -Force plugins\agent-link\skills\agent-link "$env:USERPROFILE\.claude\skills\"
```

Other agents (Codex): copy it wherever your agent loads skills, or point `AGENTS.md` at it.
`agentlink hook install` does not install the skill; copy it again after an update.

## CLI

### Build

```powershell
go build -o bin/agentlink.exe ./cmd/agentlink
```

After changing the web UI (`internal/app/web/src`), rebuild its bundle and commit `dist/` with
the sources (Node 24):

```powershell
cd internal/app/web
npm ci
npm test            # vitest: chat, settings, participants, event stream, launcher
npm run build       # vite build into dist/, then the vue-tsc typecheck
```

## Usage

```powershell
$env:AGENTLINK_SECRET = 'K7Q2-MXAB-CDEF'   # the same code on every machine (or a 16+ byte secret)
agentlink serve --config node.json                         # the node (keep running)
agentlink send  --config node.json --to node-b --body "hi" # into the project chat; prints the message id
agentlink send  --config node.json --body "hi"             # no --to: the only other member (several: error listing them)
agentlink send  --config node.json --to area:dev --body "build is green"
agentlink send  --config node.json --to node-b --body "done" --reply-to <id>
agentlink wait  --config node.json --timeout 0             # blocks; JSON line per message
agentlink inbox --config node.json --limit 20              # recent in/out, non-destructive
agentlink chat unread --config node.json                   # unread messages of this node, oldest first
agentlink chat ack --config node.json --ids <id,...>       # mark read: the authors get read receipts
agentlink session pin --session <thread-id> --project <id> # prefer one Codex chat for new untargeted project messages
agentlink session unpin --session <thread-id> --project <id> # remove that preferred recipient
agentlink members --config node.json                       # member table, one JSON line each, this node first
agentlink discuss --with codex --body "Review this design" # ask a local Codex seat in this folder's project; wait for its reply
agentlink discuss --with claude --prompt-file question.md --async # post and return IDs immediately
agentlink add    --config node.json --addr 203.0.113.7     # dial a member's address; it spreads to all members
agentlink remove --config node.json --name node-c          # remove a member from the whole network
agentlink version                                          # this build's version (dev: not a release)
agentlink update --check                                   # is there a newer release?
agentlink update                                           # install it next to agentlink.exe
```

`update` replaces `agentlink.exe` with the latest release, verified as in "Updates"; a running
desktop app keeps the old version until it restarts (with automatic updates on, its next check
does that).

`wait` exits 0 after printing every undelivered inbound message (they are then marked
delivered), exits 2 with no output when `--timeout` (seconds or a Go duration, `0` = forever)
expires, and exits 1 on errors. It returns requests and replies only: a handler's `queued` /
`running` status updates never wake it, so a waiting session sees just the final reply (check
its `job_status`: `completed` or `failed`). Progress is visible in `inbox`: an outbound request
carries `job_status`, `activity` while it runs, `last_heard` and, when the peer has gone quiet,
`no_news_min`.

`discuss` is the local Claude Code ↔ Codex connector. It creates or reuses a **local** project
and its chat for the working folder, even when that folder also has a network project. Messages
in this local chat stay on this computer and never appear in the network project's shared chat.
Existing network messages are not moved into the local chat. It adds a seat for
`--with claude` or `--with codex` if needed and writes the question into the local chat.
`--folder <path>` selects another folder; `--body <text>` and `--prompt-file <path>` are
alternatives. By default it waits up to 10 minutes
for the asked seat's direct reply and prints JSON with `project`, `chat`, `id`, `seat` and
`reply`. `--timeout <duration>` changes the wait (maximum 15 minutes); on expiry it prints the
IDs with `timed_out: true` and exits 2; a held question (`held: true`: past the hop limit, or the
seat cannot answer) exits 3. `--async` prints the IDs immediately. A global,
project or seat pause returns `queued: true` with IDs promptly: the request remains in the chat
and reaches the agent after resume. The MCP `discuss` tool offers the same flow with
`{with, body, folder?, timeout?, async?}`. Image and review flag parity with the older `cx.ps1`
wrapper is not implemented.

Use `agentlink send --chat <network-chat-id> --ask <member> --body "<question>"` to ask a member
on another computer in the shared chat. Use `agentlink discuss --with codex` or
`agentlink discuss --with claude` to ask your
own agent in the separate local chat. A project's agent pause applies to that chat alone;
the tray pause/play control applies to all local agents. People can keep sending messages
during either pause; requests for paused agents wait until resumed.

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

- `listen` — peer TCP listener: the ZeroTier address, or `:7420` for every interface.
- `api` — local HTTP control API; must be a loopback address, anything else is refused.
- `data_dir` — outbox, inbox and sent messages as JSON files; relative to the config file.
- `secret_env` — name of the environment variable holding the pairing code (`XXXX-XXXX-XXXX`,
  case-insensitive; a legacy 6-character code still works, and `serve` warns about it when
  `listen` is not a private address) or a legacy shared secret of 16+ bytes.
- `areas` — topics this node subscribes to; `--to area:NAME` fans out to every peer that announced it.
- `peers` — addresses to dial; `name` is optional. A peer without a name is learned from the
  handshake. If every peer has a name, only those names may connect; with a nameless peer (or
  none) any node holding the code may. Nodes keep one session per peer. Members learned from
  the table are dialed too (see "Members and discovery").
- `discovery` (optional, default off for the CLI, on in the tray app) — send and answer LAN
  beacons; `discovery_port` replaces UDP 7421.

Loopback examples: `examples/node-a.json`, `examples/node-b.json`. `scripts/e2e-local.ps1`
builds the binary, starts both, sends a→b, replies b→a and stops them. `scripts/e2e-worker.ps1`
runs two tray apps headless (`-no-tray`) with a fake agent and checks the automatic reply;
`-RealClaude` / `-RealCodex` use the installed `claude` / `codex` instead (and must show at least
one activity line at the sender); `-WorkDir <dir> -Prompt <text>` asks the real agent your own
question. `scripts/e2e-parallel.ps1` runs b with `max_jobs` 2, a fake streaming agent and a 4 s
idle timeout (`agentlink -handler-idle-timeout`), sends three requests and checks that two
run at once, activity reaches a, and the one that hangs fails by the idle timeout while the
others complete.
`scripts/e2e-tray.ps1` plays both people on one machine: two tray apps with their own settings
folders (`-config`) and API ports (`-api`), set up, messaged and quit through the web UI
endpoints; it also quits and restarts b in the middle of three slow jobs (job 1's agent keeps
running, all complete, each runs once), checks that a settings save leaves a running agent
alone, that an agent killed while b is down starts over once and killed again fails the job,
that no agent processes are left, and that a restart keeps the inbox. `-Address <ip>` binds the peers to e.g. the ZeroTier IP.
With a non-default `-config` the app never touches the autostart entry, and `POST /ui/api/quit`
(token-guarded, the same path as the tray's Quit) exits it.

`scripts/demo-local.ps1` is the same two-people setup but as a live demo, not a test: it starts
`morgott` and `nikita` with their settings under `$env:TEMP\agentlink-demo`, API ports 7530/7531,
peers on loopback, area `demo`, both answering with real Claude Code over `-WorkDir`
(default `E:\DEV\CodeDungeon`). It proves the round trip with one real question, prints both
`/ui/inbox` URLs and leaves the apps running with their tray icons. Stop them from a tray icon's
Quit or with `scripts/demo-local.ps1 -Stop`.

## Waking a Claude Code session

With the hooks below a live session is woken by itself. Without them, run `wait` as a background
shell command. When a message arrives the command exits, the harness reports the completion, and
the agent reads the JSON lines from its output:

```text
agentlink wait --timeout 0   (run_in_background; --config <node.json> for a node started with serve)
```

After handling the messages (and replying with `send --reply-to`), start `wait` again.

### Hooks: a live session in the folder

A Claude Code or Codex session open in a folder of this node is connected through its own
hooks. Install once (the desktop app does it for its folders, see below):

```powershell
agentlink hook install claude     # ~/.claude/settings.json; --scope project for .claude/settings.json
agentlink hook install codex      # ~/.codex/hooks.json; then trust the hook with /hooks in Codex
```

The hook (`agentlink hook claude|codex`) registers the session for its folder on `SessionStart`
(every event is a heartbeat, `SessionEnd` ends it) and, on `SessionStart`, `UserPromptSubmit`,
`PostToolUse` and `Stop`, hands the model the node's **unread** messages for that folder (all
ages: sender, human or agent, chat, full text, the reply command), then marks them read, so each
is delivered once and the sender sees «прочитано». The person at the session sees a line like
«agent-link: 2 сообщения от KPECTIK — беру в работу». While the session works on them, what it
does («читает src/x.go», «правит …», «запускает go») shows in the sender's chat. Claude Code
also gets a background waiter (`asyncRewake`) that wakes an idle session when a message arrives;
an idle Codex session is woken by the node with `codex queue --thread <id>` (codex 0.149 or
later found on this machine), else it reads new messages at its next event. One message goes to
one session: a chat's messages go to the live session that last sent in it, so replies reach the
asking session. The hook stays silent when the node is not running and does nothing inside a
handler job or a headless run (`claude -p`, `codex exec`). Details:
[docs/agent-usage.md](docs/agent-usage.md#hearing-about-messages-in-a-live-session-hooks).

The desktop app installs **both** Claude Code and Codex hooks in each bound project folder,
regardless of which fallback handler is selected (Claude Code:
`.claude/settings.local.json`, Codex: `.codex/hooks.json`). It also installs them in the legacy
working folder, when configured. On start and save it replaces old agentlink entries in place;
changing a folder removes only its own entries from the old place. A session in a project's
folder gets that project's messages; a session elsewhere gets none, apart from the legacy
working folder's legacy messages. Codex runs a new hook only after you trust it once with
`/hooks` in that folder.

**Agent autonomy (off by default).** Each project's **Автономия** setting controls how far its
agents work by themselves: **off** (the default, also for projects that had auto-open off) —
no session is opened by the node, a message waits for a session of the folder; **asked** (what
auto-open on was) — a message that asks you while no session (active or idle) is live in the
project's folder makes the node open a **new** agent session there: in the agent's desktop app
(the first turn runs headless with the messages as its prompt, the app shows it live) or in
Windows Terminal; **full** — a message of another member that only informs you opens one too,
with no hop limit by default (settable, 0 = none; off and asked keep 8) and finite budgets: at
most 30 autonomous turns (wake, launch, seat turn) per hour and 240 minutes of continuous
autonomous work (both settable); an exhausted budget pauses autonomous delivery for that
project, the tray shows one notification, and «Продолжить» in the project setting goes on.
**An opened session runs with your full permissions** (Claude `bypassPermissions`, Codex no
approvals and no sandbox) and acts on what other members' agents wrote without asking: use
asked or full only for projects whose members you trust. **Global pause:** the tray control
switches between pause and play; the general settings page has the same saved switch. It ends
agent turns the node runs and releases automatic delivery. People can continue to send and
receive chat messages, while this machine's agents receive nothing and do not respond. Those
messages stay unread and reach the agents after resume.
Details: [docs/agent-usage.md](docs/agent-usage.md#hearing-about-messages-in-a-live-session-hooks).

## Delivery

- Sent messages are written to `outbox/<peer>/` first and removed only when the peer ACKs, so an
  offline peer gets them on the next connection; unACKed messages are resent periodically.
- A project has one active chat. The legacy network still identifies conversations by members
  and area. A person's clear moves the project chat to the next generation everywhere, and a
  message that crossed the clear is kept in the previous chat's history.
- Read receipts (`kind: "receipt"`, capability `receipts-v1`) go back to the author through
  the outbox when a session acks a message (or the worker takes it); the author shows
  queued → delivered → read → answered per recipient. Unread state is on disk per node.
- Inbound messages are persisted, and requests recorded as handler jobs, before the ACK; both
  are deduplicated by id, so resends are idempotent. Status updates and handler replies use ids
  derived from the request, so re-sending them after a crash is deduplicated too.
- Status updates are `kind: "status"` messages; a node without that field treats them as empty
  replies, so update both sides together.
- Each session carries a heartbeat frame (`{"type":"hb"}`) every 15 s. Once a peer has sent one,
  45 s without any frame from it closes the session: the status turns «нет связи» and the dialing
  side reconnects with its usual backoff. A v0.2 peer sends no heartbeats and ignores them (and
  `activity`); it is never timed out, it just shows no activity.
- Versions interoperate: since v0.4 `hello` carries `proto` (protocol version, now 6) and `caps`
  (`caps`, `hb`, `activity`, `job-reattach`, and since v0.6 `members` and `pake`); v0.6 adds
  `node_id`, `app` (version), `port` and `pake` (the CPace share) to `hello` and the `members` frame. A peer that sends neither is an older version
  with no optional capabilities; it still connects and exchanges messages. Unknown frame
  types, unknown fields, fields of an unexpected JSON type and lines that do not parse are
  skipped (debug log), during the handshake and after, and never close the session. A feature
  newer than v0.3 is only used towards a peer that announced its capability (`Node.PeerHas`).
- `wait` marks messages delivered as it returns them; a `wait` killed mid-response can lose that
  batch from `wait` (it stays visible in `inbox`).

## Security

- The tray app listens on every interface by default (so LAN and external addresses work).
  Anyone who can reach TCP 7420 can attempt the handshake. To narrow that, set «Мой адрес» /
  `listen` to the ZeroTier IP and firewall the port to the members, or switch discovery off on
  untrusted LANs.
- **Handshake: a PAKE.** Peers run CPace (draft-irtf-cfrg-cpace-20, ristretto255 + SHA-512,
  checked against the draft's test vectors; group arithmetic from `github.com/gtank/ristretto255`
  on `filippo.io/edwards25519`). The password is the session key, HKDF-SHA256 of the normalized
  code (`agentlink/pair-code/v2`; `v1` for a 6-character code). The dialer's nonce is the
  session id; each side's name and node id are bound into the key. Both sides then prove the key
  with an HMAC-SHA256 tag derived from it (the acceptor first; the dialer answers only after
  checking it; its tag also covers both raw hello lines). The code is never sent, and **no transcript, recorded
  or obtained by connecting, lets anyone test code guesses offline**: an attacker who takes
  part in a handshake gets exactly one guess, and a passive one none.
- **Online guessing** is slowed per source (an IPv4 address or an IPv6 /64): after 5 failed
  inbound handshakes, further connections are closed unread for 1 s, doubling per failure up to
  5 min; a success clears it, 30 min without failures forgets it. At most 4096 sources are tracked.
- **Codes.** New codes are 12 symbols of a 32-letter alphabet, 60 bits: at even a thousand
  online guesses a day, hopeless. A legacy 6-character code (about 31 bits) still works; the
  app and `serve` warn about it while the listener is reachable beyond private networks.
- **Legacy handshake.** Versions without `pake` authenticate with HMAC-SHA256 keyed directly by
  the session key, and the acceptor answers any hello with such a MAC: one recorded or requested
  MAC lets the code be brute-forced offline (minutes for a 6-character code). A node therefore
  runs it only with a peer at a private address (loopback, RFC 1918, fc00::/7, link-local, or a
  network of this machine's ZeroTier adapter), logs a warning and marks the member «старая
  версия»; never with a name that has had a PAKE session since the node started (no silent
  downgrade); a public address gets no MAC at all. Update every member and switch to a new code
  to leave it behind.
- **Discovery tag.** A heard beacon still lets its hearer test code guesses offline, at Argon2id
  cost (64 MiB per guess). With a new code that is out of reach; with a 6-character code on an
  untrusted LAN, switch discovery off. Old members' v1 beacons (PBKDF2) keep leaking their
  cheaper tag until they update.
- Any member can add addresses to the table and remove members; every member is trusted alike.
- The CLI's code or secret lives only in an environment variable; CLI configs are safe to commit.
- **Session: sealed records.** After the handshake every frame of a PAKE session, both ways,
  starting with the acceptor's `ok`, is a record: a 4-byte big-endian length, then the frame
  sealed with ChaCha20-Poly1305 (`golang.org/x/crypto`), the length as associated data. Each
  direction has its own key, HKDF-SHA256 of the CPace ISK salted with the hash of both hello
  lines, so a hello altered on the way (areas, caps, version) breaks the session. The nonce is a
  per-direction record counter that is never sent: a dropped, replayed, reordered, reflected or
  altered record fails to open and the connection is closed, as it is after 2^48 records under one
  key. A record holds at most 1 MiB. A man in the middle sees only lengths and timing.
- **Not protected: legacy sessions and the hellos.** A session with a pre-v0.6 member (private
  addresses only) stays plain newline-delimited JSON, unauthenticated per frame: on that path
  anyone who can see the traffic reads it, and an active attacker can inject or alter frames.
  Confidentiality there comes from ZeroTier or the private LAN alone. The hellos themselves are
  readable by anyone on the path (names, node ids, areas, program version, port).
- **Connection floods.** At most 64 inbound connections may be in the handshake at once, 8 per
  source (IPv4 address or IPv6 /64); more are closed unread, and a handshake that does not finish
  within 10 s is dropped. Many sources can still keep those 64 slots busy and delay real members'
  handshakes (not their established sessions), and nothing limits traffic on an authenticated
  session.
- Not protected either: a LAN attacker who plays a legacy peer under a new name gets a legacy MAC
  (offline oracle) — that is why the weak code has to go.
- The control API listens on loopback only and rejects browser requests (`Origin` header) and
  non-loopback `Host` headers, but any local process on the machine can use it.
- The tray app's web pages share that address. Their API calls need a random per-run token that
  is embedded in the page and must come from the same origin, so other websites cannot read or
  change settings or send messages; the pages refuse to be framed. Their Content-Security-Policy
  admits only same-origin scripts and styles, plus the style elements carrying the page's
  per-load nonce. The token lives only in the app's memory and changes on every start, so quitting
  and starting the app (or installing an update) rotates it; a tab left open then reloads itself
  after an update, or asks for a reload (F5) after a plain restart.
