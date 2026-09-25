# agent-link: Codex desktop on a shared app-server (draft)

Verified live 2026-09-26 (logs scratchpad\native-codex\live):
- Codex desktop = C:\Program Files\WindowsApps\OpenAI.Codex_26.917.9434.0_x64__2p2nqsd0c76g0\app\ChatGPT.exe. Normally spawns own `%LOCALAPPDATA%\OpenAI\Codex\bin\<hash>\codex.exe -c features.code_mode_host=true app-server --analytics-default-enabled -c plugins.codex-app-tools@openai-bundled.mcp_servers.codex_app.enabled=true` over stdio.
- Env CODEX_APP_SERVER_WS_URL=ws://127.0.0.1:PORT (unless CODEX_APP_SERVER_FORCE_CLI=1) -> desktop uses external ws server, spawns none. Loopback needs no auth. Env must reach ChatGPT.exe directly (start exe from a process with env set; shell:AppsFolder launch drops it).
- Our threads (thread/start cwd + dynamicTools) appear live in desktop Recent. Custom item/tool/call not raced by desktop. thread/queue/add works idle+busy (auto next turn). thread/status/changed for all threads (notLoaded/idle/active) = presence.

## Proposal (opt-in per machine: "Codex desktop via agent-link", default OFF)
- Node supervises ONE shared app-server: desktop's own bundled codex.exe (exact version match), same flags + `--listen ws://127.0.0.1:<port>`; restart on crash; logs.
- agent-link offers "Open Codex" (tray menu + optional replace of Start-menu launch? no — just our launcher) that starts ChatGPT.exe with CODEX_APP_SERVER_WS_URL.
- Node is a ws client of that server: thread/list + thread/status/changed -> Codex presence per folder (replaces hook TTL / mtime heuristic for Codex); delivery: thread/queue/add {threadId, input:[real formatted message + attachments]} for idle AND busy threads (replaces codex queue CLI); new session: thread/start {cwd, approvalPolicy, sandbox} + queue/add -> appears in desktop.
- dynamicTools namespace `agentlink` on threads we create (send/reply/unread without MCP) — optional phase 2.
- Fallbacks: option off or server down -> current path (hooks + codex queue + app-server stdio launch).

## Open issues
- CODEX_APP_TOOLS_PIPE_PATH: desktop provides it to its own child; with external server codex_app MCP fails -> desktop app-tools lost. Need to find how desktop sets it (asar) and pass it (pipe owned by desktop? then server must start after desktop -> chicken/egg).
- remoteControl/enable from desktop -> our server connects to OpenAI relay (same as desktop's own server; acceptable?).
- Node death -> desktop loses backend: supervise server as separate long-lived process not tied to node lifetime.
- Desktop auto-updates -> bundled codex.exe path/hash changes -> detect and restart server on new binary.
