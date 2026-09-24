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
  members: MemberInfo[] // self first
  can_rename: boolean
  has_invite: boolean
  busy: boolean
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
