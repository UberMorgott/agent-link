// The tools of `agentlink mcp` (cmd/agentlink/mcp.go), served in-process:
// each is the same call of the node's local API the MCP server makes, and
// answers with the same JSON. An API error is a tool error carrying the API's
// message. The list itself (names, descriptions, schemas) is tools.gen.ts.
import type { Link } from '../types'

import type { Answer, NodeApi } from './node'
import { parse, seg } from './node'

/** Who calls: the session, and its subagent when one does. */
export type Caller = {
  link: Link
  /** The subagent's id and type, "" for the main agent. */
  agentId: string
  agentType: string
  /** The session id tools speak as: "" in a worker's job (not a session). */
  sessionId: string
  /** $AGENTLINK_SEAT and $AGENTLINK_JOB_ID of the process. */
  seat: string
  job: string
}

/** What a tool call answers: its JSON text, or the error the model reads. */
export type ToolAnswer = { text: string; sent?: { chat: string; id: string } } | { error: string }

type Args = Readonly<Record<string, unknown>>

/** A discuss waits this long for its answer by default, at most DISCUSS_MAX_MS. */
const DISCUSS_DEFAULT = '10m'
const DISCUSS_MAX_MS = 15 * 60_000
/** One held request of a discuss wait; the wait is as many as it takes. */
const DISCUSS_CHUNK_MS = 25_000
const PAGE = 50

const str = (v: unknown): string => (typeof v === 'string' ? v : '')
const list = (v: unknown): string[] =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? Math.trunc(v) : 0)
const isAbsolute = (p: string): boolean => /^([A-Za-z]:[\\/]|[\\/])/.test(p)

/** main.apiErrorText: an API error's message with its code after it. */
function errorOf(a: Answer, api: string): string {
  if (a.status === 0) return `agentlink: the local node (${api}) did not answer`
  try {
    const e: unknown = JSON.parse(a.text)
    if (typeof e === 'object' && e !== null && 'error' in e && typeof e.error === 'string' && e.error.trim() !== '') {
      const code = 'code' in e && typeof e.code === 'string' ? e.code : ''
      return code === '' ? e.error : `${e.error} (${code})`
    }
  } catch {
    // not JSON: the body as it is
  }
  const body = a.text.trim()
  return body === '' ? `agentlink: the local node answered ${a.status}` : body
}

/** A list answer as the MCP server gives it: the JSON, "[]" for none. */
function passed(a: Answer, api: string): ToolAnswer {
  if (!a.isOk) return { error: errorOf(a, api) }
  const text = a.text.trim()
  return { text: text === '' || text === 'null' ? '[]' : text }
}

/** main.withProject: the project selector, when there is one. */
const withProject = (project: string, q: Record<string, string> = {}): Record<string, string> =>
  project === '' ? q : { ...q, project }

/** main.inFolder: without a project the session's folder hints at one. */
const inFolder = (c: Caller, project: string, q: Record<string, string> = {}): Record<string, string> =>
  project === '' ? { ...q, cwd: c.link.folder } : { ...q, project }

/** A path as the node takes it: absolute, relative ones under the session's folder. */
const abs = (c: Caller, p: string): string => (isAbsolute(p) ? p : `${c.link.folder}/${p}`)

/** A Go duration ("10m", "1m30s", "90s") in milliseconds, or undefined. */
function duration(s: string): number | undefined {
  const units: Record<string, number> = { h: 3_600_000, m: 60_000, s: 1000, ms: 1 }
  let rest = s.trim()
  if (rest === '') return undefined
  let total = 0
  while (rest !== '') {
    const m = /^(\d+(?:\.\d+)?)(ms|h|m|s)/.exec(rest)
    if (m === null) return undefined
    total += Number(m[1]) * (units[m[2] ?? ''] ?? 0)
    rest = rest.slice(m[0].length)
  }
  return total
}

