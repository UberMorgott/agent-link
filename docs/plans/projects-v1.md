# Projects v1 — implementation plan

Status: agreed with Codex after 3 review rounds, no blocking objections (§13). Open product questions: none — every
decision is recorded in §2 with its rationale.

## 1. Goal

The sidebar lists **projects**, not one global network. Each project has its own
invite code, members, shared name and chats, and every member binds their own
local folder to it. The UI follows ChatGPT/Claude: `＋ Новый проект` /
`Присоединиться`, a project tree with `＋ Новый чат` and the chats, and a project
`⋯` menu (Участники, Приглашение `•••• 👁 Копировать`, Общее имя, Моя папка,
Выйти). The pairing code leaves the Settings page. The duplicated empty-state
texts go: an empty project shows one line `Начните разговор в <project>` with
`Новый чат` and `Пригласить`.

## 2. Decisions (product and architecture)

| # | Decision | Rationale |
| --- | --- | --- |
| D1 | One `node.Node` per **context** (a project, or the pre-projects network "Прежняя сеть"), all behind one `node.Hub` that owns the listener, discovery socket, handshake limits and the control-API router. | `Node` already does PAKE, sealed records, gossip with a 64-member cap, mesh dialing, chats, receipts, sessions. Per-context instances keep those paths unchanged, and "no fallback into the global network" becomes structural. A multi-network `Node` would touch every `n.members/conns/store` use. |
| D2 | The legacy network is kept as the context `legacy` ("Прежняя сеть") only while `settings.Key()` exists. Nothing is converted into a project. It cannot be created from the new UI; the join dialog still accepts an `XXXX-XXXX-XXXX` code and stores it as the legacy code when none is set. | Users with not-yet-upgraded peers keep working; no silent merge of an old network into a project. |
| D3 | Anyone can rename a project (shared name, LWW). Local alias is per member and never sent. | Requested; no roles in v1. |
| D4 | Creating a project needs only a name; the folder is optional. Without a folder the project is `needs_folder`: chats work, the agent never runs (requests get a held status). | Lets people chat before choosing a folder; no `WorkDir` fallback ever. |
| D5 | `＋ Новый чат` opens a compact participant picker listing every project member with online/offline state, all preselected; the user can uncheck; ≥1 other needed; the picker shows the selected count. Participants are fixed at creation. Being a participant never means being asked: who must answer is still chosen per message in the composer (`ask`). | Explicit, predictable membership (no accidental set by who is online); a fixed list is the existing chat invariant; no agent runs just because someone was added. |
| D6 | Closing a chat is labelled **«Завершить чат»**; the archive holds finished chats. A finished project chat never reopens; a new chat is made instead. | Close affects every participant; "Архивировать" reads as a local action. |
| D7 | Joining = successful PAKE with the project; the confirm step only binds a folder. `Отмена` during a join that **created** the binding = leave; pasting an invite of a project already here just opens it (`created: false`), with no wizard and no rollback. | Membership is gossiped the moment a session exists; honest wording beats a fake pre-join; a re-pasted invite must never make you leave a working project. |
| D8 | `Выйти` sends a self-tombstone marked `left`, stops the context, removes the binding and moves its data to `data/projects/.left/` (never deleted). Re-joining later creates a fresh data dir and node id. Leaving `legacy` clears **both** `Code` and `Secret`, stops it and keeps `data/`. | Reversible by hand; a new incarnation never inherits old queues or chats. |
| D9 | No targeted kick, roles, secret rotation, session multiplexing or internet traversal in v1. `SecretEpoch` is carried (fixed `1`). | Scope; epoch keeps invites forward compatible. |
| D10 | With no projects and no legacy network the app shows a welcome screen (create / join). `/ui/inbox` opens the last used existing project, else the welcome screen; after leaving, the next project opens. | No dead empty page. |
| D11 | Sidebar unread badges use the browser read cursor per `(project, chat)`, not the agent-ack unread. Opening a chat never acknowledges agent work. | Existing UI semantics (`inbox.reads`); agent read state is a different thing. |
| D12 | Per-context `node_id`. | No cross-project correlation beyond the global node name; no store change. |

## 3. Data model

### 3.1 Shared per project (only inside that project's sealed sessions)

```go
// internal/node/project.go
type ProjectMeta struct {
    ID        string    `json:"id"`      // project id
    Name      string    `json:"name"`    // 1..80 runes after trim, no control chars
    Lamport   uint64    `json:"lamport"` // LWW clock of Name
    Writer    string    `json:"writer"`  // node id of the last renamer (tie-break)
    CreatedAt time.Time `json:"created_at,omitzero"`
}
```

- LWW order: `(Lamport, Writer)` lexicographic, higher wins. Rename:
  `Lamport = local.Lamport + 1`, `Writer = own node id`. Creator starts at
  `Lamport 1`; a joiner at `Lamport 0, Name ""`, so any received meta wins.
- Stored in `data/projects/<pid>/project.json` (`writeJSON`, atomic).

### 3.2 Local (never sent)

