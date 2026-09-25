// Pure chat helpers shared by the chat list, the open chat and the toasts.
import { fmt, t } from './runtime'
import type { AppSettings, ChatInfo, ChatMember, ChatMessage, Delivery, Job, Session } from '@/types'

// A message within GROUP_MS of the previous one by the same author continues it
// without repeating the author line.
export const GROUP_MS = 5 * 60 * 1000

export function others(info: ChatInfo | null | undefined, self: string): string[] {
  return (info?.participants || []).filter((name) => name !== self)
}

// chatName names a chat in lists and its header. A project has one chat, so
// its chat goes by the project's name (an archived one adds when it began);
// projectName is that name, empty for the network from before projects.
export function chatName(info: ChatInfo | null | undefined, self: string, projectName = ''): string {
  if (!info) return ''
  if (info.legacy && info.peer) return info.peer
  if (projectName && info.project !== 'legacy') {
    const began = info.archived ? clock(info.created_at) : ''
    return began ? projectName + ' · ' + began : projectName
  }
  const names = others(info, self)
  return names.length ? names.join(', ') : self
}

export function authorName(name: string, self: string): string { return name === self ? t("inbox.you") : name }
// genitiveName is authorName after "для"/"от": "вас" for this node.
export function genitiveName(name: string, self: string): string { return name === self ? t("inbox.you.gen") : name }

// A person and their agent write from the same node; author_kind tells them
// apart. A message of an old peer has no kind and counts as the person's.
export function isAgent(m: ChatMessage): boolean { return m.author_kind === 'agent' || m.author_kind === 'worker' }
export function agentName(name: string, self: string): string {
  return name === self ? t("inbox.author.own_agent") : fmt("inbox.author.agent", { name })
}
// A local agent (seat) names itself: "Morgott · Codex".
export function providerName(p: string | undefined): string { return p === 'codex' ? 'Codex' : p === 'claude' ? 'Claude' : p || '' }
export function authorLabel(m: ChatMessage, self: string): string {
  if (m.agent && (m.agent.label || m.agent.provider)) return m.from + ' · ' + (m.agent.label || providerName(m.agent.provider))
  return isAgent(m) ? agentName(m.from, self) : authorName(m.from, self)
}

// whoIndex gives every name one of six stable colours, so a group chat can be
// followed by colour as well as by name.
export function whoIndex(name: string): number {
  let hash = 0
  for (const ch of String(name)) hash = (hash * 31 + ch.codePointAt(0)!) >>> 0
  return (hash % 6) + 1
}
export function whoColor(name: string): string { return 'var(--who-' + whoIndex(name) + ')' }

function sameDay(a: Date, b: Date) { return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate() }
export function clock(iso: string | undefined): string {
  const at = new Date(iso || '')
  if (Number.isNaN(at.getTime())) return ''
  const time = at.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' })
  return sameDay(at, new Date()) ? time : at.toLocaleDateString('ru-RU', { day: '2-digit', month: '2-digit' }) + ' ' + time
}
export function when(iso: string): string { return new Date(iso).toLocaleString('ru-RU') }

// elapsed renders a duration like a CLI status line: 0:07, 3:41, 1:02:03.
export function elapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000))
  const h = Math.floor(total / 3600), m = Math.floor((total % 3600) / 60), s = total % 60
  const ss = String(s).padStart(2, '0')
  return h ? h + ':' + String(m).padStart(2, '0') + ':' + ss : m + ':' + ss
}

export function preview(text: string | undefined, limit: number): string {
  const chars = Array.from(String(text || '').replace(/\s+/g, ' ').trim())
  return chars.slice(0, limit).join('') + (chars.length > limit ? '…' : '')
}

// --- live activity text, shared by the chat list and the open chat ---

export function activityText(job: Job): string {
  if (job.stale) return t("inbox.activity.stale")
  if (job.job_status === 'queued') return t("inbox.activity.queued")
  const info = job.activity_info
  const text = info?.text || job.activity || ''
  const typeKey = info?.type ? "inbox.activity.type." + info.type : ''
  const verb = typeKey && t(typeKey) !== typeKey ? t(typeKey) : ''
  // A step that only names its type ("thinking") reads as the verb alone; a
  // text that already starts with a verb (session hooks send «думает»,
  // «правит x.go», and older ones «читает сообщения» as type thinking) is
  // shown as it is, never with a second verb.
  if (!verb || !text) return verb || text || t("inbox.activity.working")
  const lower = text.toLowerCase()
  if (lower === info!.type!.toLowerCase()) return verb
  const first = lower.split(/[\s:]/, 1)[0]!
  if (activityVerbs().includes(first)) return text
  return verb + ' ' + text
}

