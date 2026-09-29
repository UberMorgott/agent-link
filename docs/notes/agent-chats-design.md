# Local agent chats: Claude Code <-> Codex through agent-link

Status: design, 2026-09-29, against main a363b9b (v0.6.35). No code changed.

Goal: replace the `cx` shell peer with agent-link `discuss` for local Claude Code <-> Codex talk,
with per-caller thread continuity, per-subagent threads, cleanup on end, and a sidebar that shows
only live temporary chats grouped per project.

## Today's model (facts the design builds on)

- `discuss` (MCP `discuss`, CLI `agentlink discuss`) posts into a *local binding*: the folder's
  local project chat by default, or a `LocalChat` binding (topic / temporary / folderless)
  - picker: `internal/app/localchats.go:78-141` (`discussContextLocked`)
  - request: `internal/app/discuss.go:17-33`, handler `:39-143`, reply wait `:164-238`
  - MCP schema: `cmd/agentlink/mcp.go:76-86`, tool `:145-152`
- A binding has at most one seat per provider; the first discuss adds it
  (`discuss.go:92-108`). A seat = one agent thread: `Seat.SessionID` (`internal/node/seats.go:72-86`),
  resumed headless each turn (`runSeatTurn` `seats.go:1013-1111`, `LaunchSpec.ResumeID` `:1081`;
  Codex = `codex exec resume`, `internal/worker/command.go:141-149`; turn cap
  `desktopTurnTimeout = 60m`, `internal/node/launch_desktop.go:58`).
  => **Thread identity = (binding, provider).** Continuity is whatever key picks the binding.
- `LocalChat` (`internal/settings/settings.go:156-170`): `Temporary|Topic`, `Project`, `Folder`,
  `Sessions` (creator first), `LastUsed`. Cap `MaxLocalChats = 64` (`settings.go:289`), `maxSeats = 8`
  per node (`seats.go:66`).
- Subagent identity: Claude Code hook input `agent_id/agent_type` (`cmd/agentlink/hook.go:103`);
  PreToolUse stamps the asking subagent (`hook.go:317-324`), the MCP/CLI resolves it with
  `askOrigin` (`cmd/agentlink/main.go:451`) and sends `agent_id` in discuss; the node records it on
  the message for reply routing only (9a2b857, 4e78e6f, 33bef52). Ambiguous stamp -> parent session.
  Live subagents reported as `SessionRequest.Agents` (`internal/node/sessions.go:75-79,443`),
  lapse after `agentLiveTTL = 30m` (`sessions.go:123`); hook side `liveAgentTTL = 30m`
  (`cmd/agentlink/hook_agents.go:44`). Stamping and `trackAgent` are Claude-only (`hook.go:310,317`).
- Session end: hook `SessionEnd` -> `DELETE /sessions/{id}` (`hook.go:291-293,514-525`), node
  `EndSession` (`sessions.go:478-498`) -> `onSession(sid,false)` -> app `reconcileSession`
  (`internal/app/contexts.go:178-192`, per-session lock). Expiry without SessionEnd:
  `expireSessions` (`sessions.go:568`); TTL Claude 900 s, Codex 3600 s (`hook.go:89-90`); pinned
  idle Codex threads are heartbeated by the wake loop (7920dc1).
- Temporary chat GC: every 10 min, removed when no session in `Sessions` is live, nothing pending,
  idle 24 h (`localchats.go:32-37,233-287`; `TempChatIdle`). Unread needs_human reply keeps it (c0615c5).
- UI: `ProjectSidebar.vue:43-44` splits bindings into network `tree` and flat `localTree`
  (every local binding: project chats + topic + temporary, one row each). `ProjectView.local_chat`
  exists in Go (`internal/app/projects.go:57`) but is absent from `web/src/types.ts:232-254`, so the
  UI cannot tell scope, owner or expiry.
- `codex-peer` skill (`~/.claude/skills/codex-peer/SKILL.md:8-14`) already routes text asks to
  `discuss with=codex`; `cx` stays for images / explicit old threads. Global `CLAUDE.md`
  "Codex GPT peer" section still says `cx`.

## R1 - Claude asks Codex via agent-link, not `cx`

- State: **exists** for text asks. `discuss with=codex` waits for the exact seat reply
  (`discuss.go:164-238`); skill already defaults to it.
- Gap: global `~/.claude/CLAUDE.md` "Codex GPT peer" still prescribes `cx`; `cx` features without
  agent-link equivalent: `-Img`, `-Review` (diff-scoped), `-Deep` (background >10 min),
  `-Fork/-Resume <id>`. Codex MCP client tool timeout may be shorter than the 10 m wait
  (Codex `tool_timeout_sec`; unverified value) -> Codex-side sync discuss could be cut.
