// agent-link for Claude Code as an in-process hooks module: what the shell
// hooks, their Stop-hook waiter and the `agentlink mcp` process do
// (cmd/agentlink/hook.go, hook_wait.go, hook_activity.go, hook_agents.go,
// mcp.go), done through the engine's `$` alone.
//
//   - session.start registers the session with the local node (POST /sessions,
//     native: the node keeps the shell hooks of the same session out) and the
//     nine tools of `agentlink mcp` (tools.ts).
//   - One loop asks the node for the session's unread messages, the request
//     held by the node until there are some (GET /unread?wait), and hands
//     them over:
//       idle     -> `$.prompt.submit`, a turn of its own (the wake)
//       mid-turn -> `$.session.append`, read by the running turn's next step
//     The person's own prompt carries what waits then (prompt.submit), a
//     subagent's tool call the replies to what it asked (tool.call).
//   - The node hears the acknowledgement (POST /ack) only when a model request
//     of the turn that carries the messages has started to answer (turn.step);
//     a turn that ended without one gives them back (POST /release).
//   - What the model reads is the node's text (POST /render): this module
//     words no message.
//   - Tool calls tell the node what the session does (POST /sessions/{id}/doing,
//     POST /chats/{id}/activity), named by the tool and worded by the node.
import type { EngineInterface, Register } from 'claude-code'

import type { Delivery, Link, LiveAgent, Turn } from '../types'

import type { Answer, NodeApi } from './node'
import { parse, seg } from './node'
import type { Caller } from './tools'
import { serve } from './tools'
import { TOOLS } from './tools.gen'

type Engine = EngineInterface

const LINK = { plugin: 'agent-link', key: 'link' } as const
const DELIVERIES = { plugin: 'agent-link', key: 'deliveries' } as const
const TURN = { plugin: 'agent-link', key: 'turn' } as const
const AGENTS = { plugin: 'agent-link', key: 'agents' } as const
const ACTIVE = { plugin: 'agent-link', key: 'active' } as const
const AGENT_CHATS = { plugin: 'agent-link', key: 'agentChats' } as const
const NOTES = { plugin: 'agent-link', key: 'notes' } as const

const TOOL_PREFIX = 'mcp__agent-link__'

/**
 * The poll: how often the node is asked when it does not hold the request (a
 * node before GET /unread?wait, or one that answered at once).
 */
const POLL_MS = 2000
/** How long the node holds one unread request (node.UnreadWaitMax is 50). */
const LONG_POLL_SEC = 25
/** A request answered sooner than this was not held: the next waits POLL_MS. */
const HELD_MS = 1000
/** cmd/agentlink hookHeartbeat and hookTTLClaude. */
const HEARTBEAT_MS = 60_000
const TTL_SEC = 900
/** A node that is down or refuses the folder is asked again this often. */
const RETRY_MS = 10_000
/** node.inboxWakeGrace: a wake no turn took lapses after it. */
const WAKE_GRACE_MS = 120_000
/** node.claimTTL is a minute: a turn's claim not yet acknowledged is taken again. */
const CLAIM_REFRESH_MS = 30_000
/** cmd/agentlink liveAgentTTL, maxLiveAgents, hookMinActivity, hookSameActivity. */
const AGENT_TTL_MS = 30 * 60_000
const MAX_AGENTS = 64
const MIN_ACTIVITY_MS = 300
const SAME_ACTIVITY_MS = 20_000
const PAGE = 50

/** node.UnreadMessage, the fields read here; the rest goes back to the node as it came. */
type Unread = { id: string; project?: string; wake_token?: string }
/** node.UnreadPage. */
type UnreadPage = { messages: Unread[]; total: number }
/** node.RenderResult. */
type Rendered = {
  text: string
  notice?: string
  shown: { id: string; project?: string }[]
  chats?: Record<string, string>
}
/** node.AckResult. */
type Acked = { id: string; assigned?: string; note?: string }
/** What one delivery hands over. */
type Taken = { text: string; notice: string; keys: string[]; chats: Record<string, string> }

/**
 * What the module holds while it runs. `$.state` keeps a copy (save*), read
 * back when the engine replaced this copy of the module (hydrate).
 */
const mem: {
  isHydrated: boolean
  link: Link | undefined
  deliveries: Delivery[]
  turn: Turn
  agents: Record<string, LiveAgent>
  active: Record<string, string>
  agentChats: Record<string, Record<string, string>>
  notes: string[]
} = {
  isHydrated: false,
  link: undefined,
  deliveries: [],
  turn: { id: '', step: -1 },
  agents: {},
  active: {},
  agentChats: {},
  notes: [],
}

let timer: { cancel: () => void } | undefined
let isPolling = false
let lastBeat = 0
let lastTry = 0
/** The session id a /clear just ended: never registered again. */
let endedId = ''
/** What the node was last told the main agent does, and the last activity posted. */
let lastDoing = ''
let lastActivity = { key: '', at: Number.NEGATIVE_INFINITY }
const lastAgentActivity = new Map<string, { key: string; at: number }>()
let job = ''
let seat = ''

/** settings.DefaultAPI: the desktop app's local API when nothing says else. */
const DEFAULT_API = '127.0.0.1:7520'

