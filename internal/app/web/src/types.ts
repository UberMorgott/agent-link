// The shapes of /ui/api answers as the page reads them (internal/app, internal/node).

export interface Member {
  name: string
  self?: boolean
  online?: boolean
}

export interface Status {
  configured?: boolean
  running?: boolean
  connected?: boolean
  error?: string
  problem?: string
  warning?: string
  zerotier?: boolean
  online?: number
  total?: number
  node?: string
  listen?: string
  handler?: string
  members?: Member[]
  stop_all?: boolean // every project's agents are stopped (emergency stop)
  chat_color?: string // this member's own chat color ("" = derived from the name)
  nickname?: string // this member's nickname ("" = its name)
}

export interface DashboardSummary {
  status?: Status
  total_messages?: number
  active_requests?: number
  recent?: { peer?: string; preview?: string }[]
}

export interface ParticipantView {
  name: string
  online?: boolean
  seen?: string
  app?: string
  old_auth?: boolean
  legacy?: boolean
  addrs?: string[]
  sent?: number
  received?: number
  total?: number
}

export interface ActivityInfo {
  type?: string
  text?: string
  phase?: string
  started_at?: string
  // session: the reporting live session's short id (none for the worker).
  session?: string
  // Optional agent-tree fields (newer hooks): role "main" or "subagent"; a
  // subagent names its parent's full session id, its own agent id, and its
  // agent type as label.
  role?: string
  label?: string
  parent_session?: string
  agent_id?: string
}

export interface Job {
  reply_to: string
  job_status?: string
  hold_reason?: string
  activity?: string
  activity_info?: ActivityInfo
  updated_at?: string
  // heard_at: when this node last heard of the job, by its own clock.
  heard_at?: string
  stale?: boolean
}

export interface Presence {
  area?: string
  session?: string
  auto_answer?: boolean
}

export interface ChatMember {
  name: string
  self?: boolean
  connected?: boolean
  compatible?: boolean
  queued?: number
  jobs?: Job[]
  held?: Job[]
  presence?: Presence
}

export interface Delivery {
  peer: string
  status?: string
  state?: string
  at?: string
  // attempt: the latest delivery attempt event the recipient's node reported
  // (wake_requested, launch_requested, launch_failed:<reason>, needs_human…).
  attempt?: string
  attempts?: { id: string; event: string; at: string }[]
}

export interface ChatMessage {
  id: string
  seq: number
  kind?: string
  from: string
  direction?: string
  body?: string
  created_at: string
  reply_to?: string
  responders?: string[]
  held?: boolean
  job_status?: string
  delivery?: Delivery[]
  author_kind?: string
  own_human?: boolean
  unread?: boolean
  chat_id?: string
  participants?: string[] // on a chat_members message: the chat's new participants
  attachments?: Attachment[]
  // agent: the local agent of its node that wrote it (a seat: Claude, Codex…);
  // ask_seats: the seats of the author's node it asks.
  agent?: AgentRef
  ask_seats?: string[]
}

export interface AgentRef {
  seat?: string
  label?: string
  provider?: string
}

// SeatView is one local agent (seat) of this member in a project
// (GET projects/{pid}/seats): status active | idle | running | closed | stopped.
export interface SeatView {
  id: string
  provider: 'claude' | 'codex'
  label: string
  session_id?: string
  status: string
  error?: string
  pending?: { id: string; ask?: boolean }[]
}

// A file on a chat message: id is the sha256 of its content, mime its sniffed type.
export interface Attachment {
  id: string
  name: string
  mime: string
  size: number
  failed?: boolean // this node does not have the file (it did not arrive)
  key?: string // capability that opens the file at /ui/files (?k=)
}

export interface ChatInfo {
  id: string
  project?: string // project id or "legacy"
  mode?: 'project' // a standalone chat of a project
  participants?: string[]
  owner?: string // project chat: who made it, the only one who changes its participants
  removed?: boolean // project chat: the owner took this node out
  count?: number
  title?: string
  created_at?: string
  prev?: string // project chat: the chat whose history it took over (archived)
  closed?: boolean
  archived?: boolean
  legacy?: boolean
  peer?: string
  area?: string
  closed_by?: string
  closed_at?: string
  last_seq?: number
  last_at?: string
  last_message?: ChatMessage
  members?: ChatMember[]
}