- Design: no code for the ask itself. (a) doc/rule edit: CLAUDE.md peer section -> "use
  `agent-link` discuss with=codex; `cx` only for -Img"; (b) `-Deep` = `discuss async:true` +
  hook delivery of the reply (already works: reply is unread for the origin session);
  (c) review = prompt convention ("review `git diff` of <repo>") - no new API.
- Risks: long asks exceed 15 m cap (`discuss.go:172`) -> must use async; images need `cx` until an
  attachment-out path exists.

## R2 - Same session + same topic -> same Codex thread

- State: **partial**.
  - In a project folder the default is the folder's project chat: ONE Codex seat/thread shared by
    every session and every subagent in that folder, forever (`localchats.go:106-108`) -> continuity
    yes, isolation no (two Claude sessions share Codex's memory).
  - `topic` -> persistent chat keyed by (project, topic) only, shared across sessions (`:116`).
  - Per-session temporary reuse only for the folderless default (`:117`: `Sessions[0]==session`);
    `temporary:true` always creates a new chat (`:109`), continuation needs the caller to carry `chat`.
- Gap: no routing key that includes the caller session; the caller must remember chat ids.
- Design:
  - Add owner identity to `settings.LocalChat`: `Owner LocalChatOwner {Session, Agent, Provider string}`
    (Agent "" = main agent). `Sessions` stays for "used by" (unread/claim scope).
  - Routing key = `(Project|"" , Owner.Session, Owner.Agent, lower(Topic))`, Topic "" = the
    caller's default thread. New default for agent callers (`session_id` set, no `chat`/`topic`/
    `temporary`): find-or-create a *session chat* (`Temporary: true`, owner = caller) in the
    folder's project (or folderless). Humans (no session) and `chat:<id>` keep today's behavior.
  - `topic` from an agent caller: key includes the owner -> per-session topic thread
    (temporary, owned). Explicit shared persistent topic stays reachable with new flag
    `shared:true` (today's semantics) - see owner question Q1.
  - Lookup in `discussContextLocked` replaces the `mine` test (`localchats.go:116-117`) with an
    exact key match; LRU on `LastUsed` stays as tie-break for legacy chats without Owner.
  - Migration: existing temp chats without Owner: treat `Sessions[0]` as Owner.Session, Agent "".
- Risks: more chats/seats -> hit `MaxLocalChats=64`; mitigated by R4 retirement + LRU eviction of
  retired chats. Thread cold-start cost (intro turn) per new owner.

## R3 - Subagents get their own persistent Codex threads

- State: **missing** (identity exists, only used for reply routing). `discuss` passes
  `agent_id` (`discuss.go:26-27,116`) but chat/seat choice ignores it -> a subagent asks in the
  parent's (or folder's) thread.
- Design:
  - Owner.Agent = resolved `agent_id` (askOrigin). Same routing key as R2 -> each subagent gets its
    own session chat -> own seat -> own `Seat.SessionID` thread, reused across its calls.
  - Ambiguous stamp (no agent id) -> parent key, as today (never guess, 4e78e6f).
  - Parent may hand a thread to a subagent explicitly via `chat:<id>` (unchanged).
  - Codex callers: extend stamp + `trackAgent` to Codex if its PreToolUse input carries
    `agent_id` (comment `agenthook.go:41-42` says both clients send it; stamping is gated to Claude
    at `hook.go:310,317`) - verify on Codex 0.155 first; else Codex subagents share the parent key.
- Risks: agent ids are per-run (a re-spawned subagent of the same type gets a new id -> new thread;
  intended). Parallel fan-out of N subagents -> N seats; seat cap is per binding (1 per provider), so
  fine, but N concurrent `codex exec` processes - see Q3 (concurrency cap).

## R4 - Threads close when the subagent / session ends

- State: **partial**. Per-turn Codex processes already end with the turn (headless resume, 60 m cap)
  and `RemoveSeat` cancels a running turn (`seats.go:400-424`). Temporary chats are collected only
  24 h after their sessions end (`localchats.go:233-264`); project-chat seats never close; nothing
  reacts to SubagentStop.