/** The node's local API: $AGENTLINK_API, else the desktop app's settings. */
async function findApi($: Engine): Promise<string> {
  const fromEnv = (await $.env.get('AGENTLINK_API'))?.trim()
  if (fromEnv !== undefined && fromEnv !== '') return fromEnv
  const appData = await $.env.get('APPDATA')
  const xdg = await $.env.get('XDG_CONFIG_HOME')
  const home = await $.env.get('HOME')
  const dir = appData ?? xdg ?? (home === undefined ? undefined : `${home}/.config`)
  if (dir === undefined) return DEFAULT_API
  try {
    // Only `api` is read; the file's other values never leave this function.
    const settings: unknown = JSON.parse(await $.fs.read(`${dir}/agentlink/config.json`))
    if (typeof settings === 'object' && settings !== null && 'api' in settings) {
      const api = settings.api
      if (typeof api === 'string' && api.trim() !== '') return api.trim()
    }
  } catch {
    // no settings file: the default
  }
  return DEFAULT_API
}

/** One call of the node's local API; never throws. */
async function call(
  $: Engine,
  api: string,
  method: string,
  path: string,
  query: Readonly<Record<string, string>>,
  body?: unknown,
): Promise<Answer> {
  const pairs = Object.entries(query).map(([k, v]) => `${k}=${encodeURIComponent(v)}`)
  const url = `http://${api}${path}${pairs.length > 0 ? `?${pairs.join('&')}` : ''}`
  try {
    const res = await $.http.fetch(url, {
      method,
      ...(body === undefined
        ? {}
        : { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }),
    })
    return { isOk: res.ok, status: res.status, text: res.text }
  } catch {
    return { isOk: false, status: 0, text: '' }
  }
}

/** The node, for the helpers outside this file (which never hold $). */
const nodeOf = ($: Engine, link: Link): NodeApi => ({
  api: link.api,
  call: (method, path, query, body) => call($, link.api, method, path, query, body),
  now: () => $.clock.now(),
})

/**
 * One hand-over at a time: the loop, the person's prompt and a subagent's tool
 * call may all see the same unread message at once, and only the first takes
 * it (the others find it among what was handed over).
 */
let handing: Promise<unknown> = Promise.resolve()
function exclusive<T>(fn: () => Promise<T>): Promise<T> {
  const run = handing.then(fn, fn)
  handing = run.catch(() => undefined)
  return run
}

const keyOf = (m: { id: string; project?: string }): string => `${m.project ?? ''}|${m.id}`
const idOf = (key: string): string => key.slice(key.indexOf('|') + 1)
const newToken = (now: number): string =>
  `${now.toString(36)}${Math.floor(Math.random() * 0xffffffff).toString(36).padStart(7, '0')}`

async function hydrate($: Engine): Promise<void> {
  if (mem.isHydrated) return
  mem.link = (await $.state.get(LINK)).value
  mem.deliveries = (await $.state.get(DELIVERIES)).value ?? []
  mem.turn = (await $.state.get(TURN)).value ?? { id: '', step: -1 }
  mem.agents = (await $.state.get(AGENTS)).value ?? {}
  mem.active = (await $.state.get(ACTIVE)).value ?? {}
  mem.agentChats = (await $.state.get(AGENT_CHATS)).value ?? {}
  mem.notes = (await $.state.get(NOTES)).value ?? []
  mem.isHydrated = true
}

const saveLink = async ($: Engine): Promise<void> => {
  if (mem.link !== undefined) await $.state.set(LINK, mem.link)
}
const saveDeliveries = ($: Engine): Promise<unknown> => $.state.set(DELIVERIES, mem.deliveries)
const saveTurn = ($: Engine): Promise<unknown> => $.state.set(TURN, mem.turn)
const saveAgents = ($: Engine): Promise<unknown> => $.state.set(AGENTS, mem.agents)
const saveActive = ($: Engine): Promise<unknown> => $.state.set(ACTIVE, mem.active)
const saveAgentChats = ($: Engine): Promise<unknown> => $.state.set(AGENT_CHATS, mem.agentChats)
const saveNotes = ($: Engine): Promise<unknown> => $.state.set(NOTES, mem.notes)

const withProject = (link: Link, q: Record<string, string> = {}): Record<string, string> =>
  link.project === '' ? q : { ...q, project: link.project }

function byProject(keys: readonly string[]): Map<string, string[]> {
  const groups = new Map<string, string[]>()
  for (const k of keys) {
    const project = k.slice(0, k.indexOf('|'))
    groups.set(project, [...(groups.get(project) ?? []), idOf(k)])
  }
  return groups
}

/** The live subagents' ids, sorted; the silent ones lapse here. */
function liveAgents(now: number): string[] {
  for (const [id, a] of Object.entries(mem.agents)) {
    if (now - a.at > AGENT_TTL_MS) delete mem.agents[id]
  }
  return Object.keys(mem.agents).sort()
}

/**
 * POST /sessions, as the shell hooks' SessionStart and their keep-alive do,
 * marked native: this module is the session's only way in.
 */
