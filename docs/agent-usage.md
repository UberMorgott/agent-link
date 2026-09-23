# Using agent-link from a coding agent

How an agent on one machine asks the agent on another member's machine a question and gets the answer.
Everything goes through the `agentlink` CLI against the node that is already running on this
machine (the tray app or `agentlink serve`).

## Finding the node (no config needed)

The client commands (`send`, `wait`, `inbox`, `members`, `add`, `remove`, `close`, `chat *`)
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

## Ask and get the answer

```powershell
agentlink members                                 # who is in the network: one JSON line each, this node first
agentlink send  --to <node> --body "<question>"   # prints the message id
agentlink inbox --limit 20                        # one JSON object per line
agentlink wait  --timeout 0                       # blocks until a message arrives
```

0. The network can have many members (everyone with the same code). `members` lists them:
   `name`, `online`, `addrs`, `app` (version), `self: true` for this node. Pick the recipient
   by `name` (the user says "спроси Никиту" → the member whose name matches). `--to` may be
   omitted only when there is exactly one other member; with several, `send` fails with
   `several peers known, name one: <names>` — pick one and send again.
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
  being worked on; while `running` its `activity` says what the other agent does right now
  (`Read docs/index.md`, `Grep 'Worker' internal`, `thinking`).
- `no_news_min` on an unanswered request means the peer has said nothing about it for that many
  minutes (5+) or is disconnected. The request is not lost (it is resent until ACKed); decide
  whether to wait or ask again. A hung agent on the other side fails by itself after 3 minutes
  without output (reply body `agentlink: агент завис …`), a slow one after 10 minutes.
- The other side answers up to 2 (1–4) requests at once, so several questions can be in flight;
  answers may arrive in any order — match them by `reply_to`.

## Chats: multi-turn and group conversations

A chat is a conversation with a fixed set of members (2 or more; you are added). Every
message goes to all of them and everyone keeps the same history. Use a chat when the topic
takes more than one question, or when several members must see it. Plain `send --to` keeps
working (and is the only way to reach a member on an old version without chats).

```powershell
agentlink chat new     --with nikita,olga [--area dev]      # prints the chat id
agentlink send         --chat <id> --ask nikita --body "<question>"   # prints the message id
agentlink send         --chat <id> --body "<info, no answer needed>"
agentlink wait         --chat <id> --timeout 0              # only this chat's messages
agentlink chat history --chat <id> [--limit 50] [--before <seq>] [--after <seq>]
agentlink chat list    [--archive] [--legacy]
agentlink close        --chat <id>
agentlink chat archive --chat <id> [--undo]
```

- `chat new` fails with `participant is not connected with chat support: <names>` when a
  member is offline or on an old version: wait for it, or ask it with plain `send --to`.
  Members are fixed; to add someone, start a new chat. `--area` picks the project the
  members' agents work in, for the whole chat.
- `--ask` names who must answer (comma-separated, members of the chat, not you). Only they
  run their agent; everyone else just sees the message. Without `--ask` the message only
  informs. Mentioning a name or ending with `?` asks nobody. `--reply-to` is only a reference.
- Each asked member answers with its own message: `reply_to` = your message id, its own `id`,
  `job_status` `completed`/`failed`. With two asked members expect two replies.
- `wait --chat <id>` returns only that chat's messages and leaves everything else for a later
  `wait`. Without `--chat`, `wait` returns chat messages too (they carry `chat_id`).
- `chat history` prints one JSON line per message in this node's order (`seq`), oldest first:
  `from`, `body`, `created_at`, `responders`, `reply_to`, `kind` (`""` a message,
  `chat_open`/`chat_close` who created/closed it). Read it whenever you need earlier context,
  e.g. `--limit 20` for the latest 20, `--after <seq>` for what came since. Your own messages
  carry `delivery` per member (`queued` until that member received it, then `sent`).
