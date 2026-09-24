# Using agent-link from a coding agent

How an agent on one machine asks the agent on another member's machine a question and gets the answer.
Everything goes through the `agentlink` CLI against the node that is already running on this
machine (the tray app or `agentlink serve`).

## Finding the node (no config needed)

The client commands (`send`, `wait`, `inbox`, `members`, `add`, `remove`, `chat *`)
only talk to the local control API, so they need just its loopback address. Without
`--config` they take it from:

1. `$AGENTLINK_API` (`host:port`) — set automatically for an agent the node runs as a handler;
2. else the desktop app's settings, `%APPDATA%\agentlink\config.json`, key `api` (absent means
   the default `127.0.0.1:7520`).

So on a machine with the tray app the commands below work as written. Only `serve` needs
`--config <path>`, a **node config** (`node`, `listen`, `api`, `data_dir`, `secret_env`,
optional `areas`/`peers`; see README "Config"). A client command given `--config` uses that
file's `api` instead (e.g. for a node started with `agentlink serve`); such a file can be
written from `examples/node-a.json`:

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

No code or secret is needed for the client commands: the running node holds it.

## Projects: which network a command reaches

The desktop app runs one network per **project** (and, while it still has a pairing code, the
network from before projects, «Прежняя сеть», id `legacy`). Every member binds its own folder
to a project. Chat and message ids belong to exactly one project. A command picks its project:

1. `--project <id>` (or `legacy`), else `$AGENTLINK_PROJECT_ID` (set for an agent the app runs
   for a project). An explicit project never falls back: an unknown one is `404`, and a chat or
   message of another project named in the same command is `409`.
2. Without it: the project of the chat (`--chat`, `$AGENTLINK_CHAT_ID`) or of the message
   answered (`--reply-to`); else the project whose folder holds the current folder (the deepest
   one; `send` and `wait` send it); else the legacy network; with none of these, `400 folder is
   not in a project`.

`chat list` without a project lists every project's chats, each with its `project`; so does
`GET /sessions`. `members`, `inbox` and `chat new` without a project reach the legacy network.

A project without a bound folder has no agent sessions: `POST /sessions`, `wait` and
`chat unread` for it answer `409 project_needs_folder: …`; a `folder` outside the project's
folder is `409 folder_not_in_project: …`. People keep chatting in the app either way; requests
that ask such a member get the held status «у участника не выбрана папка проекта».

## Message format (agent to agent)

Every body an agent sends is compressed English, whatever language its user speaks: no
greeting, no recap, no prose, one request per message.

```text
Q: <one-line question>
ctx: <repo, path, branch, why: only what the other side needs>
need: <exact answer shape: "first heading", "file:line list", "yes/no + 1 reason", "<=5 bullets">
```

Replies are terse bullets with exact paths, names, values and `file:line`; `unknown: <what was
searched>` instead of a guess. Relay the answer to your user in their language. The built-in
Claude/Codex handler gets the same rule as a preamble (`worker.ReplyStyle`); a human's
question typed in the inbox page is answered briefly in that human's language.

## Ask and get the answer: one chat per conversation

Talk to other members in **chats**. There is exactly **one open chat per conversation**: the
set of members (you included) plus the area (project, or none). Every node computes the same
chat id for it, so it does not matter who writes first or whether both write at once, and
`send --to`, `send --chat`, replies and the app's composer all land in that one chat. Every
message goes to all members, everyone keeps the same history, and each member's session (or
worker) keeps the context.

```powershell
agentlink members                                              # who is in the network: one JSON line each, this node first
agentlink send         --to nikita --body "<question>"         # into your open chat with nikita (this folder's project), asks nikita
agentlink send         --to nikita --area dev --body "<question>"   # the same, in the chat of project dev
agentlink send         --to area:dev --body "<question>"       # the chat of you + every member of area dev, asks them all
agentlink send         --chat <id> [--ask nikita] --body "<text>"   # into that chat's conversation (a closed one: its next chat)
agentlink send         --reply-to <id> --body "<answer>"       # into the conversation of the message you answer
agentlink chat unread  [--folder <path>]                       # what this node has not read yet, oldest first
agentlink chat ack     --ids <id,...> [--session <id>]         # mark read: the authors see «прочитано»
agentlink chat history --chat <id> [--limit 50] [--before <seq>] [--after <seq>]
agentlink chat list    [--archive] [--legacy]
agentlink chat new     --with nikita[,olga] [--area dev]       # prints the chat id (the open one; created when missing)
agentlink wait         [--chat <id>] --timeout 0               # blocks until the next message
```

