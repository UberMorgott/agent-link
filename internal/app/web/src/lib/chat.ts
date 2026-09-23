// Pure chat helpers shared by the chat list, the open chat and the toasts.
import { fmt, t } from './runtime'
import type { AppSettings, ChatInfo, ChatMember, ChatMessage, Delivery, Job, Session } from '@/types'

// A message within GROUP_MS of the previous one by the same author continues it
// without repeating the author line.
export const GROUP_MS = 5 * 60 * 1000

export function others(info: ChatInfo | null | undefined, self: string): string[] {
  return (info?.participants || []).filter((name) => name !== self)
}

export function chatName(info: ChatInfo | null | undefined, self: string): string {
  if (!info) return ''
  if (info.legacy && info.peer) return info.peer
  // A project's own chat goes by its title, its first words.
  if (info.mode === 'project' && info.title) return info.title
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
export function authorLabel(m: ChatMessage, self: string): string { return isAgent(m) ? agentName(m.from, self) : authorName(m.from, self) }

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
  // A step that only names its type ("thinking") reads as the verb alone.
  if (verb && text && text.toLowerCase() !== info!.type!.toLowerCase()) return verb + ' ' + text
  return verb || text || t("inbox.activity.working")
}

export function workingLines(chat: ChatInfo, self: string): string[] {
  const out: string[] = []
  for (const m of chat.members || []) for (const job of m.jobs || []) out.push(agentName(m.name, self) + ' ' + activityText(job))
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
    return d.peer + ': ' + word + (state === 'read' && d.at ? ' ' + clock(d.at) : '')
  })
  return { state: lowest, label: delivery.length === 1 ? lines[0]! : lines.join('\n') }
}

// messageTick: an own message's delivery tick, or an incoming held one.
export function messageTick(m: ChatMessage, info: ChatInfo | null): Tick | null {
  if (m.direction === 'out') return ticksFor(m, info)
  return m.held ? { state: 'held', label: t("inbox.held") } : null
}

// continues: m follows prev by the same author and kind, soon after.
export function continues(prev: ChatMessage | undefined, m: ChatMessage): boolean {
  if (!prev || prev.kind || m.kind || prev.from !== m.from || isAgent(prev) !== isAgent(m) || m.reply_to) return false
  const gap = Date.parse(m.created_at) - Date.parse(prev.created_at)
  return gap >= 0 && gap < GROUP_MS
}

// --- live activity lines under the open chat ---

export interface ActivityLine {
  key: string
  cls: 'running' | 'queued' | 'stale' | 'waiting' | 'presence'
  name: string
  who: string
  text: string
  since: string
}

function jobStart(job: Job, messages: ChatMessage[]): string {
  const request = messages.find((m) => m.id === job.reply_to)
  return request?.created_at || job.activity_info?.started_at || job.updated_at || ''
}

// waitingLine: this computer's agent has unread messages of the chat but runs
// nothing for them — say why, so a silent chat is never a mystery.
export function waitingLine(info: ChatInfo | null, messages: ChatMessage[], self: string, sessions: Session[], settings: AppSettings | null) {
  if (!info || info.closed || info.legacy) return null
  const mine = (info.members || []).find((m) => m.self)
  if ((mine?.jobs || []).length) return null
  const pending = messages.filter((m) => m.unread && m.direction === 'in' && !m.kind)
  if (!pending.length) return null
  let key = ''
  if (sessions.length && sessions.every((s) => s.wake !== 'rewake')) key = "inbox.activity.waiting_session"
  else if (!sessions.length && !settings?.auto_answer) key = "inbox.activity.no_session"
  if (!key) return null
  return { name: self, text: t(key), since: pending[0]!.created_at }
}

// presenceLines: for every recipient of the last own message that has it but
// has not read it yet, what its node says of its session there — so the
// sender knows whether it is read at once or waits. Offline: the tick says it.
const PRESENCE_KEY: Record<string, string> = { rewake: "inbox.presence.rewake", 'next-event': "inbox.presence.next_event" }
export function presenceLines(info: ChatInfo | null, messages: ChatMessage[]): { name: string; text: string }[] {
  if (!info || info.closed || info.legacy) return []
  const last = [...messages].reverse().find((m) => m.direction === 'out' && !m.kind)
  if (!last) return []
  const out: { name: string; text: string }[] = []
  for (const d of last.delivery || []) {
    if (tickState(d) !== 'delivered') continue
    const member = (info.members || []).find((m) => !m.self && m.name === d.peer)
    const p = member?.connected ? member.presence : null
    if (!p || (member!.jobs || []).length) continue
    const key = PRESENCE_KEY[p.session || ''] || (p.auto_answer ? "inbox.presence.worker" : "inbox.presence.none")
    out.push({ name: d.peer, text: t(key) })
  }
  return out
}

export function activityLines(info: ChatInfo | null, messages: ChatMessage[], self: string, sessions: Session[], settings: AppSettings | null): ActivityLine[] {
  const rows: ActivityLine[] = []
  for (const member of info?.members || []) {
    for (const job of member.jobs || []) {
      rows.push({
        key: member.name + '\n' + job.reply_to,
        cls: job.stale ? 'stale' : job.job_status === 'queued' ? 'queued' : 'running',
        name: member.name, who: agentName(member.name, self), text: activityText(job), since: jobStart(job, messages),
      })
    }
  }
  const waiting = waitingLine(info, messages, self, sessions, settings)
  if (waiting) rows.push({ key: '\nwaiting', cls: 'waiting', name: waiting.name, who: agentName(waiting.name, self), text: waiting.text, since: waiting.since })
  for (const line of presenceLines(info, messages)) {
    rows.push({ key: '\npresence\n' + line.name, cls: 'presence', name: line.name, who: line.name + ':', text: line.text, since: '' })
  }
  return rows
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