// The verbs an activity text can start with: every type's, and the ones the
// session hooks write («запускает go», «работает: X»).
const ACTIVITY_TYPES = ['thinking', 'edit', 'command', 'read', 'search', 'tool']
const HOOK_VERBS = ['думает', 'читает', 'правит', 'запускает', 'ищет', 'работает', 'готово']
function activityVerbs(): string[] {
  const out = [...HOOK_VERBS]
  for (const type of ACTIVITY_TYPES) {
    const key = "inbox.activity.type." + type, verb = t(key)
    if (verb !== key) out.push(verb.toLowerCase())
  }
  return out
}

// ACTIVITY_EXPIRE_MS matches the node's ActivityExpire: a running job not
// heard of for this long is over as far as anyone can tell.
export const ACTIVITY_EXPIRE_MS = 10 * 60 * 1000

// liveJobs drops the running jobs gone quiet past ACTIVITY_EXPIRE_MS by now,
// by heard_at (this computer's clock; updated_at is the author's, maybe off).
export function liveJobs(jobs: Job[] | undefined, now: number): Job[] {
  return (jobs || []).filter((job) => {
    if (job.job_status !== 'running') return true
    const heard = Date.parse(job.heard_at || '')
    return Number.isNaN(heard) || now - heard < ACTIVITY_EXPIRE_MS
  })
}

// --- agent tree: one main agent per session, its subagents under it ---

// CONCURRENT_MS: a member's session other than its latest one shows only while
// heard of this recently, so an older session's leftover jobs make no row.
export const CONCURRENT_MS = 2 * 60 * 1000

export interface SubAgent { key: string; label: string; job: Job }
// AgentGroup: one session of a member: its latest main job (none when only its
// subagents were heard of), its subagents (latest job each) and its last news.
export interface AgentGroup { session: string; job?: Job; subs: SubAgent[]; at: number }

function jobTime(job: Job): number {
  const at = Date.parse(job.heard_at || job.updated_at || '')
  return Number.isNaN(at) ? 0 : at
}
function newer(old: Job | undefined, job: Job): boolean { return !old || jobTime(job) >= jobTime(old) }

// agentTree folds a member's live jobs into its agents: the jobs of one session
// collapse into one main agent, a subagent (role "subagent") nests under its
// parent session by agent id. A job without a role is a main agent's. The
// latest session always shows; another only while it is concurrently active.
export function agentTree(jobs: Job[] | undefined, now: number): AgentGroup[] {
  const live = liveJobs(jobs, now)
  const groups = new Map<string, AgentGroup>()
  const group = (session: string) => {
    let g = groups.get(session)
    if (!g) groups.set(session, g = { session, subs: [], at: 0 })
    return g
  }
  for (const job of live) {
    if (job.activity_info?.role === 'subagent') continue
    const g = group(job.activity_info?.session || '')
    if (newer(g.job, job)) g.job = job
  }
  for (const job of live) {
    const info = job.activity_info
    if (info?.role !== 'subagent') continue
    // The parent's full session id starts with the short one its main reports.
    const parent = info.parent_session || ''
    const known = [...groups.keys()].find((s) => s && parent.startsWith(s))
    const g = group(info.session || known || parent)
    const key = info.agent_id || info.label || ''
    const entry = { key, label: info.label || '', job }
    const at = g.subs.findIndex((s) => s.key === key)
    if (at < 0) g.subs.push(entry)
    else if (newer(g.subs[at]!.job, job)) g.subs[at] = entry
  }
  for (const g of groups.values()) g.at = Math.max(g.job ? jobTime(g.job) : 0, ...g.subs.map((s) => jobTime(s.job)))
  const all = [...groups.values()].sort((a, b) => b.at - a.at)
  return all.filter((g, i) => i === 0 || concurrent(g, now))
}