async function send(n: NodeApi, c: Caller, e: Args): Promise<ToolAnswer> {
  const chat = str(e.chat)
  const to = str(e.to)
  const fresh = list(e.new_chat_with)
  const body = str(e.body)
  const files = list(e.attachments)
  if ([chat !== '', to !== '', fresh.length > 0].filter(Boolean).length !== 1) {
    return { error: 'exactly one of chat, to, new_chat_with is required' }
  }
  if (body === '' && files.length === 0) return { error: 'body is required (or attachments)' }
  const project = c.link.project
  let chatId = chat
  if (fresh.length > 0) {
    const made = await n.call('POST', '/chats', inFolder(c, project), { participants: fresh, area: '' })
    const info = parse<{ id?: string }>(made)
    if (info?.id === undefined) return { error: errorOf(made, c.link.api) }
    chatId = info.id
  }
  const ask = list(e.ask)
  const askSeats = list(e.ask_seats)
  const res = await n.call('POST', '/send', withProject(project), {
    to,
    body,
    ...(str(e.reply_to) === '' ? {} : { reply_to: str(e.reply_to) }),
    ...(chatId === '' ? {} : { chat_id: chatId }),
    ...(ask.length === 0 ? {} : { ask }),
    ...(c.job === '' ? {} : { parent: c.job }),
    folder: c.link.folder,
    ...(c.sessionId === '' ? {} : { session_id: c.sessionId }),
    ...(c.agentId === '' || c.sessionId === '' ? {} : { agent_id: c.agentId, agent_type: c.agentType }),
    // The node reads the files itself: paths, never bytes.
    ...(files.length === 0 ? {} : { files: files.map(f => abs(c, f)) }),
    ...(askSeats.length === 0 ? {} : { ask_seats: askSeats }),
    ...(c.seat === '' ? {} : { seat: c.seat }),
  })
  const m = parse<{ id?: string; chat_id?: string; hold_reason?: string }>(res)
  if (m === undefined) return { error: errorOf(res, c.link.api) }
  const out: Record<string, string> = { id: m.id ?? '', chat: m.chat_id ?? '' }
  if (m.hold_reason !== undefined && m.hold_reason !== '') out.hold_reason = m.hold_reason
  return { text: JSON.stringify(out), sent: { chat: out.chat ?? '', id: out.id ?? '' } }
}

/** main.discussResult, the fields the node answers and this call adds. */
type Discussed = {
  project: string
  chat: string
  id: string
  seat: string
  reply?: Reply
  queued?: boolean
  timed_out?: boolean
  scope?: string
  topic?: string
  expires_at?: string
  held?: boolean
  hold_reason?: string
  note?: string
  seat_error?: string
  retry_at?: string
}

/** node.Message, the fields a compact discuss answer reads. */
type Reply = {
  id: string
  from: string
  body: string
  agent?: { label?: string; provider?: string; model?: string; effort?: string }
  attachments?: { path?: string }[]
}

/** main.compactDiscuss: the reply's text and who wrote it with what. */
function compact(r: Discussed): Record<string, unknown> {
  const out: Record<string, unknown> = { chat: r.chat }
  if (r.id !== '') out.id = r.id
  const m = r.reply
  if (m !== undefined) {
    if (m.body !== '') out.reply = m.body
    out.reply_id = m.id
    const who = m.agent === undefined ? m.from : m.agent.label || m.agent.provider || ''
    if (who !== '') out.from = who
    if (m.agent?.model) out.model = m.agent.model
    if (m.agent?.effort) out.effort = m.agent.effort
    const paths = (m.attachments ?? []).flatMap(a => (a.path ? [a.path] : []))
    if (paths.length > 0) out.attachments = paths
  }
  if (r.queued === true) out.queued = true
  if (r.timed_out === true) out.timed_out = true
  if (r.held === true) out.held = true
  if (r.hold_reason) out.hold_reason = r.hold_reason
  if (r.note) out.note = r.note
  if (r.seat_error) out.seat_error = r.seat_error
  if (r.retry_at) out.retry_at = r.retry_at
  return out
}

