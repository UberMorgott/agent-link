# Native Codex integration findings (2026-09-26, codex-cli 0.155.1, Windows)

Verified by experiments (raw logs were in a session scratchpad, not kept).

## app-server protocol
- `codex app-server --listen stdio://|ws://127.0.0.1:PORT`; schema: `codex app-server generate-json-schema --out <dir>`.
- `initialize.capabilities.experimentalApi:true` required for dynamicTools.
- Client-side tools: `thread/start.dynamicTools:[{type:"function",name,description,inputSchema,deferLoading?}]` or `{type:"namespace",name,description,tools:[...]}`. Server request `item/tool/call {threadId,turnId,callId,namespace,tool,arguments}` -> reply `{contentItems:[{type:"inputText",text}|inputImage|inputAudio], success}`. Tools persist with the thread (survive app-server restart via thread/resume). Two ws clients: tool call goes to both, first reply wins.
- Context: thread/start `baseInstructions`, `developerInstructions`, `config`; `thread/inject_items {threadId, items}` (no turn); turn/start & turn/steer `additionalContext {id:{value, kind:"application"|"untrusted"}}`.
- Delivery: `thread/queue/add {threadId, input, clientUserMessageId}` (starts turn if idle, queues if busy — verified both); `turn/steer {threadId, input, expectedTurnId}`.
- Presence: `thread/status/changed` (notLoaded/idle/active{waitingOnApproval|waitingOnUserInput}) to all clients; `thread/loaded/list`.
- Permissions per thread: thread/start `approvalPolicy:"never"`, `sandbox:"danger-full-access"`.
- `thread/resume` fails on a thread another process holds ("already has an active writer") or before first turn ("no rollout found").

## Codex desktop app
- Exe: `C:\Program Files\WindowsApps\OpenAI.Codex_<ver>_x64__2p2nqsd0c76g0\app\ChatGPT.exe`; normally spawns `%LOCALAPPDATA%\OpenAI\Codex\bin\<hash>\codex.exe -c features.code_mode_host=true app-server --analytics-default-enabled -c plugins.codex-app-tools@openai-bundled.mcp_servers.codex_app.enabled=true` over stdio.
- Env `CODEX_APP_SERVER_WS_URL=ws://127.0.0.1:PORT` (unless `CODEX_APP_SERVER_FORCE_CLI=1`) -> desktop uses external server, no auth on loopback. VERIFIED live: owner threads intact, our threads appear in Recent, custom tool calls not raced, queue/add works. Env must reach ChatGPT.exe directly (shell:AppsFolder launch drops it). Problems: `codex_app` MCP fails without `CODEX_APP_TOOLS_PIPE_PATH`; desktop calls `remoteControl/enable`; version must match bundled codex.exe; server death = desktop loses backend.
- `CODEX_CLI_PATH` overrides the binary desktop spawns -> proxy in front of codex.exe (keeps desktop env/pipe intact). Being tested by the Aegis session (not by us).
- Deep links: `codex://threads/<id>` (open), `codex://threads/new?path=&prompt=` (fill-in + trust dialog), `--open-project <path>`.
- `codex queue --thread <id> --message <text> [--image]`: auto-starts a turn in an idle TUI.
- `notify` config program fires on turn complete (owner config already uses it for computer-use; would need chaining).
- Not integration points: codex-code-mode-host (V8 sandbox, private gRPC), codex-command-runner, codex-windows-sandbox-setup.

## Claude desktop
- Deep links: `claude://resume?session=<uuid>` (shows a CLI/`-p` session in the app as local_<uuid>), `claude://code/new?folder=&q=` (fill-in only), `claude://claude.ai/epitaxy/local_<id>`, `claude://code/continue?session=`.
- Each session has inbox pipe `CLAUDE_CODE_MESSAGING_SOCKET` + `CLAUDE_CODE_MESSAGING_TOKEN` (env to hooks); protocol: two lines `{"type":"auth","token":...}` then `{"type":"user","message":{"role":"user","content":...}}`; text only.
- No local HTTP API; session tools (start_session/send_message) only inside Claude sessions.