- Design: *retire* an owned chat when its owner ends:
  - Session end: extend `reconcileSession` (`contexts.go:178`) - when sid is live nowhere, call
    `retireOwnedChatsLocked(sid, agent="*")`. Covers SessionEnd and expiry (same callback path).
  - Subagent end: node diffs `Session.Agents` in `RegisterSession` (`sessions.go:443`) and on
    `agentLiveTTL` lapse, firing new callback `onAgentGone(sid, agent)`; hook already forces a
    heartbeat with the new list on SubagentStop (`hook.go:310-312`). App -> `retireOwnedChatsLocked(sid, agent)`.
  - Retire = `StopSeat`+`RemoveSeat` of its seats (cancels running `codex exec`, kills the process
    tree via ctx), wait for turn exit (bounded 10 s), then `leaveProjectLocked` (data -> `.left`, as GC).
    Unread reply for the gone owner: keep data in `.left`, mark needs_human in dashboard, but hide
    from sidebar (see Q2).
  - Delay: grace `RetireGrace = 2m` after end (a resumed Claude session re-registers the same id;
    `claude --resume` keeps session id) - retire only if still gone.
  - Codex-side thread: rollout file stays on disk (no Codex API needed). Optional: archive via
    app-server `thread/archive` when the shared daemon path exists (unverified; not in PR scope).
  - Existing 24 h GC stays as backstop for chats without Owner.
- Risks: SessionEnd hook budget 1.5 s Claude / 3 s Codex (`agenthook.go:61-63`) - retirement must
  run async in the app, never in the hook. False "gone" when hooks are missing -> TTL expiry retires a
  still-open session's threads; grace + re-create-on-next-ask limits damage (context lost). A
  running turn killed mid-reply: its lease goes back (`revokeLeases`), reply lost - acceptable on end.

## R5 - Codex asks Claude the same way

- State: **exists (untested end-to-end as a matched pair)**. `discuss with=claude` is accepted
  (`discuss.go:53`), `source=codex` + `CODEX_THREAD_ID` as session (`main.go:516-521`), Claude seat
  = headless `claude -p --resume`; shared skill documents it (`plugins/agent-link/skills/agent-link/SKILL.md:47,90-91`).
- Gap: R2-R4 keys must be provider-neutral (Owner.Provider); Codex subagent identity (R3 note);
  Codex MCP tool timeout vs 10 m wait (R1 risk); Codex TTL 3600 s delays R4 on sessions without
  SessionEnd.
- Design: same code path; Owner.Provider = `source`. Default `timeout` for Codex callers = min(10m,
  Codex tool timeout - 5 s), fall back to async + hook delivery when exceeded.
- Risks: loop Claude<->Codex asking each other - bounded by existing chain/autonomy limits
  (`seats.go:42-45`, autonomy budgets).

## R6 - Temporary chats in the sidebar only while live

- State: **missing**. Every local binding shows until GC (24 h after idle) (`ProjectSidebar.vue:44`).
- Design:
  - Server: add to `LocalChatView` (`localchats.go:42-50`): `Owner` (session, agent, agent_type,
    provider), `Live bool`, `Waiting bool`. `Live` = owner session (and agent if set) live, or a
    seat running/pending, or a discuss waiter open, or unread. Computed in `projects_api.go:159`
    from `localChatActivity` (`localchats.go:268-287`) + session registry.
  - Client: add `local_chat?: LocalChatView` to `ProjectView` (`types.ts:232`); `localTree`
    filters temporary chats to `local_chat.live`; project chats and persistent topics always shown.
  - Row label: `<agent_type or "main"> · <topic>` + live dot; retired chats vanish on the
    `projects` event already published by leave (`localchats.go:262`).
  - `AgentsView` keeps a full list (incl. hidden) for history/needs_human.
- Risks: flicker when a session idles between turns -> hide only after `hideGrace = 60s` not-live.

## R7 - Sidebar grouped per project, accordion when several chats