// Session is a SessionView of GET sessions: an agent session with its context.
export interface Session {
  session_id?: string
  provider?: string
  folder?: string
  area?: string
  wake?: string
  ttl_sec?: number
  registered_at?: string
  last_seen?: string
  primary?: boolean
  project?: string // project id or "legacy"
}

// --- projects (docs/plans/projects-v1.md §7) ---

export type ProjectState = 'connecting' | 'needs_folder' | 'ready' | 'error'

export interface MemberInfo {
  name: string
  self?: boolean
  online: boolean
  addrs?: string[]
  seen?: string
  app?: string
  proto?: number
  legacy?: boolean
  old_auth?: boolean
  // agent: the member's computer has an agent session (Claude Code, Codex)
  // open in the project now; color: its own chat color ("" = derived).
  agent?: boolean
  color?: string
  display?: string // the member's nickname, shown instead of its name
}

export interface ProjectView {
  id: string // project id or "legacy"
  legacy: boolean
  name: string // shared name; "" while connecting
  alias: string
  display: string // alias || name || ""
  dir: string
  state: ProjectState
  problem: string // "" or unknown_project | wrong_project | auth | name_taken | removed
  online: number
  total: number
  // agents: computers, this one included, with an agent session open in the project.
  agents?: number
  members: MemberInfo[] // self first
  can_rename: boolean
  has_invite: boolean
  busy: boolean
  // autonomy: how far this member's agents work by themselves in the project;
  // absent for the legacy network. launch_mode: where an opened session opens.
  autonomy?: AutonomyView
  launch_mode?: 'desktop' | 'terminal'
}

export type AutonomyMode = 'off' | 'asked' | 'full'

// AutonomyView: the mode, the hop limit in effect (0: none; _default: it
// follows the mode), the budgets of full mode and what of them is used.
export interface AutonomyView {
  mode: AutonomyMode
  max_auto_depth: number
  max_auto_depth_default: boolean
  turns_per_hour: number
  max_run_minutes: number
  paused?: boolean
  pause_reason?: 'turns' | 'run'
  turns_last_hour: number
  run_minutes: number
}

// AutonomyRequest changes a project's autonomy (absent: kept); a negative
// max_auto_depth follows the mode again, 0 budgets their defaults.
export interface AutonomyRequest {
  autonomy?: AutonomyMode
  max_auto_depth?: number
  turns_per_hour?: number
  max_run_minutes?: number
}

export interface InviteView { invite: string }

export interface JoinResult {
  project: ProjectView
  created: boolean // false: the project was here already
}

// SentMessage is the answer of a send: the envelope as the node keeps it.
export interface SentMessage {
  id: string
  from: string
  body: string
  created_at: string
  chat_id?: string
  participants?: string[]
}

export interface Project {
  dir?: string
}

export interface AppSettings {
  node?: string
  handler?: string
  agent_path?: string
  work_dir?: string
  listen?: string
  api?: string
  areas?: string[]
  projects?: Record<string, Project>
  discovery?: boolean
  max_jobs?: number
  autostart?: boolean
  auto_answer?: boolean
}

export interface UpdateStatus {
  current?: string
  latest?: string
  text?: string
  failed?: boolean
  enabled?: boolean
  busy?: boolean
  available?: boolean
  auto?: boolean
  restarting?: boolean
  retry_at?: string
  // An install runs: bytes downloaded of size (absent: unknown) so far.
  installing?: boolean
  downloaded?: number
  size?: number
}

// One published release in the changelog (GET update/changelog).
export interface ReleaseNote {
  version: string
  name?: string
  body: string
  published?: string
}

export interface Changelog {
  current: string
  newer: boolean
  releases: ReleaseNote[]
  text?: string
  failed?: boolean
}

export interface HookStatus {
  client?: string
  work_dir?: string
  projects?: Record<string, string>
}

export interface SaveResult {
  saved?: boolean
  error?: string
  found?: string
  settings?: AppSettings
  status?: Status
  dashboard?: DashboardSummary
}
