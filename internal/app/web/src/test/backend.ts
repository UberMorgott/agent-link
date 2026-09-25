// A fake /ui/api built from the projects API contract fixtures
// (internal/app/testdata/projects, checked against the Go types by
// TestContractFixtures). Tests answer fetch with it (harness.ts); it keeps
// what a request changes, so a page sees its own edits.
import type { ChatInfo, ChatMessage, InviteView, JoinResult, ProjectView, SeatView, Session } from '@/types'

const files = import.meta.glob<unknown>('../../../testdata/projects/*.json', { eager: true, import: 'default' })

export class HttpError extends Error {
  status: number
  code: string
  constructor(status: number, message: string, code = '') {
    super(message)
    this.status = status
    this.code = code
  }
}

// fixture returns a fresh copy of one fixture by its file name without ".json".
export function fixture<T>(name: string): T {
  const key = Object.keys(files).find((path) => path.endsWith('/' + name + '.json'))
  if (!key) throw new Error('no fixture ' + name)
  return structuredClone(files[key]) as T
}

// apiError is the HttpError of an error fixture, e.g. "project_busy".
export function apiError(code: string): HttpError {
  const f = fixture<{ status: number; body: { error: string; code: string } }>('error_' + code)
  return new HttpError(f.status, f.body.error, f.body.code)
}

export const SITE = 'MFRGGZDFMZTWQ2LKNNWG23TPOA'
export const JOINING = 'NBSWY3DPEB3W64TMMQQGC3DUMU'
export const LEGACY_CODE = 'ABCD-EFGH-JKLM'
export const SELF = 'alice'

// world is a list of distinct projects as GET projects serves it: the ready
// site, a project still connecting, and the legacy network.
export function world(): ProjectView[] {
  const all = fixture<ProjectView[]>('projects')
  return [all[0]!, all[1]!, all[5]!]
}

export interface Backend {
  projects: ProjectView[]
  chats: Record<string, ChatInfo[]> // by project id
  messages: Record<string, ChatMessage[]> // by chat id
  sessions: Session[]
  seats: Record<string, SeatView[]> // by project id: the local agents
  sent: unknown[]
  // legacyNeedsDir: joining the legacy network answers 400 work_dir without
  // a dir (its code joined while an agent answers and no working folder is set).
  legacyNeedsDir: boolean
  // failNext: the next request answers this error fixture (e.g. "internal").
  failNext: string
  // changed names the event topics a request made stale.
  changed: (topics: string[]) => void
}

function legacyChat(): ChatInfo {
  const t0 = '2026-09-01T10:00:00Z'
  return {
    id: 'legacy-chat-1', project: 'legacy', participants: ['alice', 'bob'], title: 'Старый вопрос', closed: false, archived: false,
    count: 1, last_seq: 1, last_at: t0,
    last_message: { id: 'lm1', seq: 1, from: 'bob', direction: 'in', body: 'Привет из прежней сети', created_at: t0 },
    members: [{ name: 'alice', self: true, connected: true, compatible: true, queued: 0 }, { name: 'bob', connected: true, compatible: true, queued: 0 }],
  }
}

export function createBackend(): Backend {
  const [chat] = fixture<ChatInfo[]>('chats')
  const [archived] = fixture<ChatInfo[]>('chats_archive')
  const closedID = 'c0ffee00c0ffee00c0ffee00c0ffee00'
  return {
    projects: world(),
    chats: {
      [SITE]: [chat!, { ...archived!, id: closedID, title: 'Шрифты в шапке' }],
      [JOINING]: [],
      legacy: [legacyChat()],
    },
    messages: {
      [chat!.id]: fixture<ChatMessage[]>('chat_messages'),
      [closedID]: fixture<ChatMessage[]>('chat_messages').map((m) => ({ ...m, id: m.id + '-c', chat_id: closedID })),
      'legacy-chat-1': [legacyChat().last_message!],
    },
    sessions: fixture<Session[]>('sessions'),
    seats: {},
    sent: [],
    legacyNeedsDir: false,
    failNext: '',
    changed: () => {},
  }
}

let seq = 0
function newID(): string {
  seq++
  return (seq.toString(16) + '0'.repeat(32)).slice(0, 32)
}

function view(b: Backend, pid: string): ProjectView {
  const p = b.projects.find((x) => x.id === pid)
  if (!p) throw apiError('not_found')
  return p
}

function setView(b: Backend, next: ProjectView) {
  b.projects = b.projects.map((p) => (p.id === next.id ? next : p))
  b.changed(['projects', 'project:' + next.id])
  return next
}