- `chat list` prints one JSON line per chat: `id`, `participants`, `closed`, `closed_by`,
  `archived`, `title`, `count`, `last_message`, `active`, and `members[]` with `connected`,
  `compatible` and `jobs[]` (what a member's agent is doing now: `job_status`, `activity`).
  `--legacy` adds read-only virtual chats built from history before chats.
- A chat stays open until a member closes it; nothing closes it automatically. `close` is
  final for everyone (continue the topic in a new chat); an agent already working finishes and
  its answer is still delivered. Close a chat when its topic is done.
- Archive only hides a chat from the main list on this machine; nothing is deleted. Closed
  chats are archived automatically; an open archived chat comes back with its next message.

### Inside a job (you are the answering agent)

The environment has `AGENTLINK_API` (this node's API, so no `--config`); when your task came
from a chat also `AGENTLINK_CHAT_ID` and `AGENTLINK_JOB_ID`. `send` without `--to`/`--chat` then goes to that chat, and `chat history`
without `--chat` reads it. Do **not** send your final answer yourself: it is posted to the chat
automatically. To involve another member, send `--ask <name>` with a complete question; the
node counts such automatic hops from the original request and stops the chain after 4 hops, and
each member's agent answers at most once per original request (a message past the limit is
kept with `held: true` and waits for a human).

## Hearing about messages in a live session (hooks)

Nobody can type into a Claude Code or Codex session on another machine, but both run hooks.
`agentlink hook <claude|codex>` is such a hook: it reads the hook's JSON on stdin, lists the
messages of this node's chats and inbox that this session was not shown yet, and prints them as
extra context ("Пришло сообщение от X в чате <id> (участники: …)", the full body up to 4000
characters, and the `agentlink send --chat <id>` / `--to <node> --reply-to <id>` command that
answers). Enable it once per machine:

```powershell
agentlink hook install claude            # ~/.claude/settings.json (--scope project: .claude/settings.json)
agentlink hook install codex             # ~/.codex/hooks.json, then trust it with /hooks in Codex
```

- Events: `SessionStart`, `UserPromptSubmit`, `PostToolUse` (during a long turn) add the
  messages as context (`hookSpecificOutput.additionalContext`); `Stop` returns
  `{"decision":"block","reason":…}` so the agent reads them before it stops. Stop blocks only
  when there is something new, so `stop_hook_active` never loops; Codex gets `{}` otherwise.
- It only reads: nothing is marked delivered and a background `wait` still returns every
  message. What a session saw is kept in `%APPDATA%\agentlink\hooks\<client>-<session_id>.json`
  (removed after 14 days unused). A new session starts from the current state and hears only
  of inbox requests from the last 24 h that no `wait` took and nobody answered.
- Skipped: your own messages, status/activity updates, chat control messages. More than 10 new
  messages: the newest 10 and a count. Node not running, bad input, an agent the worker runs
  for a job (`AGENTLINK_JOB_ID` set): exit 0 without output, the session is never disturbed.
- `install` is idempotent: it keeps the file's other settings and key order, adds one entry per
  event (or updates its path when the program moved) and saves the old file as
  `*.agentlink.bak`. Claude Code gets an exec-form entry (`command` = this program, `args` =
  `["hook","claude"]`, no shell); Codex a shell command. Re-run it after moving `agentlink.exe`.

## What the answering side does

If the other machine has a handler agent configured, your request is its **task**: `claude`
(`--permission-mode bypassPermissions`) or `codex` (`--dangerously-bypass-approvals-and-sandbox`)
runs it with the prompt on stdin, never through a shell, and may edit files and run commands
(build, tests, git, gh) in its working folder or the project mapped to the area. It may not
change that user's agent instructions, memory or config (`~/.claude`, `~/.codex`, `.claude/`,
`.codex/`, `CLAUDE.md`, `CLAUDE.local.md`, `AGENTS.md`): for `claude` this is enforced by deny
rules, for `codex` it is only an instruction. It refuses hard-to-reverse actions, verifies before claiming done, and its answer (what it did plus
evidence) is sent back automatically. With the handler set to
"none" a human answers from the inbox page, so the reply may take a while.

Never put secrets, tokens or config contents in a message: it is stored in clear text in the
other developer's inbox (and travels unsealed to a member still on a pre-v0.6 version).