```go
// internal/settings/settings.go
type ProjectBinding struct {
    ID     string   `json:"id"`              // project id
    Epoch  uint32   `json:"epoch"`           // 1 in v1
    Secret string   `json:"secret"`          // canonical base32 of 16 random bytes
    Alias  string   `json:"alias,omitempty"` // ≤64 runes after trim
    Dir    string   `json:"dir,omitempty"`   // "" = no folder bound
    Peers  []string `json:"peers,omitempty"` // bootstrap addresses typed by the user (host:port)
}
Settings.Version  int              `json:"version"`
Settings.Bindings []ProjectBinding `json:"project_bindings,omitempty"`
```

- Kept in `%APPDATA%\agentlink\config.json` (owner-only, where the code already
  lives). `Secret` reaches the page only through the reveal endpoint.
- `Settings.Projects` (area → dir) keeps its meaning, for the legacy context only.

### 3.3 Invite and keys

`ALP1.<pid>.<epoch>.<secret>.<check>`

- Base32 = RFC 4648 alphabet `A–Z2–7`, no padding; canonical form upper case.
  Parse: trim, remove all whitespace, upper-case, then require exact alphabet
  and lengths (any other byte, padding or wrong length → `invite`).
- `pid` = 16 random bytes (26 chars); `secret` = 16 random bytes (26 chars);
  `epoch` = decimal `1`.
- `check` = first 4 base32 chars of `SHA-256("ALP1."+pid+"."+epoch+"."+secret)`
  over the canonical strings.
- Key: `HKDF-SHA256(ikm = decoded secret bytes, salt = decoded pid bytes,
  info = "agentlink/project-key/v1/epoch=1", len 32)`.
- Discovery tag: `hex(HKDF-SHA256(ikm = key, salt = nil, info =
  "agentlink/project-tag/v1", len 8))` — no Argon2: the secret has 128 bits.
- `internal/config/invite.go`: `NewProjectSecret()`, `NewProjectID()`,
  `FormatInvite`, `ParseInvite`, `ProjectKey`, `ProjectTag`, `ValidProjectID`;
  fixed test vectors in `internal/config/testdata/invite_vectors.json`.

### 3.4 Chats inside a project

- A chat lives in exactly one context's `chatStore` (its data dir).
- **Chat ids are project-scoped** (fixes cross-project collisions):
  - keyed chats in a project node:
    `DerivedID("agentlink-chat-v3/"+pid+"\x00"+strings.Join(members, ","), gen)`
    where `members` are the sorted `name@nodeid` entries;
  - standalone project chats (`Chat.Mode = "project"`, from `＋ Новый чат`):
    `DerivedID("agentlink-chat-v3/"+pid, creatorNodeID+"/"+newID())`;
  - legacy context: today's `KeyedChatID` unchanged.
  - `Chat` stores `Project string json:"project,omitempty"`; `Keyed()` uses it.
- **Participants are pinned to node ids** in project chats:
  `Chat.ParticipantIDs []string json:"participant_ids,omitempty"` aligned with
  the sorted `Participants`, carried on every envelope as
  `Message.ParticipantIDs`. A project node rejects an envelope whose
  `participant_ids` is missing, misaligned, holds an invalid id, or whose entry
  for the sender ≠ the session's node id, or whose entry for itself ≠ its own id.
  `fanout` queues to a name only while the member record of that name still has
  the pinned id; otherwise the delivery shows `left` and nothing is queued.
  Receipts are applied only from the pinned incarnation.
- **New incarnation cannot inherit queues**: when a member record changes to a
  different node id, or a `left` tombstone is merged, `outbox/<name>/` of that
  context moves to `dropped/<name>-<oldid>/` (kept, never sent).
- `Message.ChatMode` (`"project"` for standalone) on every envelope;
  `chatStore.ensure` requires `Gen == 0` for it. Only nodes with `projects-v1`
  are ever project members, so older peers never receive these fields.
- Finished (closed) standalone chat: `openChatOf` returns `ErrChatClosed`; it is
  archived only when closed (today `!Keyed` means archived, so `chatInfo` checks
  `Mode`).
- Areas are off in project nodes (`cfg.Areas = nil`, chat `Area` always ""):
  the bound folder is the project. Keyed chats stay available so an agent's
  `agentlink send --to bob` from a project folder continues the direct chat
  with bob of that project.

### 3.5 Folder, workers, agent lifecycle

