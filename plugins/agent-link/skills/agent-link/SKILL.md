---
name: agent-link
description: Talk to other developers' agents (peers on other machines) over agent-link - send or ask in a project chat, read unread messages, wait for a reply, list chats, projects and members. Use on "ask the other agent", "ask <name>", "message the other machine", "check agent-link / incoming messages", "reply in the chat", "спроси Никиту", "спроси другого агента", "есть ли входящие", or when a hook delivered agent-link messages to this session.
---

# agent-link

agent-link connects the coding agents of several developers' machines. Each machine runs one
node (the `agentlink.exe` desktop/tray app, or `agentlink serve`). You never talk to peers
directly: the `agentlink` MCP tools (or the `agentlink` CLI) talk to the local node's loopback
API (`$AGENTLINK_API`, else the app's settings, default `127.0.0.1:7520`). No `--config`, code
or secret is needed. Full reference: `docs/agent-usage.md` in the agent-link repository.

Prefer the MCP tools of the `agentlink` server (the agent-link plugin, or `agentlink mcp`
registered by hand). Use the CLI only when the tools are missing: `agentlink` on `PATH` (the
plugin puts its launcher there for the Bash tool), else the `agentlink.exe` the hooks run.
`agentlink version` checks it. A connection error means the app is not running: ask the user
to start it.

## MCP tools

- `projects {}`, `members {project?}`: project networks and people (`self` = this node).
- `chats {project?, archive?, legacy?}`: chats, most recent first (without `project`: all).
- `history {chat, limit?, before_seq?, after_seq?}`: messages of a chat; reads, changes nothing.
- A project has one chat. «Очистить чат» (by a person) empties it for everyone and keeps the old
  messages as a dated snapshot: `chats {project, archive: true}` lists the snapshots (`closed_at` =
  when cleared), `history {chat: <snapshot id>}` reads one. Unread requests from before a clear
  stay in `unread`; answering them lands in the current chat.
- Members go by `name` (identity) and may show a nickname (`display`): `to`, `ask`, `new_chat_with`
  accept either, or an earlier nickname.
- `unread {project?, folder?, limit?, after?}`: a page of unread messages, not acked; `next` is
  the cursor for `after`. Pass the ids you handled to `ack`.
- `send {body, chat | to | new_chat_with, ask?, reply_to?, attachments?}`: exactly one of `chat`,
  `to`, `new_chat_with`; `ask` = members who must answer; `reply_to` = the message answered;
  `attachments` = absolute paths (images png/jpeg/gif/webp, pdf, text; <=10 MB each, <=10 files)
  inside the project folder or the temp folder. Returns `{id, chat}`. Received files are listed by
  absolute path (`<project>/.agentlink/attachments/...`): open images/PDFs with your file viewer,
  text by reading the file.
- `ack {ids, chat?, project?, session?}`: mark read; `session` defaults to this session.
- `seats {project?}`: this machine's local agents (Claude Code, Codex) in the project's conversation;
  `send {chat, ask_seats: ["Codex"]}` asks one of them (label or id; `all`). Its answer comes back to
  you by itself; a chain of agents alone pauses for a person after a few hops.

Results keep the node API's JSON field names. An MCP error is the API's error: fix the input from
it (table below), do not retry with guessed ids. There is no `wait` tool: hooks deliver replies.

## Projects: which network a command reaches

Each **project** is its own network (members, chats). Every member binds its own folder to it.
A command picks the project:

1. `--project <id>` (or `legacy`, the network from before projects), else `$AGENTLINK_PROJECT_ID`.
   Explicit never falls back: unknown -> `404 unknown project <id>`.
2. Else the project of the chat / message named (`--chat`, `--reply-to`).
3. Else the project whose bound folder holds the current folder (deepest one).
4. Else the legacy network; none -> `400 folder is not in a project: pass --project <id>; known: …`.

`agentlink chat list` without `--project` lists every project's chats, each with its `project`.

## CLI fallback

```powershell
agentlink chat list                              # one JSON line per chat: id, project, participants, unread, members[].jobs[]
agentlink members [--project <id>]               # one JSON line per member: name, online, self
agentlink chat unread [--folder <path>]          # unread for this node, oldest first; last line {"next":…} -> --after <cursor>
agentlink chat ack --ids <id,...> [--session <id>]   # mark read: authors see "read"
agentlink chat history --chat <id> [--limit 50] [--after <seq>]
agentlink send --chat <id> --ask nikita --body "<question>"      # ask in an existing chat; prints message id
agentlink send --chat <id> --reply-to <msgid> --body "<answer>"  # answer a message
agentlink seats                                  # local agents of this machine in the project
agentlink send --chat <id> --ask-seat Codex --body "<question>"   # ask a local agent (repeatable, or all)
agentlink send --to nikita --body "<question>"   # the one open chat with nikita in this folder's project (chat id on stderr)
agentlink send --chat <id> --attach shot.png --body "<text>"   # with a file (--attach repeatable)
agentlink chat new --with nikita[,olga]          # prints the chat id (the project's one chat; created when missing)
agentlink wait --chat <id> --timeout 20m         # background: exit 0 = JSON lines, 2 = timeout, 1 = error
```

- Outside a project folder add `--project <id>` to every command (ids from `chat list`).
- `--ask a,b`: chat members who must answer (not you). Without it a `--chat` message only informs.
  Each asked member replies with its own message (`reply_to` = your id).
- `--session <id>` names the session that gets the replies; default is your own
  (`$CLAUDE_CODE_SESSION_ID` / `$CODEX_THREAD_ID`), so normally omit it.
- One open chat per set of members + project: `--to`, `--chat`, replies all land in it. Never
  close chats (`agentlink close` refuses; only a person closes one in the app).

## Delivery: hooks, not polling

With the hooks active (the agent-link plugin, `agentlink hook install claude|codex`, or the app
installs them in its folders; only one of these per agent) you usually do nothing to receive:

- On SessionStart, UserPromptSubmit, PostToolUse and Stop the hook injects unread messages for
  your folder (sender, human/agent, chat id, full text, ready reply command) and acks them.
- Idle Claude Code: the Stop hook arms a background waiter (`asyncRewake`) that wakes the session
  when a message arrives. Idle Codex: the node runs `codex queue --thread <id>` (codex >= 0.149)
  to start a turn; otherwise it hears at its next event.
- Headless runs (`claude -p`, `codex exec`) and worker jobs (`$AGENTLINK_JOB_ID`) take no messages.
- Without hooks: run `agentlink wait` as a background command and re-run it after each batch.

## Routing rules

- One message goes to one session. A chat sticks to the live session that last sent in it
  (`agentlink send` inside a session records it), so replies come back to the asking session.
  A session that never took part in a chat does not get its messages.
- Act only on what asks you (`asks_you: true`; the hook writes «Просит ответа от вас»). Your own person's
  messages to others (`own_human`) are information. `assigned: "worker"` -> the worker answers,
  do not. `paused: true` -> the automatic chain limit was reached; read it as information.
- A session gets messages only for its folder's project (the project's bound folder).

## Message format (agent to agent)

Compressed English whatever the user's language; one request per message; no greeting/recap:

```text
Q: <one-line question>
ctx: <repo, path, branch, why: only what the other side needs>
need: <answer shape: "file:line list", "yes/no + 1 reason", "<=5 bullets">
```

Replies: terse bullets, exact paths/values; `unknown: <what was searched>` instead of guessing.
Relay answers to your user in their language. Never send secrets, tokens or config contents:
messages are stored in clear text on the other machine.

## Errors

| Error | Fix |
| --- | --- |
| `400 folder is not in a project: pass --project <id>; known: …` | add `--project <id>` from the list (or `chat list`) |
| `404 unknown project <id>` | wrong id: `agentlink chat list` shows valid `project` values |
| `409 the chat or message is not in that project` | drop `--project` or use the chat's project |
| `409 project_needs_folder` / `folder_not_in_project` | the project has no bound folder here / run inside it (a person sets it in the app) |
| `several peers known, name one` | pass `--to <name>` or `--chat <id>` (`members` lists names) |
| connection refused | the agentlink app is not running on this machine |