- State: **missing** (flat list).
- Design: `localTree` -> groups keyed by `local_chat.project || p.id` (project binding itself for
  `local_chat == nil`), folderless chats under a "No project" group. Group with 1 visible chat =
  plain row (today's look); >= 2 = expandable row (chevron, count, aggregated unread/dot), children
  indented. Expanded state per group in `localStorage` via the projects store. New component
  `LocalChatGroup.vue`; `ProjectSidebar.vue:156-195` renders groups. Optional: network projects
  that have a local project for the same folder show it as child - Q4.
- Risks: ids/tests: `#local_chat_tree [data-project]` selectors (`AgentsView.test.ts:39-40`) must
  keep working (children keep `data-project`).

## Implementation plan (independently shippable PRs)

1. **PR1 docs(peer): agent-link replaces cx for text asks**
   - Files: `~/.claude/CLAUDE.md` Codex peer section (owner's file - propose diff, owner applies),
     `plugins/agent-link/skills/agent-link/SKILL.md` (async for long asks, review convention).
   - Done: skill + rule say discuss first; `cx` only for `-Img`. No code.
2. **PR2 feat(discuss): owner-keyed session chats (R2)**
   - Files: `settings/settings.go` (LocalChatOwner, validate, migration), `app/localchats.go`
     (key lookup, default for agent callers, `shared` topic), `app/discuss.go` (request field
     `shared`), `cmd/agentlink/mcp.go` + `main.go` (flag/param), `docs/agent-usage.md` scope table.
   - Tests: `localchats_test.go`: same session twice -> same binding+seat; two sessions -> two;
     same session two topics -> two; human caller -> project chat; legacy chat migration.
   - Done: `go test ./...` green, qgate pass; two live Claude sessions in one folder get distinct
     Codex `Seat.SessionID`, a second ask from one resumes its thread (`seats.json`).
3. **PR3 feat(discuss): subagents own their threads (R3)**
   - Files: `app/localchats.go` (Owner.Agent in key), `app/discuss.go`; optional Codex stamping in
     `cmd/agentlink/hook.go:310,317` behind a verified-input check.
   - Tests: discuss with agent_id A/B -> distinct chats; no agent_id -> parent's; hook_agents test
     for Codex stamp (if enabled).
   - Done: two parallel Claude subagents asking Codex get separate threads, each reused on its 2nd ask.
4. **PR4 feat(sessions): retire owned chats when their owner ends (R4)**
   - Files: `node/sessions.go` (Agents diff -> `onAgentGone`, lapse check in `expireSessions`),
     `node/node.go` (callback setter), `app/contexts.go` (`reconcileSession` hook + agent callback),
     `app/localchats.go` (`retireOwnedChatsLocked`, grace timer), `node/seats.go` (wait for turn exit).
   - Tests: SessionEnd -> chat gone after grace, seat turn ctx cancelled (fake launcher); session
     re-registers within grace -> kept; SubagentStop (Agents shrink) -> only that agent's chat gone;
     unread reply -> data in `.left` + needs_human, not in project list.
   - Done: no `codex.exe` child of agentlink after owner end (process check in test via fake agent
     `internal/worker/testdata/fakeagent`); qgate pass.
5. **PR5 feat(app): live flag and owner in local chat view (R6 server)**
   - Files: `app/localchats.go` (view fields), `app/projects_api.go:159`, testdata
     `internal/app/testdata/projects/project_local.json`.
   - Tests: golden JSON; live/not-live transitions.
6. **PR6 feat(web): grouped, live-only local chats sidebar (R6+R7 client)**
   - Files: `web/src/types.ts`, `components/ProjectSidebar.vue`, new `components/LocalChatGroup.vue`,
     i18n strings, `stores/projects.ts` (expanded state), rebuilt `web/dist`.
   - Tests: vitest: temp chat hidden when not live (after grace), group with 2 chats renders
     accordion, 1 chat renders plain row, existing `AgentsView.test.ts` selectors pass.
   - Done: `npm test` + build + qgate `-All`; screenshot of grouped sidebar.
7. **PR7 feat(discuss): Codex caller parity (R5)**
   - Files: `cmd/agentlink/mcp.go` (timeout clamp for Codex source), skill text; e2e script under
     `scripts/` driving Codex -> discuss with=claude twice (thread reused) + SessionEnd retire.
   - Done: live e2e log with same Claude `Seat.SessionID` on both asks and chat retired after end.

Order: 1 -> 2 -> 3 -> 4 -> 5 -> 6 -> 7 (5 can run parallel to 2-4; 6 needs 5).

## Owner decisions needed

- **Q1 Default thread for an agent caller in a project folder.** (a) per-session chat (new), or
  (b) the folder's shared project chat (today). Recommended: **(a)**; project chat stays for humans
  and `shared:true`.
- **Q2 Unread reply when the owner ended.** (a) retire anyway, keep in `.left` + needs_human in
  dashboard, hidden from sidebar; (b) keep chat visible until read (today's c0615c5 rule).
  Recommended: **(a)** - matches "auto-disappear", nothing is lost.
- **Q3 Concurrency cap on parallel headless agent turns** (N subagents -> N `codex exec`).
  Recommended: **4 per node**, queue the rest.
- **Q4 Network project + local project on the same folder**: show local chats nested under the
  network project row, or separate "Local chats" section grouped by project. Recommended:
  **separate section** (keeps 42a22af separation).
- **Q5 Persistent topics from agents**: per-session (retired with owner) unless `shared:true`.
  Recommended: **yes**.