async function registerSession($: Engine, isHeartbeat: boolean): Promise<boolean> {
  const link = mem.link
  if (link === undefined || link.sessionId === '') return false
  const now = await $.clock.now()
  lastTry = now
  const res = await call($, link.api, 'POST', '/sessions', withProject(link), {
    session_id: link.sessionId,
    provider: 'claude',
    folder: link.folder,
    wake: 'rewake', // this module wakes the session; the node posts to no inbox
    ttl_sec: TTL_SEC,
    idle: mem.turn.id === '',
    native: true,
    ...(isHeartbeat ? { heartbeat: true } : { agents: liveAgents(now) }),
  })
  if (res.isOk) lastBeat = now
  if (res.isOk !== link.isRegistered) {
    link.isRegistered = res.isOk
    await saveLink($)
  }
  if (!isHeartbeat) lastDoing = '' // a turn began or ended: the node forgot what it did
  return res.isOk
}

/** The query of one unread listing, as the shell hooks ask it. */
function unreadQuery(link: Link, kind: 'wake' | 'turn' | 'agent', now: number, agent = '', wait = 0): Record<string, string> {
  const q = withProject(link, {
    folder: link.folder,
    session: link.sessionId,
    native: '1',
    limit: String(PAGE),
  })
  if (kind === 'wake') {
    q.actionable = '1'
    q.waiter = '1'
  }
  if (kind === 'agent') {
    q.for_agent = agent
  } else {
    const live = liveAgents(now)
    if (live.length > 0) q.skip_agents = live.join(',')
  }
  if (wait > 0) q.wait = String(wait)
  return q
}

/** POST /claim per project; the keys the node granted this session. */
async function claim($: Engine, link: Link, keys: readonly string[], wakeToken: string): Promise<string[]> {
  const granted: string[] = []
  for (const [project, ids] of byProject(keys)) {
    const res = await call($, link.api, 'POST', '/claim', project === '' ? withProject(link) : { project }, {
      ids,
      session_id: link.sessionId,
      folder: link.folder,
      native: true,
      ...(wakeToken === '' ? {} : { wake_token: wakeToken }),
    })
    for (const id of parse<string[]>(res) ?? []) granted.push(`${project}|${id}`)
  }
  return granted
}

/** POST /release: what no turn read is the node's to hand over again. */
async function release($: Engine, link: Link, keys: readonly string[]): Promise<void> {
  if (keys.length === 0) return
  await call($, link.api, 'POST', '/release', {}, { ids: keys.map(idOf), session_id: link.sessionId })
}

/** POST /ack per project; undefined when the node did not take every one. */
async function ack($: Engine, link: Link, keys: readonly string[]): Promise<Acked[] | undefined> {
  const out: Acked[] = []
  for (const [project, ids] of byProject(keys)) {
    const res = parse<Acked[]>(
      await call($, link.api, 'POST', '/ack', project === '' ? {} : { project }, { ids, session_id: link.sessionId }),
    )
    if (res === undefined) return undefined
    out.push(...res)
  }
  return out
}

/** POST /render: the node's words for a delivery. */
async function render($: Engine, link: Link, body: Record<string, unknown>): Promise<Rendered | undefined> {
  return parse<Rendered>(
    await call($, link.api, 'POST', '/render', {}, { folder: link.folder, session_id: link.sessionId, ...body }),
  )
}

/**
 * Claims what fits one text and has the node word it: the messages granted,
 * led by the notes that wait (and, in a session's first text, the CLI's path).
 */
async function take(
  $: Engine,
  link: Link,
  msgs: readonly Unread[],
  total: number,
  o: { mode: '' | 'wake'; token?: string; agent?: string; now: number },
): Promise<Taken | undefined> {
  const isMain = o.agent === undefined
  const notes = isMain && o.mode === '' ? mem.notes : []
  const isFirst = isMain && o.mode === '' && !link.isGreeted
  const words = (list: readonly Unread[], count: number): Promise<Rendered | undefined> =>
    render($, link, {
      messages: list,
      total: count,
      mode: o.mode,
      ...(o.token === undefined ? {} : { token: o.token }),
      ...(isMain ? { live_agents: liveAgents(o.now) } : { agent: o.agent }),
      ...(notes.length === 0 ? {} : { notes }),
      ...(isFirst ? { cli: true } : {}),
    })
  let r = await words(msgs, total)
  if (r === undefined) return undefined
  if (r.shown.length > 0) {
    const want = r.shown.map(keyOf)
    const granted = new Set(await claim($, link, want, o.token ?? ''))
    if (granted.size < want.length) {
      // Another session took some: only what was granted is put into words.
      r = await words(msgs.filter(m => granted.has(keyOf(m))), total - (want.length - granted.size))
      if (r === undefined) {
        await release($, link, [...granted])
        return undefined
      }
    }
  }
  if (r.text === '' || (o.mode === 'wake' && r.shown.length === 0)) return undefined
  if (notes.length > 0) {
    mem.notes = []
    await saveNotes($)
  }
  if (isFirst) {
    link.isGreeted = true
    await saveLink($)
  }
  return { text: r.text, notice: r.notice ?? '', keys: r.shown.map(keyOf), chats: r.chats ?? {} }
}

async function drop($: Engine, link: Link, token: string, isGivenBack: boolean): Promise<void> {
  const gone = mem.deliveries.filter(d => d.token === token)
  if (gone.length === 0) return
  mem.deliveries = mem.deliveries.filter(d => d.token !== token)
  await saveDeliveries($)
  if (isGivenBack) await release($, link, gone.flatMap(d => d.keys))
}

