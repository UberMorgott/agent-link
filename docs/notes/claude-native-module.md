# Claude Code: the in-process hooks module

`plugins/agent-link/hooks/register.ts` is agent-link's Claude Code integration as
a function-hooks module (Claude Code 2.1.295+, early access): it does through the
engine's `$` what the shell hooks (`agentlink hook claude`), their Stop-hook
waiter (`--wait`) and the `agentlink mcp` process do. The shell hooks and
`.mcp.json` still ship beside it; Codex is untouched (its hooks, MCP and queue).

## What runs where

| Piece | Module | Node |
| --- | --- | --- |
| Registration | `session.start` → `POST /sessions` with `native: true`, idle at turn boundaries, a keep-alive every minute | keeps the shell hooks of a native session out (`sessionNative`) |
| Idle wake | held `GET /unread?native=1&waiter=1&wait=25` → `POST /render` (mode `wake`) → `POST /claim` (wake token) → `$.prompt.submit` | holds the request (`WaitUnread`), words the prompt (`Render`) |
| Mid-turn | the same loop without the waiter flags → `$.session.append` | |
| With a prompt | `prompt.submit` adds what waits as context (first text of a session: the CLI's path) | |
| Subagents | `classic.SubagentStart/Stop` → `agents` of the registration; a subagent's `tool.call` gets its replies as `context` (`for_agent`) | marks an ended subagent's reply (`EndedAgentNote`) |
| Ack | first chunk of a `turn.step` of the carrying turn → `POST /ack` | |
| Not read | `turn.complete` aborted/error/refusal, a dropped prompt → `POST /release` | `Release`: lease back in retry, claim dropped |
| Activity | `tool.call` → `POST /sessions/{id}/doing {tool}`, `POST /chats/{id}/activity {tool, input}` | reads the state (`DoingOf`), words the line (`ToolActivity`) |
| Tools | the nine of `agentlink mcp`, list generated into `hooks/tools.gen.ts` (`TestNativeToolsMatchMCP`) | |

The module words nothing the model reads: every text comes from `POST /render`.
State that must outlive the module's copy (the engine may replace it mid-session)
is in `$.state`: the session, the running turn, what was handed over and not yet
acknowledged.

## Both in one session

A session the module registered is `native` at the node while it lives. The
shell hooks of that session then get nothing: their registration only keeps it
live (no inbox, no idle state of theirs), their unread queries (hook, Stop,
waiter) are answered empty and their claims are not granted. The shell side
stands down, by the node's word, because the module is the side that knows
when a turn really read a message; it needs no change in an installed plugin
or an older executable. A node that does not know `native` (older than this
change) has no `POST /render`: the module then hands nothing over and the shell
hooks deliver as before.

Only the module's own registrations and keep-alives keep a session `native`
(`Session.NativeSeen`); the shell hooks' never do. A module that says nothing
for three of its keep-alives (`nativeSeenTTL`: refused at a reload, disabled,
crashed) has the session no more: the shell hooks deliver again, and what the
module had claimed for a turn and never acknowledged is theirs to hand over.
When the module registers again they stand down again.

## Handed over, not read yet

A message the module handed over stays claimed at the node until a model
request read it: a turn's claim is taken again every half minute, and so is a
wake's (with the wake's own token, which renews that wake and counts as no new
one) while its prompt waits behind a running turn or is still on its way in. A
wake is given up only when its prompt entered an idle session and no turn
started with it for two minutes. What the person's prompt carried is
acknowledged by the turn that prompt started and by no other; a prompt a hook
beneath dropped gives its messages back at once.

## Checks

```
claude plugin validate plugins/agent-link
claude plugin test plugins/agent-link
go test ./plugins ./cmd/agentlink -run 'TestNativeModule|TestNativeToolsMatchMCP'
```

Type-check: a `tsconfig.json` kept outside the plugin folder with the options
from the header of the engine's `claude-code.d.ts`, its `include` naming that
file and the plugin's `hooks`, `types`, `tests`; then `tsc -p <that tsconfig>`.

A headless run (`claude -p`) is not a session, as for the shell hooks: it has
the tools and takes no message. `AGENTLINK_NATIVE_HEADLESS=1` makes one attended
for a proof against a throwaway node (`AGENTLINK_API`).

## Trying it in a terminal session

1. `claude --plugin-dir <repo>/plugins/agent-link` in a folder of the node.
2. Leave the session idle; from another member send it a message. The status
   line shows `agent-link: непрочитанных — 1` and a turn starts by itself,
   opening with `The agent-link plugin sent a message:`.
3. In the app the message turns read when the model starts to answer; `/clear`,
   then send another: the new session is woken the same way, once.