function stateOf(p: ProjectView): ProjectView['state'] {
  if (p.problem) return 'error'
  if (!p.name) return 'connecting'
  return p.dir ? 'ready' : 'needs_folder'
}

function findChat(b: Backend, pid: string, id: string): ChatInfo {
  const found = (b.chats[pid] || []).find((c) => c.id === id)
  if (!found) throw apiError('unknown_chat')
  return found
}

function messagesPage(items: ChatMessage[], query: URLSearchParams): ChatMessage[] {
  const limit = Number(query.get('limit')) || 200
  if (query.get('after')) return items.filter((x) => x.seq > Number(query.get('after'))).slice(0, limit)
  const before = Number(query.get('before') || 0)
  const older = items.filter((x) => !before || x.seq < before)
  return older.slice(Math.max(0, older.length - limit))
}

// handle answers one request: method, path under /ui/api/ with its query, body.
export function handle(b: Backend, method: string, fullPath: string, body: unknown): unknown {
  const [path = '', search = ''] = fullPath.split('?')
  const query = new URLSearchParams(search)
  const req = (body || {}) as Record<string, unknown>
  const parts = path.split('/').map(decodeURIComponent)
  if (b.failNext) {
    const code = b.failNext
    b.failNext = ''
    throw apiError(code)
  }

  if (method === 'GET') {
    switch (path) {
      case 'status': return { configured: true, connected: true, zerotier: true, node: SELF, online: 1, total: 2 }
      case 'settings': return { node: SELF, handler: 'none', work_dir: 'C:\\work', discovery: true }
      case 'dashboard': return { status: { online: 1, total: 2, handler: 'none' }, total_messages: 3, active_requests: 1, recent: [] }
      case 'participants': return []
      case 'update': return { current: 'dev', enabled: false }
      case 'hooks': return {}
      case 'sessions': return b.sessions
      case 'projects': return b.projects
    }
  }
  if (method === 'POST' && (path === 'agent' || path === 'pick-folder')) return path === 'agent' ? { text: '' } : { path: 'C:\\work\\picked' }

  if (parts[0] !== 'projects') throw new HttpError(404, 'unexpected ' + method + ' ' + fullPath)

  if (method === 'POST' && parts.length === 1) {
    const name = String(req.name || '').trim()
    if (!name) throw apiError('name')
    const alias = String(req.alias || '').trim()
    const p: ProjectView = {
      ...fixture<ProjectView>('project_ready'), id: newID().toUpperCase().slice(0, 26), name, alias, display: alias || name,
      dir: String(req.dir || ''), online: 0, total: 0,
    }
    p.members = p.members.filter((m) => m.self)
    p.state = stateOf(p)
    b.projects = [...b.projects, p]
    b.chats[p.id] = []
    b.changed(['projects'])
    return p
  }

  if (method === 'POST' && parts[1] === 'join') {
    const invite = String(req.invite || '').trim()
    const addr = String(req.addr || '').trim()
    if (addr && !/^\d{1,3}(\.\d{1,3}){3}(:\d{1,5})?$/.test(addr)) throw apiError('addr')
    const dir = String(req.dir || '').trim()
    if (dir && !/^([A-Za-z]:[\\/]|\/)/.test(dir)) throw apiError('work_dir')
    // A known invite (or the legacy code set here) changes nothing, not even addr.
    const known = b.projects.find((p) => invite.includes('.' + p.id + '.'))
    if (known) return { project: known, created: false } satisfies JoinResult
    if (/^[A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4}$/i.test(invite)) {
      const legacy = b.projects.find((p) => p.legacy)
      if (legacy) {
        if (invite.toUpperCase() !== LEGACY_CODE) throw apiError('legacy_exists')
        return { project: legacy, created: false } satisfies JoinResult
      }
      if (b.legacyNeedsDir && !dir) throw apiError('work_dir')
      const p = { ...fixture<ProjectView>('project_legacy') }
      if (dir) p.dir = dir
      b.projects = [...b.projects, p]
      b.chats.legacy = []
      b.changed(['projects', 'project:legacy'])
      return { project: p, created: true } satisfies JoinResult
    }
    if (!invite.startsWith('ALP1.')) throw apiError('invite')
    const joined = fixture<JoinResult>('join_created')
    if (b.projects.some((p) => p.id === joined.project.id)) b.projects = b.projects.filter((p) => p.id !== joined.project.id)
    b.projects = [...b.projects, joined.project]
    b.chats[joined.project.id] = []
    b.changed(['projects'])
    return joined
  }

  const pid = parts[1]!
  const p = view(b, pid)
  const rest = parts.slice(2)

  if (rest.length === 0 && method === 'GET') return p
  if (method === 'POST' && rest[0] === 'name') {
    if (p.legacy) throw apiError('legacy_rename')
    const name = String(req.name || '').trim()
    if (!name) throw apiError('name')
    return setView(b, { ...p, name, display: p.alias || name, state: stateOf({ ...p, name }) })
  }
  if (method === 'POST' && rest[0] === 'binding') {
    if (p.busy && req.dir !== undefined && req.dir !== p.dir) throw apiError('project_busy')
    // The legacy network's binding sets its working folder; it has no alias.
    if (p.legacy && typeof req.alias === 'string' && req.alias.trim()) throw apiError('alias')
    const next = { ...p }
    if (typeof req.alias === 'string') next.alias = req.alias.trim()
    if (typeof req.dir === 'string') next.dir = req.dir.trim()
    if (typeof req.auto_open === 'boolean') next.auto_open = req.auto_open
    next.display = next.alias || next.name
    next.state = stateOf(next)
    return setView(b, next)
  }
  if (method === 'POST' && rest[0] === 'invite') {
    if (p.legacy) return { invite: LEGACY_CODE } satisfies InviteView
    return { ...fixture<InviteView>('invite'), invite: fixture<InviteView>('invite').invite.replace(SITE, pid) }
  }
  if (method === 'POST' && rest.join('/') === 'members/add') {
    if (!String(req.addr || '').trim()) throw apiError('addr')
    return p
  }
  if (method === 'POST' && rest.join('/') === 'members/remove') {
    const name = String(req.name || '').trim()
    if (!name) throw apiError('bad_request')
    if (name === SELF) throw apiError('remove_self')
    if (!p.members.some((m) => m.name === name)) throw apiError('unknown_member')
    const members = p.members.filter((m) => m.name !== name)
    const others = members.filter((m) => !m.self)
    return setView(b, { ...p, members, total: others.length, online: others.filter((m) => m.online).length })
  }
  if (method === 'POST' && rest[0] === 'leave') {
    if (p.busy) throw apiError('project_busy')
    b.projects = b.projects.filter((x) => x.id !== pid)
    delete b.chats[pid]
    b.changed(['projects'])
    return undefined
  }
  if (rest[0] === 'seats') {
    const list = b.seats[pid] || (b.seats[pid] = [])
    if (rest.length === 1 && method === 'GET') return list
    if (rest.length === 1 && method === 'POST') {
      const provider = String(req.provider || '')
      if (provider !== 'claude' && provider !== 'codex') throw apiError('bad_request')
      const base = provider === 'codex' ? 'Codex' : 'Claude'
      const taken = list.filter((s) => s.provider === provider).length
      const s: SeatView = { id: 'seat-' + newID().slice(0, 8), provider, label: taken ? base + ' ' + (taken + 1) : base, session_id: provider + '-' + (taken + 1), status: 'closed' }
      list.push(s)
      b.changed(['seats', 'project:' + pid])
      return s
    }
    const s = list.find((x) => x.id === rest[1])
    if (!s) throw apiError('not_found')
    if (rest[2] === 'start') s.status = 'closed'
    if (rest[2] === 'stop') s.status = 'stopped'
    if (rest[2] === 'remove') b.seats[pid] = list.filter((x) => x.id !== s.id)
    b.changed(['seats', 'project:' + pid])
    return rest[2] === 'remove' ? b.seats[pid] : s
  }
  if (method === 'POST' && rest[0] === 'send') {
    const text = String(req.body || '').trim()
    if (!text) throw apiError('empty_body')
    const info = findChat(b, pid, String(req.chat_id || ''))
    if (info.closed) throw apiError('chat_closed')
    b.sent.push(body)
    const items = b.messages[info.id] || (b.messages[info.id] = [])
    const at = new Date().toISOString()
    const m: ChatMessage = {
      id: newID(), seq: (info.last_seq || 0) + 1, from: SELF, direction: 'out', body: text, created_at: at, chat_id: info.id,
      reply_to: typeof req.reply_to === 'string' ? req.reply_to : undefined, author_kind: 'human',
      ask_seats: Array.isArray(req.ask_seats) ? req.ask_seats.map(String) : undefined,
      delivery: (info.participants || []).filter((n) => n !== SELF).map((peer) => ({ peer, status: 'queued', state: 'queued' })),
    }
    items.push(m)
    Object.assign(info, { last_seq: m.seq, last_at: at, last_message: m, count: (info.count || 0) + 1, title: info.title || text.slice(0, 60) })
    b.changed(['project:' + pid])
    return { id: m.id, from: SELF, to: '', body: text, created_at: at, chat_id: info.id, participants: info.participants }
  }
  if (rest[0] === 'chats') {
    if (rest.length === 1 && method === 'GET') return (b.chats[pid] || []).filter((c) => !!c.archived === (query.get('archive') === '1'))
    if (rest.length === 1 && method === 'POST') {
      const names = (Array.isArray(req.participants) ? req.participants : []).map(String)
      // Unknown participants (not members of the project) are refused as well.
      const members = new Set(p.members.map((m) => m.name))
      // A project chat may start with this node alone; the legacy network's needs another.
      if ((p.legacy && !names.some((n) => n !== SELF)) || names.some((n) => !members.has(n) && n !== SELF)) throw apiError('chat_participants')
      const participants = [...new Set([SELF, ...names])].sort()
      const online = new Set(p.members.filter((m) => m.online || m.self).map((m) => m.name))
      // A project has one active chat: asking for a new one returns it, with the
      // members asked for added.
      const active = p.legacy ? undefined : (b.chats[pid] || []).find((c) => !c.archived && !c.legacy)
      if (active) {
        const all = [...new Set([...(active.participants || []), ...participants])].sort()
        Object.assign(active, { participants: all, members: all.map((name) => ({ name, self: name === SELF, connected: online.has(name), compatible: true, queued: 0 })) })
        return active
      }
      const at = new Date().toISOString()
      const info: ChatInfo = {
        id: newID(), project: pid, mode: p.legacy ? undefined : 'project', owner: p.legacy ? undefined : SELF, participants, title: '', closed: false, archived: false,
        count: 0, last_seq: 0, last_at: at,
        members: participants.map((name) => ({ name, self: name === SELF, connected: online.has(name), compatible: true, queued: 0 })),
      }
      b.chats[pid] = [info, ...(b.chats[pid] || [])]
      b.messages[info.id] = []
      b.changed(['project:' + pid])
      return info
    }
    const info = findChat(b, pid, rest[1]!)
    if (rest.length === 2 && method === 'GET') return info
    if (rest[2] === 'messages' && method === 'GET') return messagesPage(b.messages[info.id] || [], query)
    if (rest[2] === 'members' && method === 'POST') {
      if (info.mode !== 'project') throw apiError('chat_participants')
      if (info.owner !== SELF) throw apiError('chat_owner')
      if (info.closed) throw apiError('chat_closed')
      const members = new Set(p.members.map((m) => m.name))
      const add = (Array.isArray(req.add) ? req.add : []).map(String)
      const remove = (Array.isArray(req.remove) ? req.remove : []).map(String)
      if (remove.includes(SELF) || add.some((n) => !members.has(n))) throw apiError('chat_participants')
      const participants = [...new Set([...(info.participants || []).filter((n) => !remove.includes(n)), ...add])].sort()
      const online = new Set(p.members.filter((m) => m.online || m.self).map((m) => m.name))
      Object.assign(info, {
        participants,
        members: participants.map((name) => ({ name, self: name === SELF, connected: online.has(name), compatible: true, queued: 0 })),
      })
      b.changed(['project:' + pid])
      return info
    }
    if (rest[2] === 'archive' && method === 'POST') {
      if (p.legacy) throw apiError('bad_request')
      const now = new Date().toISOString()
      const active = (b.chats[pid] || []).find((c) => !c.archived && !c.legacy)
      if (!active) throw apiError('unknown_chat')
      if (active.id !== info.id) return active
      Object.assign(info, { closed: true, archived: true, closed_by: SELF, closed_at: now })
      const fresh: ChatInfo = {
        ...info, id: newID(), prev: info.id, closed: false, archived: false, closed_by: undefined, closed_at: undefined,
        count: 0, last_seq: 0, last_at: now, created_at: now, last_message: undefined, title: '',
      }
      b.chats[pid] = [fresh, ...(b.chats[pid] || [])]
      b.messages[fresh.id] = []
      b.changed(['project:' + pid])
      return fresh
    }
    if (rest[2] === 'close' && method === 'POST') {
      Object.assign(info, { closed: true, archived: true, closed_by: SELF, closed_at: new Date().toISOString() })
      b.changed(['project:' + pid])
      return info
    }
  }
  throw new HttpError(404, 'unexpected ' + method + ' ' + fullPath)
}