async function discuss(n: NodeApi, c: Caller, e: Args): Promise<ToolAnswer> {
  const provider = str(e.with)
  const body = str(e.body)
  if (provider !== 'claude' && provider !== 'codex') return { error: '--with must be claude or codex' }
  if (body.trim() === '') return { error: '--body is required' }
  const wait = duration(str(e.timeout) === '' ? DISCUSS_DEFAULT : str(e.timeout))
  if (wait === undefined || wait <= 0 || wait > DISCUSS_MAX_MS) {
    return { error: '--timeout must be greater than 0 and at most 15m0s' }
  }
  const asked = await n.call('POST', '/discuss', {}, {
    folder: str(e.folder) === '' ? c.link.folder : abs(c, str(e.folder)),
    provider,
    body,
    session_id: c.sessionId,
    source: c.sessionId === '' ? '' : 'claude',
    seat: c.seat,
    agent_id: c.sessionId === '' ? '' : c.agentId,
    agent_type: c.sessionId === '' ? '' : c.agentType,
    chat: str(e.chat),
    topic: str(e.topic),
    temporary: e.temporary === true,
    shared: e.shared === true,
  })
  const r = parse<Discussed>(asked)
  if (r === undefined) return { error: errorOf(asked, c.link.api) }
  const done = (): ToolAnswer => ({ text: JSON.stringify(e.full === true ? r : compact(r)) })
  if (e.async === true || r.queued === true || r.held === true || (r.hold_reason ?? '') !== '') return done()
  // The node holds each request for its timeout; the wait is as many requests
  // as it takes, each well inside what one held request may last.
  const deadline = (await n.now()) + wait
  // A node that answers each request at once holds nothing: the wait is over
  // after as many requests as it is long, whatever the clock says.
  let requests = Math.ceil(wait / DISCUSS_CHUNK_MS) + 2
  let failures = 0
  for (;;) {
    const left = deadline - (await n.now())
    if (left <= 0 || requests-- <= 0) {
      r.timed_out = true
      return done()
    }
    const res = await n.call('GET', '/discuss/reply', {
      project: r.project,
      chat: r.chat,
      id: r.id,
      seat: r.seat,
      timeout: `${Math.max(1, Math.ceil(Math.min(left, DISCUSS_CHUNK_MS) / 1000))}s`,
      client_ack: '1',
    })
    if (res.status === 202) {
      r.queued = true
      return done()
    }
    if (res.status === 204) continue // this request's time is up, the wait's is not
    if (res.status === 409) {
      const held = parse<{ error?: string; hold_reason?: string; seat_error?: string; retry_at?: string }>({ ...res, isOk: true })
      if (held?.hold_reason) {
        r.held = true
        r.hold_reason = held.hold_reason
        r.note = held.error || r.note
        if (held.seat_error) r.seat_error = held.seat_error
        if (held.retry_at) r.retry_at = held.retry_at
        return done()
      }
    }
    if (res.status === 0 && ++failures < 3) continue // a request cut short: the wait goes on
    const reply = parse<Reply>(res)
    if (reply === undefined) return { error: errorOf(res, c.link.api) }
    r.reply = reply
    // The answer is on its way to the caller: its reply is read now (a failed
    // ack leaves it for the next delivery).
    await n.call('POST', '/ack', withProject(r.project), { ids: [reply.id], session_id: c.sessionId })
    return done()
  }
}

/** Runs the tool `name` with the model's arguments `e`. */
export async function serve(n: NodeApi, c: Caller, name: string, e: Args): Promise<ToolAnswer> {
  const api = c.link.api
  const proj = (p: unknown): string => (str(p) === '' ? c.link.project : str(p))
  const limit = String(num(e.limit) === 0 ? PAGE : num(e.limit))
  switch (name) {
    case 'projects':
      return passed(await n.call('GET', '/projects', {}), api)
    case 'members':
      return passed(await n.call('GET', '/members', inFolder(c, proj(e.project))), api)
    case 'seats':
      return passed(await n.call('GET', '/seats', inFolder(c, proj(e.project))), api)
    case 'chats':
      return passed(
        await n.call('GET', '/chats', withProject(proj(e.project), {
          ...(e.archive === true ? { archive: '1' } : {}),
          ...(e.legacy === true ? { legacy: '1' } : {}),
        })),
        api,
      )
    case 'history': {
      if (str(e.chat) === '') return { error: 'chat is required' }
      const q = withProject(proj(e.project), { limit })
      if (num(e.before_seq) > 0) q.before = String(num(e.before_seq))
      if (num(e.after_seq) > 0) q.after = String(num(e.after_seq))
      return passed(await n.call('GET', `/chats/${seg(str(e.chat))}/messages`, q), api)
    }
    case 'unread': {
      const q = inFolder(c, proj(e.project), { limit })
      if (str(e.folder) !== '') q.folder = abs(c, str(e.folder))
      if (str(e.after) !== '') q.after = str(e.after)
      if (c.sessionId !== '') q.session = c.sessionId
      q.agent = '1'
      return passed(await n.call('GET', '/unread', q), api)
    }
    case 'ack': {
      const ids = list(e.ids)
      if (ids.length === 0) return { error: 'ids is required' }
      const path = str(e.chat) === '' ? '/ack' : `/chats/${seg(str(e.chat))}/ack`
      const session = str(e.session) === '' ? c.sessionId : str(e.session)
      return passed(
        await n.call('POST', path, withProject(proj(e.project)), {
          ids,
          ...(session === '' ? {} : { session_id: session }),
        }),
        api,
      )
    }
    case 'send':
      return send(n, c, e)
    case 'discuss':
      return discuss(n, c, e)
    default:
      return { error: `unknown tool ${name}` }
  }
}
