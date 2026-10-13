# agent-link-native (spike)

agent-link for Claude Code as an in-process hooks module (Claude Code 2.1.295+,
function hooks are early access). It does, through the engine's `$` alone, what
`plugins/agent-link` does with shell hooks, the Stop-hook waiter and the
`agentlink mcp` process. The classic plugin is untouched; do not enable both in
one session (each would deliver the same messages).

Superseded by the module of `plugins/agent-link` (`hooks/register.ts` there),
which is what ships; this folder is kept only as the record of the spike. It is
in no marketplace and nothing installs it: it loads only when someone names it
with `--plugin-dir`. Never load it in a session that has `plugins/agent-link`
(or the installed `agent-link` plugin): both would register the same session
with the node as its only way in and each would hand over the same messages.

- `hooks/register.ts` — the module: registers the session (`POST /sessions`),
  polls `GET /unread?waiter=1`, wakes an idle session with `$.prompt.submit`,
  appends to a running turn with `$.session.append`, acknowledges (`POST /ack`)
  when a model request of the carrying turn starts to answer (`turn.step`),
  serves the tools `mcp__agent-link-native__unread` and `…__send`.
- `types/index.d.ts` — the `$.state` contract.
- `tests/*.test.ts` — run by `claude plugin test`.

The node's address: `$AGENTLINK_API`, else `api` of the desktop app's
`config.json`, else `127.0.0.1:7520` (as `agentlink hook` finds it).

## Checks

```
claude plugin validate plugins/agent-link-native
claude plugin test plugins/agent-link-native
go test ./plugins/agent-link-native
```

Type-check: a `tsconfig.json` kept outside this folder with the options from
the header of the engine's `claude-code.d.ts`, its `include` naming that file
and this folder's `hooks`, `types`, `tests`; then `tsc -p <that tsconfig>`.

## Trying it in a terminal session

1. `claude --plugin-dir <repo>/plugins/agent-link-native --setting-sources project`
   in a folder of the node (`--setting-sources project` keeps the classic
   plugin and the user's hooks out of this one session).
2. Leave the session idle; from another member send it a message. Within a few
   seconds the status line shows `agent-link: 1 unread` and a turn starts by
   itself, opening with `The agent-link-native plugin sent a message:`.
3. In the app the message turns read when the model starts to answer; ask the
   session to reply and it calls `mcp__agent-link-native__send`.