function concurrent(g: AgentGroup, now: number): boolean {
  if (g.job?.job_status === 'queued') return true
  if (g.job?.stale && !g.subs.length) return false
  return !g.at || now - g.at < CONCURRENT_MS
}

// groupAgent names a main agent: with its session when the member shows more
// than one, and with its label when its hook gives one.
function groupAgent(name: string, self: string, g: AgentGroup, several: boolean): string {
  const notes = [several ? g.session : '', g.job?.activity_info?.label || ''].filter(Boolean)
  return agentName(name, self) + (notes.length ? ' (' + notes.join(', ') + ')' : '')
}
function groupText(g: AgentGroup): string { return g.job ? activityText(g.job) : t("inbox.activity.working") }

export function workingLines(chat: ChatInfo, self: string): string[] {
  const out: string[] = []
  for (const m of chat.members || []) {
    const groups = agentTree(m.jobs, Date.now())
    for (const g of groups) out.push(groupAgent(m.name, self, g, groups.length > 1) + ' ' + groupText(g))
  }
  return out
}

export type MemberState = 'on' | 'old' | 'away'
export function memberState(member: ChatMember): MemberState {
  return member.self || member.connected ? (member.compatible ? 'on' : 'old') : 'away'
}

// localArea is the session area a chat's messages reach on this computer: an
// area without a project folder here goes to the working folder ("").
export function localArea(area: string, settings: AppSettings | null): string {
  const projects = settings?.projects || {}
  return area && Object.prototype.hasOwnProperty.call(projects, area) ? area : ''
}

// chatSessionList: the agent sessions on this computer a chat's messages reach,
// those of its project and, in the legacy network, of its area.
export function chatSessionList(info: ChatInfo | null, sessions: Session[] | null, settings: AppSettings | null): Session[] {
  const project = info?.project || ''
  const area = project === 'legacy' ? localArea(info?.area || '', settings) : ''
  return (Array.isArray(sessions) ? sessions : []).filter((s) => (s.project || '') === project && (s.area || '') === area)
}

// --- delivery ticks ---

export type TickState = 'queued' | 'delivered' | 'read' | 'held'
export interface Tick { state: TickState; label: string }

// The recipient's progress; answered reads as read (the reply itself shows).
const TICK_RANK: Record<string, number> = { queued: 0, delivered: 1, read: 2 }
export function tickState(d: Delivery): 'queued' | 'delivered' | 'read' {
  if (d.state === 'answered' || d.state === 'read') return 'read'
  if (d.state === 'delivered' || d.state === 'queued') return d.state
  return d.status === 'sent' ? 'delivered' : 'queued'
}

// attemptText: what the recipient's node reported doing to get an unread
// message seen (its latest delivery attempt), '' when nothing or once read.
export function attemptText(d: Delivery): string {
  if (!d.attempt || tickState(d) === 'read') return ''
  const [event, reason] = d.attempt.split(':', 2)
  if (event === 'launch_failed') return fmt("inbox.attempt.launch_failed", { reason: reason || '?' })
  return ATTEMPTS.includes(event!) ? t("inbox.attempt." + event) : ''
}
const ATTEMPTS = ['wake_requested', 'woken_confirmed', 'launch_requested', 'launch_confirmed', 'needs_human']

// attemptFailed: the recipient's node gave up getting the message seen
// (it could not open a session, or the chain is paused): a person must act.
export function attemptFailed(d: Delivery): boolean {
  return tickState(d) !== 'read' && !!d.attempt && (d.attempt.startsWith('launch_failed:') || d.attempt === 'needs_human')
}

// holdsFor lists the participants that were asked message id but will not
// answer it automatically, with why (their node's text); such a hold is idle.
export function holdsFor(info: ChatInfo | null, id: string): { name: string; text: string }[] {
  const out: { name: string; text: string }[] = []
  for (const member of info?.members || []) {
    for (const job of member.held || []) if (job.reply_to === id) out.push({ name: member.name, text: job.activity || t("inbox.hold.unknown") })
  }
  return out
}

