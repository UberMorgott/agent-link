# agent-link -> plugins (A=Claude me, B=KPECTIK Codex)
Chat: project CYYHUMIPOVN4TQP6RTJ4JJV6FQ, chat 07671ed1252861786ae2e1236fc64073. CLI: E:\DEV\agent-link\dist\agentlink.exe

## Agreed contract (msgs 6edef865, 51c03782)
- stdio `agentlink mcp`, server name `agentlink`, proxies loopback API via CLI client code; no second daemon; no wait tool v1; hooks stay push/wake; CLI stays.
- session: Claude CLAUDE_CODE_SESSION_ID; Codex CODEX_THREAD_ID then CODEX_SESSION_ID.
- tools: projects{}; members{project?}; chats{project?,archive?,legacy?}; history{chat,limit?=50,before_seq?,after_seq?}; unread{folder?,project?,limit?=50,after?} (no ack); send{exactly one of chat|to|new_chat_with, body, ask?[], reply_to?}->{id,chat}; ack{ids[],chat?,project?,session?}
- output = CLI JSON verbatim; errors isError + API msg verbatim; parity tests vs CLI.
- no bundled exe; tray writes abs exe path marker under %APPDATA%\agentlink; launcher order AGENTLINK_EXE, marker, PATH, error.

## Steps
- [x] PR1 #2 merged e6d8d8e
- [~] PR2 #3 head fd065c1 awaiting B rerun; open: Codex MCP gets no PLUGIN_ROOT -> propose marker fallback in .mcp.json
- [~] Owner bugs: A feat/agent-tree (tree panel + Claude Subagent hooks, agent running); B fix/agent-loop-status ("!" pause + Codex subagent hooks)
- [x] #3 #5 #4 merged, v0.6.15; #6 (plugin coexist, counts) merged 625bc2b, v0.6.16 (released after renaming locked dist exe -> dist\agentlink.running-0.6.15.exe; delete after tray restart)
- [x] Claude plugin installed locally; folder hooks removed (backups in scratchpad)
- [ ] B: tray restart, Codex plugin PR #1 rebase, Codex PluginEnabled detection, live e2e — B session in agent-link folder absent (only CodeDungeon session alive)
- [ ] idea (asked owner): delivery when project has no live session -> wake same agent elsewhere or show "no recipient"
- [~] #8 guaranteed delivery + #9 one chat/project: review+merge+v0.6.17 (agent running). Main channel now chat bed7c121a435a25e6bb127eaa2fded45
- [x] v0.6.18 (#12 direct wake, #10 project menu, #11 attachments), v0.6.19 (#13 native launch: auto_open default OFF, active>idle>new, full perms, persistent spent/ack)
- [~] Codex desktop via shared app-server (design scratchpad\codex-shared-server-design.md; live-verified CODEX_APP_SERVER_WS_URL works); cx consulting on CODEX_APP_TOOLS_PIPE_PATH
- [ ] stray untracked E:\DEV\agent-link\inbox.ts (root) — check/delete; stash@{0} pre-v0.6.18 WIP awaits owner
- [x] local seats #14 -> v0.6.20 (ed27d0d)
- [ ] waiting: Aegis CODEX_CLI_PATH proxy result -> decide Codex native path (proxy vs WS_URL); KPECTIK back -> update to 0.6.20 + live e2e
- note: owner tray still 0.6.17 (auto-update 6h)
- NIGHT MODE: KPECTIK machine off; reviewer = owner's local Codex via `cx` (review focused diffs, may need background run). A owns everything incl. seats.
- running: feat/project-menu (wt5), feat/attachments (wt6), fix/direct-content-wake (wt7). Merge order: direct-content-wake -> project-menu -> attachments (rebase) -> release; then seats.
- cleanup: stale worktrees wt2/wt3/wt4
- [~] (was B) direct-content wake (queue/inbox carry real message, no WakeText, no hook re-inject) branch fix/codex-direct-agentlink-message
- Owner backlog 21:40-21:42 (chat bed7c121):
  - [ ] UI: remove chat header (title/presence/buttons); project sidebar "..." menu: Archive history, Manage participants (add/remove, confirm), Delete project (confirm)
  - [ ] attachments: paste/drag/picker images+files supported by Claude+Codex; stored as attachments (name,MIME,size,hash); agent delivery materializes to safe path
  - [ ] agents can post files/images as attachments (same API, allowed paths only)
  - [ ] local seats: add Claude Code + Codex to same project conversation; local agent-to-agent with loop limit; add/remove/start/stop controls
- [ ] flaky TestPresencePropagates (presence_test.go:47)
- note: Codex draft #1 60d7d8d; follow-up: app folder hooks double with plugin; plugin hooks Windows-only (cmd)
- [ ] PR2 A (orig): Claude plugin (.claude-plugin, hooks, skill, .mcp.json, marketplace, exe marker+launcher)
- [ ] PR3 B: Codex plugin
- [ ] release gate together
