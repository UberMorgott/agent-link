# agent-link: guaranteed delivery (draft)

Owner requirement: a message to the other agent must ALWAYS be seen and acted on. No "peer asleep / session gone / wrong session". If no session exists, open one visibly in the user's tool (Claude Code / Codex). Sender must see real delivery state.

Repo: E:\DEV\agent-link (Go node/tray, CLI agentlink, plugins/agent-link for Claude+Codex). Routing today: chat -> project -> folder; chat sticks to last session that sent in it; hooks inject unread on SessionStart/UserPromptSubmit/PostToolUse/Stop; idle Claude woken by Stop hook `--wait` asyncRewake; idle Codex by `codex queue --thread` (internal/node/wake.go).

## Facts (verified locally)
- Claude Code 2.1.282: every session has inbox pipe `CLAUDE_CODE_MESSAGING_SOCKET` (\\.\pipe\LOCAL\cc-msg-...) + `CLAUDE_CODE_MESSAGING_TOKEN`, exported to hooks/bash. External POST `{"type":"message","text":...}` -> starts a new turn if idle. Respects session `crossSessionInbound` (accept/hold/refuse). Docs: https://code.claude.com/docs/en/cross-session-messaging.md
- Claude: no deep link for desktop app / VS Code to open session. New visible session = `wt -d <dir> claude "<prompt>"`.
- Codex 0.155.1: `codex queue` only shows in idle TUI, turn starts on user Enter (not auto). app-server v2 WS: `turn/start{threadId,input}` starts turn on loaded thread; `thread/resume`, `thread/start`. No URI scheme for app/VS Code. New visible session = `wt -d <dir> codex -C <dir> "<prompt>"`.

## Delivery ladder (node, per target member+chat)
1. Target session in active turn -> hooks inject (existing). State `delivered`.
2. Target session idle:
   - Claude: node POSTs to that session's inbox pipe (hook registers socket+token with node on every event). State `woken`. Fallback: Stop --wait asyncRewake.
   - Codex: app-server `turn/start` on the thread if TUI is attached to a daemon we can reach; else `codex queue` (needs Enter -> state `queued_needs_user`). OPEN QUESTION.
3. No live session for project (or pipe dead / refused): node launches visible session in project folder via Windows Terminal with the shell the owner configured for that project (claude|codex), initial prompt = "agent-link: N unread in chat X, read and act". State `launched`. Debounce: one launch per chat per N min; never launch for headless-only / paused messages.
4. Everything failed -> state `failed:<reason>` shown to sender (not a green double tick).

Delivery state flows back to sender node (wire: optional field on read receipt) and to UI + MCP `history`.

## Resolved with local Codex peer (cx, E:\Temp\cx\14045d33538e4a069c6e8c406ef72da8.out.md)
- VERIFIED: `codex queue --thread <id> --message <text>` auto-starts a turn in an idle Codex TUI (no Enter). Queue survives TUI close; `codex resume <id>` then processes it. Keep queue as Codex wake; no app-server daemon needed.
- Launch order when no live session: RESUME last known session of that member+project (`codex resume <id>` / `claude --resume <id>`) in new window; only if none known -> new session. Trust/update screens can block first turn -> confirm via SessionStart hook (expected folder/session), timeout, retry w/ dedupe, then `failed`.
- Status model: keep existing delivered(node stored)/read/answered; ADD separate per-message attempt events (wake_requested, woken_confirmed, launch_requested, launch_confirmed, launch_failed:<reason>, needs_human(paused)) as separate optional wire signal with cap + msg id; old peers ignore.
- Re-check eligibility (not paused/guarded, still unread) before EVERY wake/launch.
- Claude inbox token: node memory only, expiry, never sent to peers.

## Open questions (old)
- Codex idle wake without user Enter: is the interactive TUI's thread reachable via a local app-server daemon (`codex app-server daemon`?) so node can `turn/start`? How does TUI connect — embedded or daemon? Any env var exported to hooks exposing its app-server endpoint (like Claude's socket)?
- Security: inbox token stored only in node memory; pipe local only.
- Loop guard interplay: guarded/paused messages must not trigger wake or launch.