- A project node gets `SetFolders(binding.Dir, nil)`. With `Dir == ""` the node
  runs, there is no worker, and the inbound hook answers every request that asks
  this node with the held status `HoldNoFolder` (text: "у участника не выбрана
  папка проекта").
- One worker per context with a folder: `worker.New(nil, ctxNode.SendMessage,
  data/projects/<pid>, binding.Dir, opt)` with `opt.Chats = ctxNode`, `opt.Project =
  nil`, `opt.ProjectID = pid` (new; the agent env gets `AGENTLINK_PROJECT_ID`),
  `opt.Slots = app.slots`.
- `worker.Slots` (new, shared by all workers): capacity = settings `MaxJobs`,
  closed at creation. Start is two-phase over the whole app: every worker of
  every context runs `Worker.Reattach()`, which registers its running jobs with
  `Slots.Hold` (may exceed capacity; never blocks); only after all of them
  returned does the app call `Slots.Open()`. `Slots.Acquire` (new jobs) waits
  until open and in use < capacity. A context added later (create, join,
  rebind) reattaches before its worker runs; the slots are already open.
  Lowering `MaxJobs` never kills running processes.
- Leave and folder change: `Worker.Quiesce()` atomically (under the worker
  lock) stops intake and checks for non-terminal jobs. Busy → intake resumes,
  `409 project_busy`. Not busy → intake stays closed: `Accept` returns
  `errDraining`, so the message is not ACKed and the sender resends it later
  (to the rebound context, or never after leave). If the settings save or the
  context restart after a successful `Quiesce` fails, intake is resumed
  (`Worker.Resume()`) before the error is returned (tested). A successful folder change
  deletes the context's worker agent sessions (`<data>/sessions/*.json`), so the
  next request starts a fresh agent session in the new folder.
- A project context without a folder has **no folder at all**: `folderMap`
  gets a `none` mode in which `FolderArea` matches nothing. `POST /sessions`,
  `GET /wait` and `GET /unread` for it answer `409 project_needs_folder`, even
  with an explicit `project`. With a folder, a given `folder` must be inside the
  binding dir, also with an explicit selector (`409 folder_not_in_project`).
  People keep chatting in the web UI either way.

## 4. Storage layout, migration, crash safety

```text
%APPDATA%\agentlink\
  config.json                      Settings v2
  config.v1.bak.json               written once by the v1→v2 migration
  data\                            legacy context (layout unchanged)
  data\projects\<pid>\             one project context: inbox/ outbox/ sent/ dropped/
                                   chats/ members.json node_id pake_seen.json
                                   sessions.json jobs/ sessions/ project.json
  data\projects\.left\<pid>-<unix>\  left or orphaned project data (moved, never deleted)
```

- Migration in `settings.Load`: `version` absent/0/1 → write
  `config.v1.bak.json` unless it exists, set `version = 2`, keep every field,
  save atomically. `version == 2` → no-op. `version > 2` → load error "settings
  from a newer agentlink" and the app never saves over it.
- Settings page saves keep `project_bindings`, `code` and `secret` from the
  current settings (the page no longer sends them). `Validate` requires
  `work_dir` for a handler only when a legacy key exists.
- Binding validation: `ValidProjectID`, epoch 1, secret canonical and 16 bytes,
  alias ≤64 runes, dir empty or absolute and an existing directory, ids unique,
  normalized dirs unique (`filepath.Clean` + case-fold on Windows; nesting
  allowed, deepest wins), at most `maxProjects = 32`.
- Order of writes: create/join = data dir + `project.json` first, then config;
  leave = config first, then stop, then move the dir. On start the app moves
  every `data/projects/<pid>` without a binding to `.left/<pid>-<unix>`. A
  binding without a data dir gets a fresh one.

## 5. Protocol and handshake

- `frame.Project string json:"project,omitempty"` in `hello`, both directions.
- `CapProjects = "projects-v1"`, announced by project nodes only.
- CPace: `prs = ProjectKey`, channel id `CI = "agentlink/cpace/project/v1\x00"+pid`
  (legacy keeps `cpaceCI`). The raw hello lines, which carry `project`, are in
  the transcript hash too.
- Project nodes refuse any hello without `pake` (never the legacy MAC handshake)
  and any `project ≠ own pid`, on both sides (`ErrWrongProject`). The legacy node
  refuses a hello with `project`. The Hub answers an unknown pid with
  `hello{error:"unknown-project"}` without a MAC → dialer `ErrUnknownProject`.
- Frame `project` `{project_meta: ProjectMeta}`: sent after `members` on every
  new session and to every `projects-v1` session when a merge or rename changed
  it; merged by LWW (§3.1).
- `Member.Left bool json:"left,omitempty"` on a self-tombstone. A `left`
  tombstone does not block a later hello with the same name and a **different**
  node id (a re-join). Plain `RemoveMember` keeps today's semantics; the
  project UI does not expose it.
- Beacons: legacy keeps v2 as today. Project beacons are `v: 3` with
  `net = ProjectTag`; the Hub sends one per context per `BeaconEvery` and routes
  a heard beacon by tag to that node's `heard`. Older nodes ignore unknown tags.
- Limits: 64 members per project (existing `maxMembers`), 32 projects, one
  shared `authGuard` and `connLimit` (pending handshakes) for the Hub, at most
  256 live sessions over all contexts (both directions, checked in `register`),
  at most 64 concurrent outbound dial attempts over all contexts (`Hub.dialSem`,
  acquired in `dialOnce` around dial + handshake).

### 5.1 Hub demux and limit ownership

1. `Hub.acceptLoop` accepts; `guard.allow(ip)` and `pending.acquire(ip)` — the
   Hub owns both. It creates an `inboundLease{ip, released atomic.Bool}` whose
   `release()`, `fail()` (guard backoff) and `ok()` (guard success) act once.
2. Deadline `handshakeTimeout`; read the first line with a `bufio.Reader` on
   the raw conn, accumulating `ReadSlice('\n')` chunks and failing once the line
   exceeds `maxFrame` (the limit applies to this first line only, never to the
   session). Deadline, oversize or EOF fail the lease and close. Context
   cancellation closes the conn.
3. `decodeFrame`; pick the node by `f.Project` (`""` → legacy). No node →
   unknown-project reply (legacy absent → close), `lease.fail()`.
4. Hand over `handoff{conn: prefixConn{Conn, io.MultiReader(exactHelloBytes,
   copy of br.Peek(br.Buffered()), rawConn)}, lease}` through the node's
   `chanListener` (its `Addr()` is the real listener's, so `listenAddrs` keeps
   working). The send selects on the node's context; a stopped node → close +
   `lease.release()`. Tests: hello split over many TCP writes, hello and the
   next frame in one write, a session carrying more than `maxFrame` in total.
5. `Node.handleInbound` uses the lease instead of its own guard/pending:
   `lease.release()` right after `acceptHandshake` returns (every path), then
   `lease.fail()` or `lease.ok()` exactly like today's `guard.fail/success`.
   A `Node` started without a Hub (tests, CLI `serve`) keeps its own guard and
   limit unchanged.

## 6. App runtime

- `App.startLocked`: `node.NewHub(listener, discovery config, limits)`; the
  legacy node only when `settings.Key() != nil`; one node per binding.
- Project operations never restart other contexts: `Hub.Add(n)`,
  `Hub.Remove(pid)`, and a per-context stop/start for a folder change. A
  settings-page save still restarts everything (today's behaviour).
- Hooks (`syncHooksLocked`) install into `WorkDir`, the legacy area dirs and
  every binding `Dir`.
- Events: every context's node **and** worker publish their topic plus
  `project:<pid>` (the legacy context uses `project:legacy`); project list,
  binding and meta changes publish `projects` and `project:<pid>`.
- Dashboard and history concatenate every context's `Inbox()`; `Entry` gains
  `project`. The participants page serves the legacy context only.

### 6.1 Control API router (agents, CLI; loopback, no token)

Selector sources: `project` query param or JSON field, which the CLI fills from
`--project` or `AGENTLINK_PROJECT_ID`. Owner lookup: the context whose store
holds the chat or message id (ids are project-scoped, §3.4).

- Explicit `project`: unknown → **404**; the context must also own any given
  `chat_id`/`reply_to`/`parent`, else **409**. No fallback.
- No `project`: `chat_id` → its owner; else `reply_to` → its owner; else
  `folder` → deepest binding dir containing it; else the legacy context; none →
  **400** `folder is not in a project`. An id owned by two contexts (only
  possible for pre-v1 data) → **409**.

| Endpoint | Context |
| --- | --- |
| `POST /send` | rule above |
| `GET /wait` | `project` > `chat` owner > `folder` (new param; CLI sends cwd) > legacy |
| `GET /unread?folder=` | `project` > folder rule |
| `POST /ack` | `chat` → owner; without chat each id goes to its owner; ids owned nowhere → `found: false` |
| `POST /chats/{id}/ack`, `/activity`, `GET /chats/{id}`, `/messages` | owner of `{id}` (404 if none) |
| `GET /chats` | `project` → that context; else concatenated, each with `project` |
| `POST /chats` | `project` → that context; else legacy |
| `POST /sessions` | `project` > folder rule; the Hub remembers session id → context |
| `GET /sessions` | concatenated, each with `project` |
| `DELETE /sessions/{id}` | remembered context; unknown → 404 |
| `/members…`, `/inbox` | `project` → that context; else legacy |

## 7. Web API contract (Track A ⇄ Track B)

All under `/ui/api/`, token header as today. `{pid}` = project id or `legacy`.
Every error is `{"error": "<Russian sentence>", "code": "<code>"}` with the
status below; the sentence is `uiStrings["error."+code]`.

### 7.1 DTOs (JSON exactly as served; optional = may be absent)

```ts
type ProjectState = 'connecting' | 'needs_folder' | 'ready' | 'error'
interface MemberInfo {          // node.MemberInfo
  name: string; self?: boolean; online: boolean; addrs?: string[]; seen?: string
  app?: string; proto?: number; legacy?: boolean; old_auth?: boolean
}
interface ProjectView {
  id: string                    // pid or "legacy"
  legacy: boolean
  name: string                  // shared name; "" while connecting; legacy: "Прежняя сеть"
  alias: string                 // "" = none
  display: string               // alias || name || "" (UI shows "Подключение…" for "")
  dir: string                   // "" = none; legacy: work_dir
  state: ProjectState           // ready = name known AND dir set; says nothing about peers online
  problem: string               // "" or a code: unknown_project | auth | name_taken | removed
  online: number                // other members with a session
  total: number                 // other members, not removed
  members: MemberInfo[]         // self first
  can_rename: boolean           // false for legacy
  has_invite: boolean           // false for a legacy long secret (no code)
  busy: boolean                 // worker has unfinished jobs (leave/folder change → 409)
}
interface InviteView { invite: string }       // ALP1… or legacy XXXX-XXXX-XXXX
interface JoinResult {
  project: ProjectView
  created: boolean              // false: this pid (same secret) or this legacy code was
}                               // already here — UI opens it, no wizard, Отмена never leaves it
interface Delivery { peer: string; status: 'queued'|'sent'; state: 'queued'|'delivered'|'read'|'answered'|'left'; at?: string }
interface Job { reply_to: string; job_status: string; activity?: string; activity_info?: ActivityInfo; hold_reason?: string; updated_at: string; stale?: boolean }
interface ActivityInfo { id?: string; type?: string; text?: string; phase?: string; started_at?: string; seq?: number }
interface Presence { area?: string; session?: string; auto_answer?: boolean }
interface ChatMember { name: string; self?: boolean; connected: boolean; compatible: boolean; queued: number; jobs?: Job[]; held?: Job[]; presence?: Presence }
interface Message {             // node.Message as returned by send
  id: string; from: string; to: string; area?: string; body: string; reply_to?: string
  kind?: string; job_status?: string; activity?: string; created_at: string
  chat_id?: string; participants?: string[]; participant_ids?: string[]; responders?: string[]
  root_id?: string; auto_depth?: number; activity_info?: ActivityInfo; hold_reason?: string
  chat_gen?: number; chat_mode?: string; author_kind?: string
}
interface ChatMessage extends Message { // node.ChatMessage
  seq: number; direction: 'in'|'out'; held?: boolean; delivery?: Delivery[]
  unread?: boolean; own_human?: boolean; assigned?: string
}
interface ChatInfo {            // node.ChatInfo
  id: string; project: string; mode?: 'project'; participants: string[]; participant_ids?: string[]
  area?: string; created_at: string; close_id?: string; closed_by?: string; closed_at?: string; gen?: number
  closed: boolean; archived: boolean; legacy?: boolean; peer?: string; title: string; keyed?: boolean
  unread?: number; count: number; last_seq: number; last_message?: ChatMessage; last_at: string
  active: boolean; members: ChatMember[]
}
interface SendRequest { chat_id: string; body: string; reply_to?: string; ask?: string[] }
interface Session { session_id: string; provider: string; folder: string; area: string; project: string; wake: string; ttl_sec: number; registered_at: string; last_seen: string; primary: boolean }
```

Participants are identified by **name** in the UI (unique inside a context);
`participant_ids` is informational.

### 7.2 Endpoints

| Method | Path | Body | 200 response | Errors (status code) |
| --- | --- | --- | --- | --- |
| GET | `projects` | – | `ProjectView[]` (projects by display name, legacy last) | – |
| POST | `projects` | `{name, dir?, alias?}` | `ProjectView` | 400 `name`/`alias`/`dir`/`dir_taken`, 409 `too_many_projects` |
| POST | `projects/join` | `{invite, addr?}` | `JoinResult` | 400 `invite`/`addr`, 409 `conflict_secret`/`too_many_projects`/`legacy_exists` |
| GET | `projects/{pid}` | – | `ProjectView` | 404 `not_found` |
| POST | `projects/{pid}/name` | `{name}` | `ProjectView` | 400 `name`, 404, 409 `legacy_rename` |
| POST | `projects/{pid}/binding` | `{alias?, dir?}` (absent = keep; `""` clears) | `ProjectView` | 400 `alias`/`dir`/`dir_taken`, 404, 409 `project_busy` |
| POST | `projects/{pid}/invite` | – | `InviteView` (the only way to read a secret) | 404, 409 `legacy_invite_unavailable` |
| POST | `projects/{pid}/members/add` | `{addr}` | `ProjectView` | 400 `addr`, 404 |
| POST | `projects/{pid}/leave` | – | `204` | 404, 409 `project_busy` |
| GET | `projects/{pid}/chats?archive=1` | – | `ChatInfo[]` (legacy adds pre-chat history) | 404 |
| POST | `projects/{pid}/chats` | `{participants: string[]}` (names, ≥1 other) | `ChatInfo` (project → `mode: "project"`) | 400 `chat_participants`, 404 |
| GET | `projects/{pid}/chats/{id}` | – | `ChatInfo` | 404 `unknown_chat` |
| GET | `projects/{pid}/chats/{id}/messages?before&after&limit` | – | `ChatMessage[]` | 400 `bad_request`, 404 |
| POST | `projects/{pid}/chats/{id}/close` | – | `ChatInfo` | 404, 409 `chat_legacy` |
| POST | `projects/{pid}/send` | `SendRequest` | `Message` | 400 `empty_body`/`chat_participants`, 404 `unknown_chat` (chat, reply_to not in `{pid}`), 409 `chat_closed` |
| GET | `sessions` | – | `Session[]` | – |

- Scoped send: `chat_id` is required and must belong to `{pid}`, as must
  `reply_to`; `author_kind` is always `human` and `parent` empty (set by the
  server).
- States: `connecting` until the first meta arrives; then `needs_folder` or
  `ready`. `error` exactly while `problem != ""`. `problem` is set only by
  `auth` (ErrAuth), `unknown_project`, `wrong_project`, `removed`, `name_taken`
  and cleared when a session comes up (today's `Node.Problem` rule); timeouts
  and refused dials never set it. Nothing ever removes a binding except leave.
- Old endpoints `chats`, `send`, `members/*`, `settings` stay (legacy aliases);
  `settings` no longer returns or accepts `code`/`project_bindings`.
- Shell routes served by Go: `/ui/p/{pid}`, `/ui/p/{pid}/c/{chat}`, `/ui/welcome`
  (plus today's).
- SSE (`change` events, `topics`): `all` → refetch everything; `projects` →
  refetch `projects`; `project:<pid>` (legacy: `project:legacy`) → refetch that
  project's `ProjectView` (incl. `busy`), its chat list, the open chat's
  timeline/activity if it is in `<pid>`, and `sessions` (shown filtered by
  `project`); `members`/`peer` → `projects` and `status`; `messages`/`worker` →
  dashboard. Every node and worker change carries its `project:` topic, so no
  per-project refresh depends on the unscoped topics.
- Contract fixtures: `internal/app/web/src/test/fixtures/projects/*.json`
  (one per DTO and per error), written in commit A0 and asserted by a Go test
  (`TestContractFixtures` marshals the Go types from a fixed state and compares)
  and loaded by vitest mocks.

## 8. UI (Track B)

- Routes: `/p/:project` (project home), `/p/:project/c/:chat` (chat, reusing
  InboxView internals), `/welcome`, `/inbox` → last project or `/welcome`,
  `/dashboard`, `/settings`, `/participants` (legacy only).
- `ProjectSidebar.vue` replaces the nav item "Сообщения" and the
  `ConversationList` heading: top `＋ Новый проект`, `Присоединиться`; a tree of
  projects (display name, online dot, unread badge from D11, `⋯`); an expanded
  project shows `＋ Новый чат` and its chats (row markup of ConversationList) and
  its own archive toggle; bottom links Dashboard, Settings, theme.
- `ProjectMenu.vue` (`UDropdownMenu`): Участники (modal from `members`),
  Приглашение (modal: `••••` by default, eye fetches `invite` once and toggles,
  closing the modal drops the value, `Копировать`), Общее имя (inline rename;
  hidden when `!can_rename`), Моя папка (pick-folder + alias; shows the 409
  sentence when busy), Выйти (confirm; legacy text says the code is removed and
  history kept).
- `NewProjectModal.vue` (name, optional folder, alias) and
  `JoinProjectModal.vue` (invite or legacy code, optional address → `connecting`
  → shared name → folder + alias → `Готово`; `Отмена` = leave only when the
  join answered `created: true`; `created: false` closes the modal and opens
  the existing project).
- `NewChatPicker.vue` (D5), chat close labelled «Завершить чат» (D6).
- Empty project: one line `Начните разговор в {name}` + `Новый чат` +
  `Пригласить`. Removed: nav `Сообщения`, heading `Беседы`, title `Выберите
  беседу`, hint `Выберите беседу слева…`, `Бесед пока нет…` (keys `nav.inbox`,
  `inbox.h1`, `inbox.list.label`, `inbox.select`, `inbox.select_hint`,
  `inbox.list.empty`).
- Settings: the code block goes; work dir and area folders move into a
  "Прежняя сеть" section shown only with a legacy key.
- `stores/projects.ts` (list, current, invite reveal state in memory only);
  `stores/inbox.ts` scoped by project (paths `projects/{pid}/…`), drafts and read
  cursors keyed `pid:chat`; switching project aborts in-flight requests
  (`AbortController`) or drops their answers by a request generation counter.

## 9. Tests

Go, each in the commit that adds the code:

- config: invite vectors, canonical base32 (case, whitespace, padding,
  non-alphabet), checksum typo, wrong epoch/lengths, key and tag vectors.
- settings: v1→v2 writes the backup once, idempotent; `version > 2` refused and
  not overwritten; page save keeps bindings/code/secret; binding validation incl.
  duplicate dirs; handler without legacy key needs no work dir.
- node: CPace CI binding (same secret, other pid fails); project node refuses
  legacy handshake and foreign pid; legacy refuses `project`; Hub routes
  legacy/project/unknown, never hands a project hello to legacy, a pre-change
  legacy hello (golden bytes) still works; lease released once on every path
  (timeout, oversize line, unknown project, stopped node, auth fail); session
  and dial caps; discovery tag routing; meta LWW convergence and tie-break;
  meta gossip on connect; keyed ids differ per project for equal names;
  standalone chats (two open with equal participants, finished → archived, no
  next gen); `participant_ids` validation and fanout stop after a re-join;
  outbox moved to `dropped/` on a new incarnation; `HoldNoFolder`; leave
  tombstone and re-join with a new node id; 64-member cap per project.
- worker: `Slots` caps two workers together; no new job starts before
  `Slots.Open()` even when another worker reattaches late; `Quiesce` refuses
  while busy and resumes intake, and closes intake atomically otherwise
  (`errDraining` leaves the message un-ACKed); `AGENTLINK_PROJECT_ID` in the
  agent env.
- no folder: `POST /sessions`, `/wait`, `/unread` → 409
  `project_needs_folder` with and without explicit `project`; a folder outside
  the binding dir → 409 `folder_not_in_project`.
- events: a legacy chat message publishes `project:legacy`; a worker-only job
  transition publishes `project:<pid>`.
- join: re-pasting a known invite → `created: false`, binding untouched.
- app: router table §6.1 incl. 404/409 cases; every §7.2 row incl. error codes;
  invite only via reveal; leave moves data to `.left`, orphan sweep; worker
  absent without folder; folder change clears agent sessions; `project_busy`;
  `TestContractFixtures`; e2e with two apps on loopback: create → invite → join
  with `addr` → name arrives → bind folder → new chat → messages both ways.
- cmd: `send`/`wait`/`chat` send `project` from `--project`/env and `folder`.

Vitest (fixtures-driven): projects store (load, create, join state machine,
reveal/hide invite, rename, 409 busy), ProjectSidebar (tree, legacy last,
unread from read cursors), ProjectMenu (masked by default, eye, copy, leave
confirm), NewChatPicker (all preselected, ≥1), empty project line + buttons,
removed strings absent, SettingsView without code, router `/p/…`, `/inbox` and
`/welcome`, stale answers dropped on project switch.

## 10. Commit sequence

Rules: conventional commits (A0 on `main`, the rest on the track branches, see
Integration); each commit green on
`qgate -All -Full`; a commit that changes `web/src` rebuilds and commits
`web/dist`. `TestUIStringsCoverPages` forbids unused keys, so a string key lands
in the commit that first uses it. `strings.go` has two append-only blocks
created in A0 — `// projects: server (Track A)` and `// projects: UI (Track B)`
— each track edits only its block (and B deletes the obsolete inbox keys).

### Track A — backend Go (implementer A)

0. `feat(app): projects API contract types and fixtures` — Go DTO types
   (`ProjectView`, `InviteView`, error `{error, code}` helper), fixtures,
   `TestContractFixtures`, the two string blocks.
1. `feat(config): project invite codec and key derivation`
2. `feat(settings): v2 migration with backup and project bindings`
3. `feat(node): project handshake selector bound into CPace`
4. `feat(node): hub with shared listener, discovery and handshake limits`
5. `feat(node): shared project name with LWW gossip`
6. `feat(node): project-scoped chat ids pinned to member ids`
7. `feat(node): standalone project chats`
8. `feat(node): leave a project and re-join with a new node id`
9. `feat(node): hold requests while no project folder is bound`
10. `feat(worker): shared job slots and busy state`
11. `feat(app): run legacy and project contexts on the hub`
12. `feat(app): route the control API by project, chat and folder`
13. `feat(cmd): project selector for send, wait and chat`
14. `feat(app): projects web API, shell routes and events`
15. `docs: projects in README and agent-usage`

### Track B — frontend (implementer B; starts after A0, uses fixtures only)

1. `feat(ui): project types, api paths and projects store`
2. `feat(ui): project sidebar tree, welcome screen and /p routes`
3. `feat(ui): chats scoped to a project with one-line empty state`
4. `feat(ui): new chat participant picker and finish-chat wording`
5. `feat(ui): project menu with members, invite reveal, name, folder, leave`
6. `feat(ui): new project and join project dialogs`
7. `refactor(ui): drop pairing code and duplicated texts from settings and inbox`

### Integration (lead)

- A0 lands on `main` first. Then A works on branch `projects-a`, B on
  `projects-b`, each commit green on its own branch. One integrator (the lead)
  brings them to `main` in order: A's commits rebased/fast-forwarded first,
  then B's rebased onto them (disjoint files except the two string blocks),
  re-running `qgate -All -Full` on `main` after each integration.
- After A14 and B7: `test(app): projects end-to-end through the web API` (the
  two-app e2e driving `/ui/api/projects…`), a final `qgate -All -Full`, dist
  rebuilt once more if needed.

## 11. Risks

- **Hub demux regressions** on the existing network → legacy path byte-identical
  (no `project`, same CI, same beacons); golden legacy hello test.
- **Resource growth** (32 × 64 members) → caps in §5; one beacon per context.
- **Pinned participant ids** reject a peer that restored its data dir from an
  old backup with a different id → shown as `left`; a new chat fixes it.
- **Detached agent jobs** outliving a stop → `project_busy`, `Slots.Hold`.
- **Secrets in the settings file** → same owner-only file as the code; never
  logged, never in `GET settings`, only via the reveal endpoint.
- **Global node name** clashes → existing name-taken rule per project; shown on
  that project only (`problem: name_taken`).
- **Two tracks editing `strings.go`** → disjoint append-only blocks (§10).
- **Windows path case** in folder routing → `inFolder` (`filepath.Rel`) and
  case-folded duplicate check.

## 12. Out of scope (v1)

Roles, targeted kick, secret rotation, session multiplexing, internet
traversal, dynamic chat participants, converting the legacy network.

## 13. Review log

- Round 1 (Codex): 8 blocking objections, all accepted — cross-project keyed
  chat id collisions (§3.4 project-scoped ids); names without pinned node ids
  (§3.4 `participant_ids`, outbox to `dropped/`); agent lifecycle on
  leave/folder change and slots (§3.5); ambiguous control-API routing (§6.1
  explicit selector, 404/409, unique dirs); legacy leave with a long secret
  (D8, `legacy_invite_unavailable`); Hub limit ownership (§5.1 lease);
  incomplete contract (§7 full DTOs, `{error, code}`, states, scoped send, SSE,
  D11 unread); track/gate coupling (§10 A0 contract commit, string blocks,
  integration step). Product picks adopted: D4, D5, D6, D10. Suggestions
  adopted: canonical base32 + vectors, dial cap, crash-safe write order,
  refuse newer settings versions, stale-answer handling.
- Round 2 (Codex): D5 accepted. 5 blocking objections, all accepted — demux
  handoff kept the rest of the TCP stream only through a limited reader (§5.1
  step 2/4: limit the first line only, `MultiReader(hello, tail, rawConn)`);
  slot restore and busy check races (§3.5 two-phase `Slots.Open`, atomic
  `Quiesce`); re-join via a known invite could leave a working project (D7,
  `JoinResult.created`); SSE missed legacy and worker-only changes (§6, §7
  `project:legacy`, worker topics); a context without a folder still accepted
  agent sessions (§3.5 `none` folder mode, `project_needs_folder`,
  `folder_not_in_project`). Notes adopted: picker count and "participant ≠
  asked" (D5), separate A/B branches with one integrator (§10), explicit rule
  for the `error` state (§7.2).
- Round 3 (Codex): no blocking objections. Notes adopted: §10 wording on
  branches; intake resumed when a save fails after `Quiesce` (§3.5).

### Implementation deviations (Track A)

- A0: the contract fixtures live in `internal/app/testdata/projects/*.json`, not
  under `internal/app/web/src/test/fixtures/projects/` — Track A must not touch
  `internal/app/web` while Track B works there; vitest mocks load them by the
  relative path `../testdata/projects` from `web/`. Error fixtures are
  `error_<code>.json` = `{status, body: {error, code}}`. The DTO fields that §3.4
  names (`Chat.Project/Mode/ParticipantIDs`, `Message.ParticipantIDs/ChatMode`)
  land in A0 as plain fields so the chat fixtures are marshalled from the real
  types; A6/A7 give them behaviour. `ChatInfo.project` and `Session.project` are
  served through the app wrappers `ChatInfoView`/`SessionView` (legacy →
  `"legacy"`).
- A2: a settings page save keeps `project_bindings` (and never returns them);
  keeping `code`/`secret` regardless of the page lands in A14 together with the
  `settings` endpoint no longer accepting `code` — until B7 the page still edits
  the legacy code there. Invalid binding id/epoch/secret or a duplicate id is
  the problem `project_binding` (new sentence `error.project_binding`).
  `settings.Save` also refuses to overwrite a file whose version is newer.
- A4 (after a Codex review): the shared `authGuard` counts failures per
  `(source, scope)` — scope `#hello` before a hello names a context (timeout,
  oversize or missing first line), then the project id (`""` = legacy) — so a
  success in project A never clears guesses against project B and failures in
  one context never block the others; `pendingTotal/PerSource` stay shared. An
  unknown-project hello only `release()`s its lease (no guard failure): a member
  may still dial a project this side left, or be ahead of this side's join.
  The first-line cap is `maxHandshakeLine` (64 KiB), not `maxFrame`: the node's
  wire refuses longer handshake lines anyway. `Hub.Remove` keeps the id taken
  until the node has stopped; `Hub.Wait` closes the Hub to further `Add`.
  `Hub.Start(ctx)`/`Wait()` replace a blocking run; the app calls `Add` after
  `Start`.

### Implementation deviations (Track B)

- B1: vitest reads the contract fixtures through `web/src/test/backend.ts`
  (`import.meta.glob('../../../testdata/projects/*.json')`), a stateful fake of
  the §7.2 endpoints that the view tests share; error fixtures become
  `{error, code}` answers. `ApiError` carries `code`.
- Obsolete string keys go in the commit that stops using them
  (`TestUIStringsCoverPages`), not all in B7: the inbox/nav keys of §8 plus
  `page.title.inbox`, `inbox.new`, `inbox.new.area*`, `inbox.archive.title/empty`
  in B2; `settings.code.*` in B7.
- B3: the new-chat form became a modal (`NewChatPicker.vue`) already in B3, so
  «Новый чат» works from the project page and the sidebar; B4 adds the D5
  behaviour (everyone preselected, online state, count, ≥1).
- A project chat (`mode: "project"`) is named by its title in lists and the
  header, not by its participants (several chats may share the same people).
- D10: `/ui/inbox` opens the last used project, else the first one, else the
  welcome screen.
- D11: read cursors live in `localStorage` `agentlink.reads.v2:<node>` keyed
  `<pid>:<chat>`; the first time a project's chats arrive its history counts as
  read (marker key `<pid>:`). The sidebar badge counts unread chats.
- Join: a legacy code (or a project already here) opens directly, without the
  folder step. After a join that created the project the dialog cannot be
  dismissed by Esc/overlay: «Отмена» leaves, «Не ждать» closes it and keeps the
  project connecting, «Готово» binds the folder/alias given.
- Leave is confirmed in a modal (it shows the 409 `project_busy` sentence), not
  with the browser's confirm.
- Settings: the «Прежняя сеть» card (only with a legacy key) holds the working
  folder, the area folders («Папки тем») and «Общие темы» (moved out of
  «Дополнительно»). The page no longer sends `code`, so A14 (the server keeps
  `code`/`secret`) must land before B7.
- `styles.css` excludes the committed `dist/` from Tailwind's source scan: old
  built class names made rebuilds differ from run to run.
- Left for Track A: server sentences that still name the removed Settings
  button or «беседа» (`link.no_code`, `link.weak_code`, `tray.weak_code`,
  `error.chat_closed`, `error.unknown_chat`); `scripts/e2e-tray.ps1` still sets
  `settings.code`.