/** Submits a wake's prompt; a prompt that did not enter gives its messages back. */
async function submit($: Engine, link: Link, token: string, text: string): Promise<void> {
  try {
    const res = await $.prompt.submit({ text })
    if (res.drop === undefined) return
  } catch {
    // refused: given back below
  }
  await drop($, link, token, true)
}

/** Hands a batch to the idle session as a prompt of its own; false when it had none to hand. */
async function wake($: Engine, link: Link, msgs: readonly Unread[], total: number, now: number): Promise<boolean> {
  const token = newToken(now)
  const got = await take($, link, msgs, total, { mode: 'wake', token, now })
  if (got === undefined) return false
  mem.deliveries.push({ token, mode: 'wake', keys: got.keys, chats: got.chats, claim: 'wake', at: now, claimedAt: now })
  await saveDeliveries($)
  if (got.notice !== '') $.ui.log(got.notice)
  // Not awaited: it resolves when the turn starts, and the loop goes on.
  void submit($, link, token, got.text)
  return true
}

/**
 * Starts a turn for messages the conversation holds already (appended after a
 * turn's last model request): the prompt names none of them again.
 */
async function nudge($: Engine, link: Link, late: readonly Delivery[], now: number): Promise<void> {
  const keys = late.flatMap(d => d.keys)
  if (keys.length === 0) return
  const token = newToken(now)
  const words = await render($, link, { mode: 'nudge', token, total: keys.length })
  const tokens = new Set(late.map(d => d.token))
  mem.deliveries = mem.deliveries.filter(d => !tokens.has(d.token))
  if (words === undefined || words.text === '') {
    await saveDeliveries($)
    await release($, link, keys)
    return
  }
  mem.deliveries.push({
    token,
    mode: 'wake',
    keys,
    chats: Object.assign({}, ...late.map(d => d.chats)) as Record<string, string>,
    claim: 'turn',
    at: now,
    claimedAt: Math.min(...late.map(d => d.claimedAt)),
  })
  await saveDeliveries($)
  void submit($, link, token, words.text)
}

/** Appends a batch to the running turn: its next step reads it. */
async function append($: Engine, link: Link, msgs: readonly Unread[], total: number, now: number): Promise<boolean> {
  const got = await take($, link, msgs, total, { mode: '', now })
  if (got === undefined) return false
  const stored = await $.session.append({ message: { type: 'user', content: [{ type: 'text', text: got.text }] } })
  if (stored.deny !== undefined) {
    await release($, link, got.keys)
    return false
  }
  if (got.notice !== '') $.ui.log(got.notice)
  if (got.keys.length === 0) return true
  const d: Delivery = {
    token: newToken(now),
    mode: 'append',
    keys: got.keys,
    chats: got.chats,
    claim: 'turn',
    at: now,
    claimedAt: now,
    ...(mem.turn.id === '' ? {} : { turnId: mem.turn.id, afterStep: mem.turn.step }),
  }
  mem.deliveries.push(d)
  await saveDeliveries($)
  // The turn ended meanwhile: no request of it carries the row.
  if (mem.turn.id === '') await nudge($, link, [d], now)
  return true
}

/** Tells the chats a delivery came from what the main agent does now. */
async function report(
  $: Engine,
  link: Link,
  what: { event?: string; tool?: string; input?: unknown; phase?: string },
  isForced = false,
): Promise<void> {
  if (Object.keys(mem.active).length === 0) return
  const now = await $.clock.now()
  const key = JSON.stringify([what.event, what.tool, what.input])
  if (what.phase !== 'idle' && !isForced) {
    const since = now - lastActivity.at
    if (since < MIN_ACTIVITY_MS || (key === lastActivity.key && since < SAME_ACTIVITY_MS)) return
  }
  let isPosted = false
  let isChanged = false
  for (const [chat, replyTo] of Object.entries(mem.active)) {
    const res = await call($, link.api, 'POST', `/chats/${seg(chat)}/activity`, {}, {
      session_id: link.sessionId,
      reply_to: replyTo,
      ...what,
    })
    if (res.isOk) isPosted = true
    else if (res.status === 404 || res.status === 400) {
      delete mem.active[chat] // chat or session gone: nothing to report there
      isChanged = true
    }
  }
  if (isPosted) lastActivity = { key, at: now }
  if (isChanged) await saveActive($)
}

/** The chats subagent `id` works for: those the session worked on when it started. */
async function agentChats($: Engine, id: string): Promise<Record<string, string>> {
  const known = mem.agentChats[id]
  if (known !== undefined) return known
  if (Object.keys(mem.active).length === 0) return {}
  const chats = { ...mem.active }
  mem.agentChats[id] = chats
  await saveAgentChats($)
  return chats
}

/** Posts a subagent's activity to its chats; true when any post went through. */
async function reportAgent(
  $: Engine,
  link: Link,
  id: string,
  chats: Readonly<Record<string, string>>,
  what: { event?: string; tool?: string; input?: unknown; phase?: string },
): Promise<boolean> {
  const type = mem.agents[id]?.type ?? ''
  let isPosted = false
  for (const [chat, replyTo] of Object.entries(chats)) {
    const res = await call($, link.api, 'POST', `/chats/${seg(chat)}/activity`, {}, {
      session_id: link.sessionId,
      reply_to: replyTo,
      agent_id: id,
      ...(type === '' || type.length > 128 ? {} : { label: type }),
      ...what,
    })
    isPosted = isPosted || res.isOk
  }
  return isPosted
}