// ticksFor is the tick of an own message: the lowest state across its
// recipients, or a hold «!» with the reason. Null when there is nothing to show.
export function ticksFor(m: ChatMessage, info: ChatInfo | null): Tick | null {
  if (m.direction !== 'out' || m.kind) return null
  const holds = holdsFor(info, m.id)
  const failed = m.job_status === 'failed' && !!m.reply_to
  if (m.held || holds.length || failed) {
    const lines = holds.map((h) => h.name + ': ' + h.text)
    if (m.held) lines.unshift(t("inbox.held"))
    if (failed) lines.unshift(t("inbox.failed"))
    return { state: 'held', label: lines.join('\n') }
  }
  const delivery = m.delivery || []
  if (!delivery.length) return null
  let lowest: 'queued' | 'delivered' | 'read' = 'read'
  const lines = delivery.map((d) => {
    const state = tickState(d)
    if (TICK_RANK[state]! < TICK_RANK[lowest]!) lowest = state
    const word = t("inbox.tick." + (d.state === 'answered' ? 'answered' : state))
    const attempt = attemptText(d)
    return d.peer + ': ' + word + (state === 'read' && d.at ? ' ' + clock(d.at) : '') + (attempt ? ' — ' + attempt : '')
  })
  // A recipient whose node gave up shows «!», not a quiet delivered tick.
  const state: TickState = delivery.some(attemptFailed) ? 'held' : lowest
  return { state, label: delivery.length === 1 ? lines[0]! : lines.join('\n') }
}

// messageTick: an own message's delivery tick, or an incoming held one.
export function messageTick(m: ChatMessage, info: ChatInfo | null): Tick | null {
  if (m.direction === 'out') return ticksFor(m, info)
  return m.held ? { state: 'held', label: t("inbox.held") } : null
}

// continues: m follows prev by the same author and kind, soon after.
export function continues(prev: ChatMessage | undefined, m: ChatMessage): boolean {
  if (!prev || prev.kind || m.kind || prev.from !== m.from || isAgent(prev) !== isAgent(m) || m.reply_to) return false
  if ((prev.agent?.seat || prev.agent?.provider || '') !== (m.agent?.seat || m.agent?.provider || '')) return false
  const gap = Date.parse(m.created_at) - Date.parse(prev.created_at)
  return gap >= 0 && gap < GROUP_MS
}

// --- live activity lines under the open chat ---

export interface ActivityLine {
  key: string
  cls: 'running' | 'queued' | 'stale' | 'waiting' | 'presence' | 'idle'
  name: string
  who: string
  text: string
  // since: when the job's request (or the wait) began; heard: the job's last
  // news (running and stale lines), which is what their time shows.
  since: string
  heard?: string
  // children: a main agent's subagents, one line each.
  children?: ActivityLine[]
}

function jobStart(job: Job, messages: ChatMessage[]): string {
  const request = messages.find((m) => m.id === job.reply_to)
  return request?.created_at || job.activity_info?.started_at || job.updated_at || ''
}

// waitingLine: this computer's agent has unread messages of the chat but runs
// nothing for them — say why, so a silent chat is never a mystery.
export function waitingLine(info: ChatInfo | null, messages: ChatMessage[], self: string, sessions: Session[], settings: AppSettings | null, now = Date.now()) {
  if (!info || info.closed || info.legacy) return null
  const mine = (info.members || []).find((m) => m.self)
  if (liveJobs(mine?.jobs, now).length) return null
  const pending = messages.filter((m) => m.unread && m.direction === 'in' && !m.kind)
  if (!pending.length) return null
  let key = ''
  if (sessions.length && sessions.every((s) => s.wake !== 'rewake' && s.wake !== 'queue')) key = "inbox.activity.waiting_session"
  else if (!sessions.length && !settings?.auto_answer) key = "inbox.activity.no_session"
  if (!key) return null
  return { name: self, text: t(key), since: pending[0]!.created_at }
}