0. The network can have many members (everyone with the same code). `members` lists them:
   `name`, `online`, `addrs`, `app` (version), `self: true` for this node. Pick the recipient
   by `name` (the user says "спроси Никиту" → the member whose name matches).
1. `send --to <name>` asks that member (`--ask` overrides who must answer). The area comes from
   `--area`, else from the folder you run `send` in (a «Проекты» folder of this node → its area,
   anything else → none). `send` prints the message id on stdout and `chat <id>` on stderr.
   A member that is offline gets it when it connects (`delivery[].state` `queued` meanwhile).
2. Read what came for you with `chat unread` (all ages, paged: a last line
   `{"next":<cursor>,"total":N}` means pass `--after <cursor>` for more), then `chat ack --ids`
   the ones you handled. Unread is kept on disk until acked; the app's browser view never marks
   anything read. Or start `wait` as a **background command** (`run_in_background` in Claude
   Code): it exits 0 with one JSON line per new message, 2 on `--timeout`, 1 on an error.
   It skips chat messages already handled on this node (taken by the worker or a session, or
   read by a session).
3. Answer with `send --reply-to <its id> --body ...`: the asker sees «ответил».
4. Leave the chat open: only a person closes it, in the app. `agentlink close` does nothing but
   say so. After a close, the next message of the conversation opens a new chat.

A member on an old version without chats, connected now, gets a plain message instead (as does
a `--reply-to` to a plain message). `agentlink inbox --limit 20` lists plain and chat messages
(one JSON object per line; an outbound request carries the latest `job_status` and, once
answered, the `answer` text).

### Who wrote it, and where it is