/** Ends the activity of every chat the session worked on: its turn is over. */
async function idle($: Engine, link: Link): Promise<void> {
  for (const [id, chats] of Object.entries(mem.agentChats)) {
    await reportAgent($, link, id, chats, { event: 'done', phase: 'idle' })
  }
  if (Object.keys(mem.agentChats).length > 0) {
    mem.agentChats = {}
    lastAgentActivity.clear()
    await saveAgentChats($)
  }
  if (Object.keys(mem.active).length === 0) return
  await report($, link, { event: 'done', phase: 'idle' })
  mem.active = {}
  lastActivity = { key: '', at: Number.NEGATIVE_INFINITY }
  await saveActive($)
}

/** Tells the node what the main agent does now; only a change is posted. */
async function tellDoing($: Engine, link: Link, what: { doing?: string; tool?: string; input?: unknown }): Promise<void> {
  const key = JSON.stringify([what.doing, what.tool, what.input])
  if (key === lastDoing) return
  const res = await call($, link.api, 'POST', `/sessions/${seg(link.sessionId)}/doing`, withProject(link), what)
  if (res.status !== 0) lastDoing = key // refused: not asked again until it changes
}

/** The part of a tool call's input the node words an activity from. */
function toolInput(e: Readonly<Record<string, unknown>>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const k of ['file_path', 'notebook_path', 'path', 'command']) {
    const v = e[k]
    if (typeof v === 'string') out[k] = v.slice(0, 4096)
  }
  return out
}

/** What a shell call tells of the agent's state: only whether it runs agentlink discuss. */
function doingInput(tool: string, input: Readonly<Record<string, string>>): Record<string, string> | undefined {
  if (tool !== 'Bash' && tool !== 'PowerShell') return undefined
  const command = input.command ?? ''
  return /agentlink\S*\s+discuss\b/i.test(command) ? { command } : undefined
}

/** Keeps the live subagents; true when the set of them changed. */
function trackAgent(id: string, type: string, now: number, isEnded: boolean): boolean {
  if (id === '' || id.length > 128) return false
  const before = liveAgents(now).join(',')
  if (isEnded) delete mem.agents[id]
  else if (mem.agents[id] !== undefined || Object.keys(mem.agents).length < MAX_AGENTS) {
    mem.agents[id] = { type: (type.length > 128 ? '' : type) || (mem.agents[id]?.type ?? ''), at: now }
  }
  return before !== liveAgents(now).join(',')
}

/** Acknowledges what a model request of `turnId` (its step `index`) carried. */
async function ackTurn($: Engine, turnId: string, index: number): Promise<void> {
  const link = mem.link
  if (link === undefined) return
  const carried = mem.deliveries.filter(
    d => d.turnId === turnId && (d.mode !== 'append' || index > (d.afterStep ?? -1)),
  )
  if (carried.length === 0) return
  const done = new Set<string>()
  const notes: string[] = []
  let isReading = false
  for (const d of carried) {
    const res = await ack($, link, d.keys)
    if (res === undefined) continue // still unread: the node hands them over again
    done.add(d.token)
    const chats = { ...d.chats }
    for (const r of res) {
      // The worker took it after the session was told about it.
      if (r.assigned !== 'worker' || r.note === undefined) continue
      for (const [chat, id] of Object.entries(chats)) {
        if (id === r.id) {
          delete chats[chat]
          notes.push(r.note)
        }
      }
    }
    if (Object.keys(chats).length > 0) {
      Object.assign(mem.active, chats)
      isReading = true
    }
  }
  mem.deliveries = mem.deliveries.filter(d => !done.has(d.token))
  await saveDeliveries($)
  if (isReading) {
    await saveActive($)
    await report($, link, { event: 'reading' }, true)
  }
  if (notes.length > 0) {
    mem.notes.push(...notes)
    await saveNotes($)
    await tellNotes($, link)
  }
}

/** The notes that wait reach the running turn as a row of their own. */
async function tellNotes($: Engine, link: Link): Promise<void> {
  if (mem.notes.length === 0 || mem.turn.id === '') return
  const words = await render($, link, { messages: [], notes: mem.notes })
  if (words === undefined || words.text === '') return
  const stored = await $.session.append({ message: { type: 'user', content: [{ type: 'text', text: words.text }] } })
  if (stored.deny !== undefined) return
  mem.notes = []
  await saveNotes($)
}

/**
 * What goes on into the session that follows a /clear: a wake whose prompt is
 * still on its way runs there, so its messages are that session's to read and
 * acknowledge (claimed again under its id at the next pass), not to be handed
 * over a second time.
 */
const carried = (): Delivery[] =>
  mem.deliveries
    .filter(d => d.mode === 'wake' && d.turnId === undefined)
    .map(d => ({ ...d, claim: 'turn' as const, claimedAt: Number.MIN_SAFE_INTEGER }))