// presenceLines: for every recipient of the last own message that has it but
// has not read it yet, what its node says of its session there — so the
// sender knows whether it is read at once or waits. Offline: the tick says it.
const PRESENCE_KEY: Record<string, string> = { rewake: "inbox.presence.rewake", queue: "inbox.presence.queue", 'next-event': "inbox.presence.next_event" }
export function presenceLines(info: ChatInfo | null, messages: ChatMessage[], now = Date.now()): { name: string; text: string }[] {
  if (!info || info.closed || info.legacy) return []
  const last = [...messages].reverse().find((m) => m.direction === 'out' && !m.kind)
  if (!last) return []
  const out: { name: string; text: string }[] = []
  for (const d of last.delivery || []) {
    if (tickState(d) !== 'delivered') continue
    const member = (info.members || []).find((m) => !m.self && m.name === d.peer)
    if (member && liveJobs(member.jobs, now).length) continue
    // What its node reported doing about the message says more than presence.
    const attempt = attemptText(d)
    if (attempt) {
      out.push({ name: d.peer, text: attempt })
      continue
    }
    const p = member?.connected ? member.presence : null
    if (!p) continue
    const key = PRESENCE_KEY[p.session || ''] || (p.auto_answer ? "inbox.presence.worker" : "inbox.presence.none")
    out.push({ name: d.peer, text: t(key) })
  }
  return out
}

export function activityLines(info: ChatInfo | null, messages: ChatMessage[], self: string, sessions: Session[], settings: AppSettings | null, now = Date.now()): ActivityLine[] {
  const rows: ActivityLine[] = []
  const line = (job: Job, key: string, name: string, who: string, text: string): ActivityLine => {
    const queued = job.job_status === 'queued'
    return {
      key, cls: job.stale ? 'stale' : queued ? 'queued' : 'running', name, who, text, since: jobStart(job, messages),
      heard: queued ? undefined : job.heard_at || job.updated_at,
    }
  }
  for (const member of info?.members || []) {
    const groups = agentTree(member.jobs, now)
    for (const g of groups) {
      const key = member.name + '\n' + g.session
      // A session heard of only through its subagents times by the latest one.
      const lead = g.job || [...g.subs].sort((a, b) => jobTime(b.job) - jobTime(a.job))[0]!.job
      const row = line(lead, key, member.name, groupAgent(member.name, self, g, groups.length > 1), groupText(g))
      if (g.subs.length) {
        row.children = g.subs.map((s) => line(s.job, key + '\n' + s.key, member.name, s.label || t("inbox.activity.subagent"), activityText(s.job)))
      }
      rows.push(row)
    }
  }
  const waiting = waitingLine(info, messages, self, sessions, settings, now)
  if (waiting) rows.push({ key: '\nwaiting', cls: 'waiting', name: waiting.name, who: agentName(waiting.name, self), text: waiting.text, since: waiting.since })
  for (const line of presenceLines(info, messages, now)) {
    rows.push({ key: '\npresence\n' + line.name, cls: 'presence', name: line.name, who: line.name + ':', text: line.text, since: '' })
  }
  return rows
}

// keepLastKnown: a member still connected whose agent has no line now keeps
// its last running line, idle, as «последнее: …» with its age, so a finished
// job's row never just vanishes. memory is the view's own, across refreshes.
export function keepLastKnown(rows: ActivityLine[], info: ChatInfo | null, memory: Map<string, ActivityLine>): ActivityLine[] {
  const idle: ActivityLine[] = []
  for (const member of info?.members || []) {
    const key = info!.id + '\n' + member.name
    if (memberState(member) === 'away') { memory.delete(key); continue }
    const mine = rows.filter((r) => r.name === member.name && (r.cls === 'running' || r.cls === 'queued' || r.cls === 'stale'))
    const running = mine.find((r) => r.cls === 'running')
    if (running) { memory.set(key, running); continue }
    const last = memory.get(key)
    if (!last || mine.length) continue
    idle.push({
      key: last.key + '\nidle', cls: 'idle', name: last.name, who: last.who,
      text: fmt("inbox.activity.last", { text: last.text }), since: last.since, heard: last.heard,
    })
  }
  const at = rows.findIndex((r) => r.cls === 'waiting' || r.cls === 'presence')
  const out = [...rows]
  out.splice(at < 0 ? out.length : at, 0, ...idle)
  return out
}

// legacyPeerOld: the legacy chat's peer is connected without chats, so it
// cannot take part in closing it either.
export function legacyPeerOld(info: ChatInfo): boolean {
  return (info.members || []).some((m) => !m.self && m.connected && !m.compatible)
}

// unread: the chat's last message came in after the read cursor, and the chat
// is not the one on screen.
export function isUnread(chat: ChatInfo, open: boolean, read: number): boolean {
  return Boolean(chat.last_message && chat.last_message.direction === 'in' && !open && (chat.last_seq || 0) > read)
}
