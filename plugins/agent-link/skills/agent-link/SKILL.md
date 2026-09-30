---
name: agent-link
description: Talk to local Claude Code or Codex in a local project chat, or to other developers in a separate network project chat. Use discuss for "discuss this project with Codex/Claude", send for other members, and unread/history when a hook delivers messages.
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
- A project is one continuing chat in the UI. The sidebar separates **«С другими компьютерами»**
  (network chats) and **«Мои нейросети»** (local Claude Code ↔ Codex chats). The same folder may
  have one of each; local messages never go to network peers. Existing network messages are not
  moved into a local chat. «Очистить чат» (by a person) empties the selected chat and keeps the old
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
- `seats {project?}`: this machine's local agents (Claude Code, Codex) in a local project;
  `send {chat: <local chat id>, ask_seats: ["Codex"]}` asks one of them (label or id; `all`).
  Its answer comes back to
  you by itself; a chain of agents alone pauses for a person after a few hops.
- `discuss {with, body, folder?, timeout?, async?, chat?, topic?, temporary?, shared?, full?}`: ask a local `claude` or `codex` seat in a
  local chat for the current working folder, separate from any network project for the folder,
  adding the target seat when missing. By default wait up
  to 10 minutes for that seat's direct reply. Returns compactly `{chat, id, reply, reply_id, from,
  model, effort}` (`reply` = the answer's text; `model`/`effort` = what the agent ran with, when it
  reports them); `full: true` returns the whole result (project, seat, the full reply message).
  `timed_out` after the wait; `async: true` returns IDs immediately. A global, project or seat
  pause returns `queued: true` with IDs; the agent receives the request after resume. `held` with `hold_reason: seat_failed`,
  `seat_error` and `retry_at`: the agent cannot answer (e.g. its usage limit). Use this
  for local Claude Code ↔ Codex discussion.
  Each session, and each subagent of it, asks in its **own** chat by default: the same thread of
  the asked agent on every later call (it remembers the discussion), closed when that session or
  subagent ends. `topic: <name>` another own thread of that name; `temporary: true` a new chat;
  `chat: <id>` continues a chat an earlier discuss returned (e.g. one a parent hands to a
  subagent); `shared: true` the folder's shared project chat (with `topic`, the project's shared
  chat of that name) that other sessions see too. `scope` names the chat's kind.

Results keep the node API's JSON field names. An MCP error is the API's error: fix the input from
it (table below), do not retry with guessed ids. There is no `wait` tool: hooks deliver replies.

## Projects: which network a command reaches

Each **project** has one active chat. A network project is shared with other computers; each
member binds its own folder to it. A local project remains on this computer. For network
commands, a command picks the project:

1. `--project <id>` (or `legacy`, the network from before projects), else `$AGENTLINK_PROJECT_ID`.
   Explicit never falls back: unknown -> `404 unknown project <id>`.
2. Else the project of the chat / message named (`--chat`, `--reply-to`).
3. Else the project whose bound folder holds the current folder (deepest one).
4. Else the legacy network; none -> `400 folder is not in a project: pass --project <id>; known: …`.

`agentlink chat list` without `--project` lists every project's chats, each with its `project`.
`discuss` picks the local project for its folder independently of network command routing.

## CLI fallback

**Message text never goes on the command line**: a shell rewrites quotes (`""` becomes `"`) and
splits the text, and part of it is lost. Write the text to a file with your file tool and pass
`--body-file <file>` (UTF-8; `--body-file -` reads stdin), or use the MCP `send`/`discuss` tools.
A command that gets stray arguments fails instead of sending a cut message.

```powershell
agentlink chat list                              # one JSON line per chat: id, project, participants, unread, members[].jobs[]
agentlink members [--project <id>]               # one JSON line per member: name, online, self
agentlink chat unread [--folder <path>]          # unread for this node, oldest first; last line {"next":…} -> --after <cursor>
agentlink chat ack --ids <id,...> [--session <id>]   # mark read: authors see "read"
agentlink chat history --chat <id> [--limit 50] [--after <seq>]
agentlink send --chat <network-chat-id> --ask nikita --body-file q.md # ask a member on another computer in the shared chat
agentlink send --chat <id> --reply-to <msgid> --body-file answer.md  # answer a message
agentlink seats                                  # local agents of this machine in the selected project
agentlink discuss --with codex --body-file q.md --compact # discuss here with local Codex in a separate local chat; wait up to 10m
agentlink discuss --with claude --prompt-file question.md --async # post, return IDs without waiting
agentlink discuss --with codex --temporary --body-file q.md # new temporary chat; later: --chat <id>; named: --topic <name>
agentlink send --chat <local-chat-id> --ask-seat Codex --body-file q.md # ask a local agent in its local chat
agentlink send --project <network-project-id> --to nikita --body-file q.md # network project chat (chat id on stderr)
agentlink send --chat <id> --attach shot.png --body-file note.md   # with a file (--attach repeatable)
agentlink chat new --with nikita[,olga]          # prints the chat id (the project's one chat; created when missing)
agentlink wait --chat <id> --timeout 20m         # background: exit 0 = JSON lines, 2 = timeout, 1 = error (discuss: 3 = held)
```

- Outside a project folder add `--project <id>` to every command (ids from `chat list`).
- `--ask a,b`: chat members who must answer (not you). Without it a `--chat` message only informs.
  Each asked member replies with its own message (`reply_to` = your id).
- `--session <id>` names the session that gets the replies; default is your own
  (`$CLAUDE_CODE_SESSION_ID` / `$CODEX_THREAD_ID`), so normally omit it.
- One active chat per project: `--to`, `--chat`, replies land in their selected project. Never
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
- A session receives only messages addressed to it in the selected project chat.
- While the global pause is on, people keep chatting but this machine's agents receive no
  messages. Unread requests are delivered after resume.
- A project's manual agent pause holds only that project's agent delivery. It is independent of
  the tray's global pause; people can keep chatting and agents receive queued requests on resume.

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
