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
}

export interface Job {
  reply_to: string
  job_status?: string
  hold_reason?: string
  activity?: string
  activity_info?: ActivityInfo
  updated_at?: string
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
}

export interface ChatInfo {
  id: string
  participants?: string[]
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

export interface Session {
  session_id?: string
  provider?: string
  folder?: string
  area?: string
  wake?: string
}

export interface Project {
  dir?: string
}

export interface AppSettings {
  node?: string
  code?: string
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