/** The process goes on under another session id (/clear, a resume). */
async function rebind($: Engine, sessionId: string): Promise<void> {
  const link = mem.link
  if (link === undefined) return
  if (link.sessionId !== '' && link.sessionId !== sessionId) {
    await call($, link.api, 'DELETE', `/sessions/${seg(link.sessionId)}`, withProject(link))
  }
  mem.link = { ...link, sessionId, isRegistered: false, isGreeted: false }
  mem.deliveries = carried()
  mem.turn = { id: '', step: -1 }
  mem.agents = {}
  mem.active = {}
  mem.agentChats = {}
  mem.notes = []
  lastDoing = ''
  await saveLink($)
  await saveDeliveries($)
  await saveTurn($)
  await saveAgents($)
  await saveActive($)
  await saveAgentChats($)
  await saveNotes($)
  if (mem.link.isAttended) await registerSession($, false)
}

/** One pass: keep the session live, hand over what is new. True when the node held the request. */
async function poll($: Engine): Promise<boolean> {
  await hydrate($)
  let link = mem.link
  if (link === undefined || !link.isAttended) return false
  const started = await $.clock.now()
  const sessionId = await $.session.id()
  if (sessionId !== link.sessionId && sessionId !== endedId) {
    await rebind($, sessionId)
    link = mem.link
    if (link === undefined) return false
  }
  if (link.sessionId === '') return false
  if (link.isRegistered ? started - lastBeat >= HEARTBEAT_MS : started - lastTry >= RETRY_MS) {
    await registerSession($, link.isRegistered)
  }
  if (!link.isRegistered) {
    $.ui.status(undefined)
    return false
  }
  // What no turn took is the node's again; a turn's claim is kept alive.
  const lapsed = mem.deliveries.filter(d => d.turnId === undefined && started - d.at >= WAKE_GRACE_MS)
  for (const d of lapsed) await drop($, link, d.token, d.claim === 'turn')
  for (const d of mem.deliveries) {
    if (d.claim === 'turn' && started - d.claimedAt >= CLAIM_REFRESH_MS) {
      await claim($, link, d.keys, '')
      d.claimedAt = started
    }
  }
  const page = parse<UnreadPage>(
    await call($, link.api, 'GET', '/unread', unreadQuery(link, mem.turn.id === '' ? 'wake' : 'turn', started, '', LONG_POLL_SEC)),
  )
  const now = await $.clock.now()
  const isHeld = now - started >= HELD_MS
  // The session ended or moved on while the request was held.
  if (page === undefined || mem.link !== link || !link.isRegistered) return isHeld && page !== undefined
  const live = link
  await exclusive(async () => {
    const pending = new Set(mem.deliveries.flatMap(d => d.keys))
    const fresh = page.messages.filter(m => !pending.has(keyOf(m)))
    const total = Math.max(fresh.length, page.total - (page.messages.length - fresh.length))
    const count = total + pending.size
    $.ui.status(count > 0 ? `agent-link: непрочитанных — ${count}` : undefined)
    if (fresh.length === 0) return
    if (mem.turn.id === '') await wake($, live, fresh, total, now)
    else await append($, live, fresh, total, now)
  })
  // Asked again at once only after a request the node held: what is handed
  // over and comes straight back (a prompt some hook drops) waits for the poll.
  return isHeld
}

async function tick($: Engine): Promise<void> {
  if (isPolling) return
  isPolling = true
  let isAgain = false
  try {
    isAgain = await poll($)
  } catch {
    // a pass never breaks the session; the next one tries again
  } finally {
    isPolling = false
  }
  // A held request was answered: the next is asked at once, not after POLL_MS.
  if (isAgain && timer !== undefined) void tick($)
}

function startLoop($: Engine): void {
  timer?.cancel()
  timer = $.clock.every(POLL_MS, () => {
    void tick($)
  })
  void tick($)
}

/**
 * Whether a person is behind the session, as the shell hooks tell
 * (CLAUDE_CODE_SESSION_ATTENDED, else the engine's own word): a headless run
 * and a worker's job neither take messages nor are kept alive.
 */
async function attended($: Engine, isInteractive: boolean, hasSurface: boolean): Promise<boolean> {
  if (job !== '') return false
  if ((await $.env.get('AGENTLINK_NATIVE_HEADLESS')) === '1') return true // a test's or a proof's headless run
  const flag = await $.env.get('CLAUDE_CODE_SESSION_ATTENDED')
  if (flag !== undefined && flag !== '') return flag !== '0'
  return isInteractive || hasSurface
}

async function attend($: Engine): Promise<void> {
  const link = mem.link
  if (link === undefined) return
  link.isAttended = true
  await saveLink($)
  await registerSession($, false)
  startLoop($)
}