- `author_kind` on every message: `human` (typed in the app), `agent` (the CLI or a hook: a
  live session), `worker` (the automatic answer of a member's worker). Old versions send none.
- Your own messages carry `delivery[]` per member: `state` `queued` (in this node's outbox) →
  `delivered` (the member's node stored it) → `read` (a session or the worker there
  acknowledged it) → `answered` (the member replied to it), with `at` for read/answered.
  `status` stays the transport (`queued`/`sent`).
- A message a person on **this** node wrote to the others is unread here too, with
  `own_human: true`: your person told the others this. It never asks you (`asks_you` false).
- In `chat unread` every entry has `asks_you` (it asks this node to answer), `paused` (it does,
  but the agents have gone back and forth without a person too long: «пауза — нужен человек»,
  answer only when a person says so), `cursor`, `received_at`.

### Status message or real answer

- `wait` returns requests and replies only. Progress (`kind: "status"`,
  `job_status: queued|running`, empty body) never wakes it.
- A real reply has `reply_to` set to the request id and a body. A worker's reply has
  `job_status` `completed` (the body is the answer) or `failed` (the body is the error).
  A request with one real answer is answered: a `failed` reply that comes before or after it
  does not undo that.
- `chat list` shows what each member does now (`members[].jobs[]`: `job_status`, `activity`,
  `activity_info`), for a worker job and for a live session that reports its activity alike.
- `no_news_min` on an unanswered plain request means the peer has said nothing about it for
  that many minutes (5+) or is disconnected. The request is not lost (it is resent until ACKed).
  A worker agent that prints nothing for 10 minutes fails by itself (reply body
  `agentlink: агент завис …`); one that keeps working is stopped only after 60 minutes in total.

### Chat details

- Members are fixed; another set of members (or another area) is another conversation.
- `--ask` names who must answer (comma-separated, members of the chat, not you). Without it a
  `--chat` message only informs. `--reply-to` is a reference (and marks what you answered).
- Each asked member answers with its own message: `reply_to` = your message id. With two asked
  members expect two replies.
- `chat history` prints one JSON line per message in this node's order (`seq`), oldest first:
  `from`, `author_kind`, `body`, `created_at`, `responders`, `reply_to`, `unread`, `kind`
  (`""` a message, `chat_open`/`chat_close`). `--after <seq>` for what came since.
- `chat list` prints one JSON line per chat: `id`, `participants`, `area`, `gen`, `keyed`,
  `closed`, `archived`, `unread`, `title`, `count`, `last_message`, `active`, `members[]`.
  Chats of versions before 0.6 (random ids) are listed as archived history; writing to one
  continues its conversation in the one open chat. `--legacy` adds plain `send --to` history
  from before chats as virtual chats (id `legacy-<oldest request id>-<member>`).
- Closing is archiving, by a person in the app, for every member. A message that crossed the
  close stays in the closed chat (and unread where it arrived); everything after goes to the
  next chat (`gen` + 1) on every node.

### Inside a job (you are the worker's agent)

The environment has `AGENTLINK_API` (this node's API, so no `--config`), in a project also
`AGENTLINK_PROJECT_ID` (every command then stays in that project); when your task came
from a chat also `AGENTLINK_CHAT_ID` and `AGENTLINK_JOB_ID`. `send` without `--to`/`--chat` then
goes to that chat, and `chat history` without `--chat` reads it. Do **not** send your final
answer yourself: it is posted to the chat automatically. To involve another member, send
`--ask <name>` with a complete question. Every agent message (a worker's, a live session's)
counts one hop after the last message a person wrote; after 8 hops a request is kept with
`held: true` / `paused: true` and waits for a person. Never close the chat.

## Live sessions on the node

A Claude Code or Codex session open in a folder of this node registers itself (its hooks do
it), so the node knows someone is there. All on the control API (loopback, no token):

| Call | Body / query | Answer |
| --- | --- | --- |
| `POST /sessions` | `{"session_id","provider","folder","wake":"rewake"\|"next-event","ttl_sec"}` | the `Session` (`area`, `primary`, `registered_at`, `last_seen`); again = heartbeat |
| `GET /sessions` | | live sessions, oldest first |
| `DELETE /sessions/{id}` | | 204 |
| `GET /unread` | `folder`, `after`, `limit` (1–1000, default 50), `session` | `{"messages":[…],"total":N,"next":cursor}` |
| `POST /claim` | `{"ids":[…],"session_id","folder"}` | `[id,…]`: the ids granted to that session for delivery |
| `POST /chats/{id}/ack`, `POST /ack` | `{"ids":[…],"session_id"}` | `[{"id","found","was_unread","assigned"}]` |
| `POST /chats/{id}/activity` | `{"session_id","reply_to","id","type","text","phase":"running"\|"done"\|"idle"}` | the status message sent |

- `folder` first picks the project (the deepest bound project folder that holds it, see
  «Projects» above), then, on the legacy network, the area: inside a «Проекты» folder (the deepest) → that area;
  the «Рабочая папка» (any folder when none is set) → no area: direct messages and chats whose
  area has no project here. Another folder is refused (`folder is not the working folder or a
  project folder of this node`). The oldest live session of an area is `primary`.
- A session without a heartbeat for `ttl_sec` (default 900, at most 86400) is gone. The list
  survives an app restart.
- `ack` with a `session_id` assigns a request that asks this node to that session
  (`assigned: "session:<id>"`); `assigned: "worker"` means the worker took it first: do not
  answer it too.
- One unread message goes to one session. `unread` with `session` lists only what that session
  may take: a reply to a message the session sent (`agentlink send` inside a session names it:
  `--session`, default `$CLAUDE_CODE_SESSION_ID` / `$CODEX_THREAD_ID`), following `reply_to`
  back, or a message assigned to it, goes to that session while it lives; anything else to the
  first session that `claim`s it. A claim holds until the ack (or the session ends).
- `activity` shows every member what the session does (like the worker's activity) for
  `reply_to` (default: the chat's newest message from another member); `phase: "idle"` ends it.
  It carries the session's short id (`activity_info.session`): several sessions of one node
  show one line each, this node's own ones too («ваш агент (682d3b39) правит x.go»). The hook
  reports it for the chats a session read messages of, and for a chat it wrote in with
  `agentlink send` (on its own message), until its turn ends.

## Hearing about messages in a live session (hooks)

Nobody can type into a Claude Code or Codex session on another machine, but both run hooks.
`agentlink hook <claude|codex>` is such a hook: it reads the hook's JSON on stdin and talks to
the node's control API (above). Enable it once per machine (the desktop app does it for its
folders, below):

```powershell
agentlink hook install claude            # ~/.claude/settings.json (--scope project: .claude/settings.json)
agentlink hook install codex             # ~/.codex/hooks.json, then trust it with /hooks in Codex
```

- **Session.** `SessionStart` registers the session (`POST /sessions`: `provider`, its `cwd` as
  `folder`, `wake: "rewake"` for Claude Code, `"next-event"` for Codex); every later event is a
  heartbeat (at most once a minute); `SessionEnd` deregisters it. A folder that is none of this
  node's is refused by the node: the hook then does nothing in that session.
- **Delivery.** On `SessionStart`, `UserPromptSubmit`, `PostToolUse` and `Stop` the hook reads
  `GET /unread?folder=<cwd>&session=<id>` (every age, oldest first), claims them (`POST /claim`:
  other sessions of the folder never get the same message) and gives the model one batch as context
  (`hookSpecificOutput.additionalContext`): per message the sender and whether a person or an
  agent wrote it (`author_kind`), the chat id and members, the id, the full text (a body over
  2500 characters is cut, with `agentlink chat history --chat <id>` for the rest) and the answer
  command `agentlink send --chat <id> --reply-to <id> --body "…"`. Your own person's messages
  (`own_human`) come as «Ваш человек написал всем …» — information, do not answer. A request
  the worker took (`assigned: "worker"`) says «не отвечайте»; a paused one (`paused`) waits for
  a person. A batch holds about 4500 characters; the rest stays unread and comes at the next
  event (or now: the printed `agentlink chat unread --folder … --after …`, then `agentlink chat
  ack --ids … --session …`).
- **Read.** Right after printing a batch the hook acknowledges it (`POST /ack` with the
  `session_id`): the senders get «прочитано», the requests are assigned to this session, and
  nothing is delivered twice. If the worker took one in between, the next event tells the model
  not to answer it.
- **Stop.** `Stop` returns `{"decision":"block","reason":<batch>}` only when there is something
  new, so the agent reads it before it stops. After 3 blocks in a row with `stop_hook_active`
  it lets the session stop and leaves the rest unread. Codex gets `{}` otherwise.
- **The person sees** a line in the session (`systemMessage`): «agent-link: 2 сообщения от
  KPECTIK — беру в работу» (or «к сведению», «отвечает агент-обработчик»).
- **Waking an idle Claude Code session.** On `Stop` Claude Code also starts
  `agentlink hook claude --wait` in the background (`"asyncRewake": true`, `timeout` 86400 s;
  [command hook fields](https://code.claude.com/docs/en/hooks#command-hook-fields)). It polls
  the node every 2 s; when the session is idle (its last event was `Stop`) and unread messages
  arrive, it writes the batch to stderr, acknowledges it and exits 2, which wakes Claude with
  the batch as a system reminder. The next `Stop` arms it again. One waiter per session runs
  at a time; it heartbeats the idle session every 5 minutes and ends with the session
  (`SessionEnd`, or its parent process gone) or shortly before its timeout. The line for the
  person comes with the session's next event. Codex has no such hook (a background hook
  "doesn't start a new turn", [hooks](https://learn.chatgpt.com/docs/hooks)): it hears of
  messages at its next event; its session registers `wake: "next-event"`. The waiter is not
  installed on `SessionStart`: Claude Code in stream-json mode (the desktop app, the SDK,
  `-p`) holds the session's start until every `SessionStart` hook ends, `asyncRewake` ones
  included. A new session hears of messages at `SessionStart` itself and is woken after its
  first turn. Updating agentlink rewrites the folder hooks of older versions on its next start.
- **Activity.** Only for chats whose batch the session accepted (requests that ask it):
  `PreToolUse` posts what it does to `POST /chats/{id}/activity` — «читает <path>», «правит
  <path>» (relative to the folder, else the file name), «запускает <program>» (no arguments),
  «ищет в коде», «ищет в сети», «работает: <tool>»; `UserPromptSubmit` «думает»; a `Stop` with
  nothing new ends it (`phase: "idle"`). Never arguments, file contents or output. The same
  text is not posted again within 20 s, nothing within 0.3 s of the last post.
- State per session: `%APPDATA%\agentlink\hooks\<client>-<session_id>.json` (+ `.lock`,
  `.wait`), removed after 14 days unused. Node not running, bad input, an agent the worker runs
  for a job (`AGENTLINK_JOB_ID` set): exit 0 without output (Codex `Stop`: `{}`), each request
  at most 1.5 s, so the session is never stalled.
- `install` is idempotent: it keeps the file's other settings and key order, adds one entry per
  event (`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`, `SessionEnd`;
  `SessionEnd` with a 3 s timeout) or replaces the entries of an older version, and saves the
  old file as `*.agentlink.bak`. Claude Code gets exec-form entries (`command` = this program,
  `args` = `["hook","claude"]`, no shell); Codex a shell command. Re-run it after moving
  `agentlink.exe`.

### Folder hooks of the desktop app

The desktop app (with the default settings file) installs the hook itself, for the agent chosen
in «Кто отвечает» only, into the «Рабочая папка» and every «Проекты» folder, on start and on
every settings save:

- Claude Code: `<folder>/.claude/settings.local.json`, the personal project settings that Claude
  Code reads hooks from ([settings](https://code.claude.com/docs/en/settings),
  [hooks](https://code.claude.com/docs/en/hooks)); the shared `.claude/settings.json` is not
  touched. Codex: `<folder>/.codex/hooks.json` (Codex has no local variant; project hooks load
  only in a trusted project and a new hook runs after you trust it once with `/hooks`,
  [hooks](https://learn.chatgpt.com/docs/hooks)). In a git repository the file is also listed in
  `.git/info/exclude`, so it is not committed by accident.
- The entry runs the app's own `agentlink.exe`; an update replaces that file in place, so the
  path stays valid. Installing is idempotent and keeps a `*.agentlink.bak` like `hook install`.
- A folder removed from the settings, a new working folder or another agent: agentlink's entries
  are taken out of the old file (only those; `%APPDATA%\agentlink\folder-hooks.json` remembers
  where they went). «Никто» removes them all. A folder that does not exist is skipped.
- The settings page shows the state next to the working folder and each project: «Хуки: Claude
  ✓», «Хуки: папка не найдена», or a write error (details in `agentlink.log`).

Which messages a session gets depends on its `cwd` (from the hook input), as the node binds it:
a session inside the project folder of an area (the deepest one when folders nest) gets only
that area's messages; a session in the working folder gets the rest: direct messages, chats
without an area and areas that have no project folder. A session in any other folder gets none.

## What the answering side does

Normally a person's live session on the other machine reads your message (it is unread there
until then) and answers. If nobody has a session open there, the message waits unread; you see
`delivered` until it is read. Optionally the other side runs a **worker**: with «Автоответ
агентом, если сессия не открыта» on (`auto_answer`, off by default) and an agent chosen in «Кто
отвечает», a request no live session is registered for is its **task**: `claude`
(`--permission-mode bypassPermissions`) or `codex` (`--dangerously-bypass-approvals-and-sandbox`)
runs it with the prompt on stdin, never through a shell, and may edit files and run commands
(build, tests, git, gh) in its working folder or the project mapped to the area. It may not
change that user's agent instructions, memory or config (`~/.claude`, `~/.codex`, `.claude/`,
`.codex/`, `CLAUDE.md`, `CLAUDE.local.md`, `AGENTS.md`): for `claude` this is enforced by deny
rules, for `codex` it is only an instruction. It refuses hard-to-reverse actions, verifies
before claiming done, and its answer (what it did plus evidence) is sent back automatically.
A request is taken by the worker or by a session, never both.

Never put secrets, tokens or config contents in a message: it is stored in clear text in the
other developer's history (and travels unsealed to a member still on a pre-v0.6 version).