/** Who calls a tool: the session, and its subagent when one does. */
function caller(link: Link, agentId: string): Caller {
  return {
    link,
    agentId,
    agentType: mem.agents[agentId]?.type ?? '',
    sessionId: job === '' ? link.sessionId : '',
    seat,
    job,
  }
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await hydrate($)
    job = ((await $.env.get('AGENTLINK_JOB_ID')) ?? '').trim()
    seat = ((await $.env.get('AGENTLINK_SEAT')) ?? '').trim()
    const sessionId = await $.session.id()
    const before = mem.link
    // The engine replaced this copy of the module mid-session: the session,
    // its turn and what it was handed go on as `$.state` kept them.
    const isSame = before !== undefined && before.sessionId === sessionId
    mem.link = {
      api: await findApi($),
      project: ((await $.env.get('AGENTLINK_PROJECT_ID')) ?? '').trim(),
      sessionId,
      folder: e.cwd,
      isRegistered: false,
      isAttended: await attended($, e.isInteractive, e.surface !== null),
      isGreeted: isSame && before.isGreeted,
    }
    if (!isSame) {
      mem.deliveries = []
      mem.turn = { id: '', step: -1 }
      mem.agents = {}
      mem.active = {}
      mem.agentChats = {}
      mem.notes = []
      await saveDeliveries($)
      await saveTurn($)
      await saveAgents($)
      await saveActive($)
      await saveAgentChats($)
      await saveNotes($)
    }
    await saveLink($)
    for (const tool of TOOLS) {
      await $.tool.register({ name: tool.name, description: tool.description, inputSchema: tool.inputSchema })
    }
    if (mem.link.isAttended) {
      await registerSession($, false)
      startLoop($)
    }
    return next(e)
  })

  // A client drew the session after it started (the desktop app): attended.
  on('session.attach', async ($, e, next) => {
    await hydrate($)
    const link = mem.link
    if (link !== undefined && !link.isAttended && (await attended($, true, true))) await attend($)
    return next(e)
  })

  // A /clear ends the session and raises no session.start: the classic
  // SessionStart event names the id the process goes on under.
  on('classic.SessionStart', async ($, e, next) => {
    await hydrate($)
    const link = mem.link
    if (link !== undefined && e.session_id !== '' && e.session_id !== link.sessionId) {
      await rebind($, e.session_id)
      if (mem.link?.isAttended === true && timer === undefined) startLoop($)
    }
    return next(e)
  })

  // The person's own prompt carries what waits for the session.
  on('prompt.submit', async ($, e, next) => {
    await hydrate($)
    const link = mem.link
    if (link === undefined || !link.isAttended || !link.isRegistered || e.turnId !== undefined || mem.turn.id !== '') {
      return next(e)
    }
    const now = await $.clock.now()
    const page = parse<UnreadPage>(await call($, link.api, 'GET', '/unread', unreadQuery(link, 'turn', now)))
    if (page === undefined) return next(e)
    const got = await exclusive(async () => {
      const pending = new Set(mem.deliveries.flatMap(d => d.keys))
      const fresh = page.messages.filter(m => !pending.has(keyOf(m)))
      const taken = await take($, link, fresh, page.total - (page.messages.length - fresh.length), { mode: '', now })
      if (taken !== undefined && taken.keys.length > 0) {
        mem.deliveries.push({
          token: newToken(now),
          mode: 'prompt',
          keys: taken.keys,
          chats: taken.chats,
          claim: 'turn',
          at: now,
          claimedAt: now,
        })
        await saveDeliveries($)
      }
      return taken
    })
    if (got === undefined) return next(e)
    if (got.notice !== '') $.ui.log(got.notice)
    return next({ ...e, context: [...(e.context ?? []), got.text] })
  })

  on('turn.start', async ($, e, next) => {
    await hydrate($)
    mem.turn = { id: e.turnId, step: -1 }
    await saveTurn($)
    // A wake's turn is known by its token; what a prompt carried is its turn's.
    let isBound = false
    for (const d of mem.deliveries) {
      if (d.turnId !== undefined) continue
      if (d.mode === 'prompt' || (d.mode === 'wake' && e.text.includes(d.token))) {
        d.turnId = e.turnId
        isBound = true
      }
    }
    if (isBound) await saveDeliveries($)
    const link = mem.link
    if (link?.isAttended === true) {
      if (timer === undefined) startLoop($)
      await registerSession($, false)
      await report($, link, { event: 'thinking' })
    }
    return next(e)
  })

  on('turn.step', async function* ($, e, next) {
    const stream = next(e)
    if (e.agentId !== undefined) return yield* stream
    await hydrate($)
    if (mem.turn.id === e.turnId && mem.turn.step !== e.index) {
      mem.turn = { id: e.turnId, step: e.index }
      await saveTurn($)
    }
    let isAnswering = false
    for await (const chunk of stream) {
      // The model answers a request that carries the messages: read.
      if (!isAnswering) {
        isAnswering = true
        try {
          await ackTurn($, e.turnId, e.index)
        } catch {
          // still unread: the node hands them over again
        }
      }
      yield chunk
    }
    return await stream.result
  })

  on('turn.complete', async ($, e, next) => {
    if (e.agentId !== undefined) return next(e)
    await hydrate($)
    mem.turn = { id: '', step: -1 }
    await saveTurn($)
    const link = mem.link
    if (link === undefined || !link.isAttended) return next(e)
    const now = await $.clock.now()
    const late = mem.deliveries.filter(d => d.turnId === e.turnId)
    if (late.length > 0) {
      if (e.reason === 'answer') {
        // Appended after the turn's last request: the conversation holds them
        // and no request read them. The session is idle now: a turn for them.
        const appended = late.filter(d => d.mode === 'append')
        for (const d of late) {
          if (d.mode !== 'append') await drop($, link, d.token, true)
        }
        await nudge($, link, appended, now)
      } else {
        // The turn was interrupted, refused or failed before a model request
        // read them: given back, so the node hands them over again.
        for (const d of late) await drop($, link, d.token, true)
      }
    }
    await idle($, link)
    await registerSession($, false)
    return next(e)
  })

  on('classic.SubagentStart', async ($, e, next) => {
    await hydrate($)
    const link = mem.link
    if (link?.isAttended === true && link.isRegistered) {
      const now = await $.clock.now()
      if (trackAgent(e.agent_id, e.agent_type, now, false)) {
        await saveAgents($)
        await registerSession($, false) // the node hears the live ones at once
      }
      await reportAgent($, link, e.agent_id, await agentChats($, e.agent_id), { event: 'agent' })
    }
    return next(e)
  })

  on('classic.SubagentStop', async ($, e, next) => {
    await hydrate($)
    const link = mem.link
    if (link?.isAttended === true && link.isRegistered) {
      const now = await $.clock.now()
      const chats = mem.agentChats[e.agent_id] ?? {}
      await reportAgent($, link, e.agent_id, chats, { event: 'done', phase: 'idle' })
      delete mem.agentChats[e.agent_id]
      lastAgentActivity.delete(e.agent_id)
      await saveAgentChats($)
      if (trackAgent(e.agent_id, '', now, true)) {
        await saveAgents($)
        await registerSession($, false) // its replies are the main agent's from now on
      }
    }
    return next(e)
  })

  on('tool.call', async ($, e, next) => {
    await hydrate($)
    const link = mem.link
    const tool = String(e.tool)
    const agentId = e.agentId ?? ''
    const args = e as unknown as Readonly<Record<string, unknown>>
    const isOurs = tool.startsWith(TOOL_PREFIX)
    if (link === undefined) {
      return isOurs ? { deny: 'agentlink: this session has no local node' } : next(e)
    }
    const isLive = link.isAttended && link.isRegistered
    const input = toolInput(args)
    if (isLive) {
      const now = await $.clock.now()
      if (agentId === '') {
        const ask = doingInput(tool, input)
        await tellDoing($, link, { tool, ...(ask === undefined ? {} : { input: ask }) })
        await report($, link, { tool, input })
      } else {
        // A subagent's tool call: its own line under the session.
        if (trackAgent(agentId, '', now, false)) {
          await saveAgents($)
          await registerSession($, false)
        }
        const key = JSON.stringify([tool, input])
        const last = lastAgentActivity.get(agentId) ?? { key: '', at: Number.NEGATIVE_INFINITY }
        const since = now - last.at
        if (since >= MIN_ACTIVITY_MS && (key !== last.key || since >= SAME_ACTIVITY_MS)) {
          if (await reportAgent($, link, agentId, await agentChats($, agentId), { tool, input })) {
            lastAgentActivity.set(agentId, { key, at: now })
          }
        }
      }
    }
    if (isOurs) {
      const answer = await serve(nodeOf($, link), caller(link, agentId), tool.slice(TOOL_PREFIX.length), args)
      if (isLive && agentId === '') await tellDoing($, link, { doing: 'thinking' })
      if ('error' in answer) return { deny: answer.error }
      if (isLive && answer.sent !== undefined && answer.sent.chat !== '') {
        // Everyone in the chat sees what the agent that wrote does next.
        mem.active[answer.sent.chat] = answer.sent.id
        await saveActive($)
        await report($, link, { event: 'thinking' }, true)
      }
      return { result: answer.text }
    }
    const ran = await next(e)
    if (!isLive) return ran
    if (agentId === '') {
      await tellDoing($, link, { doing: 'thinking' }) // the tool is done: the model goes on
      return ran
    }
    // Only the replies to what this subagent asked reach it, after its tool calls.
    if (ran.deny !== undefined) return ran
    const got = await exclusive(async () => {
      const now = await $.clock.now()
      const page = parse<UnreadPage>(await call($, link.api, 'GET', '/unread', unreadQuery(link, 'agent', now, agentId)))
      if (page === undefined || page.messages.length === 0) return undefined
      const taken = await take($, link, page.messages, page.total, { mode: '', agent: agentId, now })
      if (taken !== undefined) await ack($, link, taken.keys) // read with this result
      return taken
    })
    if (got === undefined) return ran
    return { ...ran, context: [...(ran.context ?? []), got.text] }
  }).catch((_$, e, next) => next(e)) // this hook failing never holds a tool call back

  on('session.end', async ($, e, next) => {
    await hydrate($)
    const link = mem.link
    endedId = e.sessionId
    if (link !== undefined) {
      if (link.isAttended && link.isRegistered) await idle($, link)
      // The node hands back what the session was asked and did not answer.
      await call($, link.api, 'DELETE', `/sessions/${seg(e.sessionId)}`, withProject(link))
      mem.link = { ...link, sessionId: '', isRegistered: false, isGreeted: false }
      await saveLink($)
    }
    mem.deliveries = e.reason === 'clear' || e.reason === 'resume' ? carried() : []
    mem.turn = { id: '', step: -1 }
    mem.agents = {}
    mem.active = {}
    mem.agentChats = {}
    mem.notes = []
    await saveDeliveries($)
    await saveTurn($)
    await saveAgents($)
    await saveActive($)
    await saveAgentChats($)
    await saveNotes($)
    $.ui.status(undefined)
    // A /clear or a resume: the process goes on under another id, which the
    // loop registers (rebind); else the session is over.
    if (e.reason !== 'clear' && e.reason !== 'resume') {
      timer?.cancel()
      timer = undefined
    }
    return next(e)
  })
}
