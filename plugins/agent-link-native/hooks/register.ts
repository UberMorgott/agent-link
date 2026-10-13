// SPIKE: agent-link's Claude Code integration as an in-process hooks module.
//
// What the classic integration does with shell hooks, a Stop-hook waiter and
// the `agentlink mcp` process (cmd/agentlink/hook.go, hook_wait.go, mcp.go),
// done here through `$` alone:
//   - session.start registers the session with the local node (POST /sessions)
//   - a `$.clock.every` loop asks the node for this session's unread messages
//     as its waiter (GET /unread?waiter=1) and hands them over:
//       idle     -> `$.prompt.submit` (a turn of its own: the wake)
//       mid-turn -> `$.session.append` (the running turn's next step reads it)
//   - the node hears the acknowledgement (POST /ack) only when a model request
//     of the turn that carries the messages has started to answer (turn.step)
//   - the tools `unread` and `send` are served by `tool.call` hooks
import type { EngineInterface, Register } from 'claude-code'

import type { Delivery, Link } from '../types'

const LINK = { plugin: 'agent-link-native', key: 'link' } as const
const DELIVERIES = { plugin: 'agent-link-native', key: 'deliveries' } as const
const TURN = { plugin: 'agent-link-native', key: 'turn' } as const

const TOOL_UNREAD = 'mcp__agent-link-native__unread'
const TOOL_SEND = 'mcp__agent-link-native__send'

/** settings.DefaultAPI: the desktop app's local API when nothing says else. */
const DEFAULT_API = '127.0.0.1:7520'
const POLL_MS = 2000
/** cmd/agentlink hookHeartbeat. */
const HEARTBEAT_MS = 60_000
/** cmd/agentlink hookTTLClaude. */
const TTL_SEC = 900
/** node.inboxWakeGrace: a wake claim nobody proved lapses after it. */
const DELIVERY_GRACE_MS = 120_000
/** node.FormatBudget and node.FormatMaxBody, in characters. */
const FORMAT_BUDGET = 4500
const FORMAT_MAX_BODY = 2500
const PAGE = 20

type Engine = EngineInterface

/** node.UnreadMessage, the fields read here. */
type Unread = {
  id: string
  from: string
  body: string
  created_at?: string
  chat_id?: string
  participants?: string[]
  author_kind?: string
  own_human?: boolean
  asks_you?: boolean
  paused?: boolean
  assigned?: string
  project?: string
}

/** node.UnreadPage. */
type UnreadPage = { messages: Unread[]; total: number }

type Answer = { isOk: boolean; status: number; text: string }

let isPolling = false
let lastBeat = 0
/** The index of the main loop's step in flight, -1 between turns. */
let stepIndex = -1
let timer: { cancel: () => void } | undefined

const marker = (token: string): string => `[agent-link wake ${token}]`
const keyOf = (m: Unread): string => `${m.project ?? ''}|${m.id}`
const newToken = (now: number): string =>
  `${now.toString(36)}${Math.floor(Math.random() * 0xffffffff).toString(36)}`

/** The node's local API: $AGENTLINK_API, else the desktop app's settings. */
async function findApi($: Engine): Promise<string> {
  const fromEnv = (await $.env.get('AGENTLINK_API'))?.trim()
  if (fromEnv) return fromEnv
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
  link: Link,
  method: string,
  path: string,
  query: Record<string, string>,
  body?: unknown,
  project?: string,
): Promise<Answer> {
  const q = { ...query }
  const p = project ?? link.project
  if (p !== '') q.project = p
  const pairs = Object.entries(q).map(([k, v]) => `${k}=${encodeURIComponent(v)}`)
  const url = `http://${link.api}${path}${pairs.length > 0 ? `?${pairs.join('&')}` : ''}`
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

function parse<T>(a: Answer): T | undefined {
  if (!a.isOk) return undefined
  try {
    return JSON.parse(a.text) as T
  } catch {
    return undefined
  }
}

/** POST /sessions, as the classic SessionStart hook and its keep-alive do. */
async function registerSession(
  $: Engine,
  link: Link,
  isIdle: boolean,
  isHeartbeat: boolean,
): Promise<boolean> {
  const res = await call($, link, 'POST', '/sessions', {}, {
    session_id: link.sessionId,
    provider: 'claude',
    folder: link.folder,
    wake: 'rewake', // this module is the session's waiter; the node wakes nobody
    ttl_sec: TTL_SEC,
    idle: isIdle,
    ...(isHeartbeat ? { heartbeat: true } : {}),
  })
  if (res.isOk) lastBeat = await $.clock.now()
  return res.isOk
}

async function deliveries($: Engine): Promise<Delivery[]> {
  return (await $.state.get(DELIVERIES)).value ?? []
}

function authorKind(kind: string | undefined): string {
  if (kind === 'human') return 'человек'
  if (kind === 'agent') return 'агент'
  if (kind === 'worker') return 'агент-обработчик'
  return 'автор не указан'
}

/** node.FormatUnread, the reply line naming this module's tool. */
function formatUnread(m: Unread): string {
  const at = m.created_at ?? ''
  const chat = m.chat_id ?? ''
  let head: string
  if (m.own_human === true) {
    head = `Ваш человек написал всем (${at}, чат ${chat}, id ${m.id}) — к сведению, отвечать не нужно:`
  } else if (chat !== '') {
    head = `От ${m.from} (${authorKind(m.author_kind)}) в чате ${chat} (участники: ${(m.participants ?? []).join(', ')}), id ${m.id}, ${at}:`
  } else {
    head = `От ${m.from} (${authorKind(m.author_kind)}), id ${m.id}, ${at}:`
  }
  const body =
    m.body.length > FORMAT_MAX_BODY ? `${m.body.slice(0, FORMAT_MAX_BODY)}… (обрезано)` : m.body
  const lines = [head, body]
  if (m.own_human === true) return `${lines.join('\n')}\n`
  const reply =
    chat !== ''
      ? `инструмент ${TOOL_SEND} (chat_id ${chat}, reply_to ${m.id})`
      : `инструмент ${TOOL_SEND} (to ${m.from}, reply_to ${m.id})`
  if (m.assigned === 'worker') {
    lines.push('Это уже обрабатывает агент-обработчик этого узла — не отвечайте.')
  } else if (m.paused === true) {
    lines.push('К сведению, ответ не требуется.')
  } else if (m.asks_you === true) {
    lines.push(`Просит ответа от вас. Ответить: ${reply}`)
  } else {
    lines.push(`К сведению, ответ не обязателен. Ответить: ${reply}`)
  }
  return `${lines.join('\n')}\n`
}

/** node.FitUnread: the first messages that fit the budget, at least one. */
function fit(msgs: readonly Unread[]): Unread[] {
  const out: Unread[] = []
  let used = 0
  for (const m of msgs) {
    const n = formatUnread(m).length
    if (out.length > 0 && used + n > FORMAT_BUDGET) break
    used += n
    out.push(m)
  }
  return out
}

const bodyOf = (msgs: readonly Unread[], rest: number): string =>
  msgs.map(m => `\n${formatUnread(m)}`).join('') +
  (rest > 0 ? `\nЕщё ${rest} непрочитанных: инструмент ${TOOL_UNREAD}\n` : '')

/** node.WakePrompt's first line, then the messages. */
const wakeText = (count: number, body: string, token: string): string =>
  `agent-link: новые сообщения (${count}). Вся переписка остаётся в истории чата. ${marker(token)}\n${body}`.trimEnd()

/** hook.go formatBatch's first line, then the messages. */
const appendText = (count: number, body: string): string =>
  `agent-link: непрочитанные сообщения для этой папки (${count}). Вся переписка остаётся в истории чата.\n${body}`.trimEnd()

function byProject(keys: readonly string[]): Map<string, string[]> {
  const groups = new Map<string, string[]>()
  for (const k of keys) {
    const cut = k.indexOf('|')
    const project = k.slice(0, cut)
    groups.set(project, [...(groups.get(project) ?? []), k.slice(cut + 1)])
  }
  return groups
}

/** POST /claim per project; the keys the node granted this session. */
async function claim(
  $: Engine,
  link: Link,
  keys: readonly string[],
  wakeToken: string,
): Promise<string[]> {
  const granted: string[] = []
  for (const [project, ids] of byProject(keys)) {
    const res = await call($, link, 'POST', '/claim', {}, {
      ids,
      session_id: link.sessionId,
      folder: link.folder,
      ...(wakeToken === '' ? {} : { wake_token: wakeToken }),
    }, project)
    for (const id of parse<string[]>(res) ?? []) granted.push(`${project}|${id}`)
  }
  return granted
}

/** POST /ack per project; true when the node took every one. */
async function ack($: Engine, link: Link, keys: readonly string[]): Promise<boolean> {
  let isAll = true
  for (const [project, ids] of byProject(keys)) {
    const res = await call($, link, 'POST', '/ack', {}, { ids, session_id: link.sessionId }, project)
    isAll = isAll && res.isOk
  }
  return isAll
}

/** Hands a batch to the idle session as a prompt of its own. */
async function wake($: Engine, link: Link, keys: string[], body: string): Promise<void> {
  const now = await $.clock.now()
  const token = newToken(now)
  const granted = await claim($, link, keys, token)
  if (granted.length === 0) return
  const held = await deliveries($)
  await $.state.set(DELIVERIES, [...held, { token, mode: 'wake', keys: granted, body, at: now }])
  await $.prompt.submit({ text: wakeText(granted.length, body, token) })
}

/** Acknowledges what a model request of `turnId` (its step `index`) carried. */
async function ackTurn($: Engine, turnId: string, index: number): Promise<void> {
  const held = await deliveries($)
  const carried = held.filter(
    d => d.turnId === turnId && (d.mode === 'wake' || index > (d.afterStep ?? -1)),
  )
  if (carried.length === 0) return
  const link = (await $.state.get(LINK)).value
  if (link === undefined) return
  const done = new Set<string>()
  for (const d of carried) {
    if (await ack($, link, d.keys)) done.add(d.token)
  }
  const left = (await deliveries($)).filter(d => !done.has(d.token))
  await $.state.set(DELIVERIES, left)
}

/** One poll: keep the session live, hand over what is new. */
async function poll($: Engine): Promise<void> {
  const read = await $.state.get(LINK)
  let link = read.value
  if (link === undefined) return
  const now = await $.clock.now()
  const turn = (await $.state.get(TURN)).value ?? ''
  if (!link.isRegistered || now - lastBeat >= HEARTBEAT_MS) {
    const isRegistered = await registerSession($, link, turn === '', true)
    if (isRegistered !== link.isRegistered) {
      link = { ...link, isRegistered }
      await $.state.set(LINK, link)
    }
  }
  if (!link.isRegistered) {
    $.ui.status(undefined)
    return
  }
  // A batch no turn proved within the node's grace is the node's again.
  let held = await deliveries($)
  const kept = held.filter(d => now - d.at < DELIVERY_GRACE_MS)
  if (kept.length !== held.length) {
    held = kept
    await $.state.set(DELIVERIES, held)
  }
  const page = parse<UnreadPage>(
    await call($, link, 'GET', '/unread', {
      folder: link.folder,
      session: link.sessionId,
      limit: String(PAGE),
      actionable: '1',
      waiter: '1',
    }),
  )
  if (page === undefined) return
  const pending = new Set(held.flatMap(d => d.keys))
  const fresh = page.messages.filter(m => !pending.has(keyOf(m)))
  const count = fresh.length + pending.size
  $.ui.status(count > 0 ? `agent-link: ${count} unread` : undefined)
  if (fresh.length === 0) return
  const shown = fit(fresh)
  const body = bodyOf(shown, Math.max(0, page.total - shown.length))
  const keys = shown.map(keyOf)
  if (turn === '') {
    await wake($, link, keys, body)
    return
  }
  const granted = await claim($, link, keys, '')
  if (granted.length === 0) return
  const stored = await $.session.append({
    message: { type: 'user', content: [{ type: 'text', text: appendText(granted.length, body) }] },
  })
  if (stored.deny !== undefined) return
  const delivery: Delivery = {
    token: newToken(now),
    mode: 'append',
    keys: granted,
    body,
    turnId: turn,
    afterStep: stepIndex,
    at: now,
  }
  await $.state.set(DELIVERIES, [...(await deliveries($)), delivery])
}

async function tick($: Engine): Promise<void> {
  if (isPolling) return
  isPolling = true
  try {
    await poll($)
  } catch {
    // a poll never breaks the session; the next one tries again
  } finally {
    isPolling = false
  }
}

const text = (value: unknown): string => (typeof value === 'string' ? value.trim() : '')

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    const link: Link = {
      api: await findApi($),
      project: ((await $.env.get('AGENTLINK_PROJECT_ID')) ?? '').trim(),
      sessionId: await $.session.id(),
      folder: e.cwd,
      isRegistered: false,
    }
    link.isRegistered = await registerSession($, link, true, false)
    await $.state.set(LINK, link)
    await $.state.set(TURN, '')
    await $.tool.register({
      name: 'unread',
      description:
        'agent-link: lists the unread messages of this folder for this session (who wrote, in which chat, the text) and marks them read.',
      inputSchema: {
        type: 'object',
        properties: { limit: { type: 'integer', minimum: 1, maximum: 50 } },
      },
      isDeferred: false,
    })
    await $.tool.register({
      name: 'send',
      description:
        'agent-link: sends a message to a chat (chat_id) or a member (to), optionally as a reply (reply_to) and asking members to answer (ask).',
      inputSchema: {
        type: 'object',
        properties: {
          body: { type: 'string' },
          chat_id: { type: 'string' },
          to: { type: 'string' },
          reply_to: { type: 'string' },
          ask: { type: 'array', items: { type: 'string' } },
        },
        required: ['body'],
      },
      isDeferred: false,
    })
    timer?.cancel()
    timer = $.clock.every(POLL_MS, () => {
      void tick($)
    })
    void tick($)
    return next(e)
  })

  on('turn.start', async ($, e, next) => {
    stepIndex = -1
    await $.state.set(TURN, e.turnId)
    const held = await deliveries($)
    const bound = held.map(d =>
      d.mode === 'wake' && d.turnId === undefined && e.text.includes(marker(d.token))
        ? { ...d, turnId: e.turnId }
        : d,
    )
    if (bound.some((d, i) => d !== held[i])) await $.state.set(DELIVERIES, bound)
    const link = (await $.state.get(LINK)).value
    if (link?.isRegistered === true) void registerSession($, link, false, false)
    return next(e)
  })

  on('turn.step', async function* ($, e, next) {
    const stream = next(e)
    if (e.agentId !== undefined) return yield* stream
    stepIndex = e.index
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
    stepIndex = -1
    await $.state.set(TURN, '')
    const link = (await $.state.get(LINK)).value
    const held = await deliveries($)
    const late = held.filter(d => d.turnId === e.turnId)
    if (late.length > 0) {
      await $.state.set(DELIVERIES, held.filter(d => d.turnId !== e.turnId))
      // Appended after the turn's last request: no request carried them. The
      // session is idle now, so they wake it. A turn that failed leaves them
      // to the node's grace, so a broken session is not woken in a loop.
      if (link !== undefined && e.reason === 'answer') {
        for (const d of late) {
          if (d.mode === 'append') void wake($, link, d.keys, d.body)
        }
      }
    }
    if (link?.isRegistered === true) void registerSession($, link, true, false)
    return next(e)
  })

  on('tool.call', { tool: TOOL_UNREAD }, async ($, e) => {
    const link = (await $.state.get(LINK)).value
    if (link === undefined || !link.isRegistered) {
      return { result: 'agent-link: this session is not registered with the local node.' }
    }
    const limit = typeof e.limit === 'number' ? Math.min(50, Math.max(1, Math.trunc(e.limit))) : PAGE
    const page = parse<UnreadPage>(
      await call($, link, 'GET', '/unread', {
        folder: link.folder,
        session: link.sessionId,
        limit: String(limit),
      }),
    )
    if (page === undefined) return { result: 'agent-link: the local node did not answer.' }
    if (page.messages.length === 0) return { result: 'agent-link: no unread messages.' }
    const shown = fit(page.messages)
    // The result is read by the model in this very turn: acknowledged here.
    await ack($, link, shown.map(keyOf))
    return {
      result: appendText(page.total, bodyOf(shown, Math.max(0, page.total - shown.length))),
    }
  }).catch(() => ({ deny: 'agent-link unread failed inside the module.' }))

  on('tool.call', { tool: TOOL_SEND }, async ($, e) => {
    const link = (await $.state.get(LINK)).value
    if (link === undefined) return { result: 'agent-link: no local node.' }
    const body = text(e.body)
    if (body === '') return { deny: 'agent-link send: body is required.' }
    const ask = Array.isArray(e.ask) ? e.ask.filter(a => typeof a === 'string') : []
    const res = await call($, link, 'POST', '/send', {}, {
      body,
      ...(text(e.chat_id) === '' ? {} : { chat_id: text(e.chat_id) }),
      ...(text(e.to) === '' ? {} : { to: text(e.to) }),
      ...(text(e.reply_to) === '' ? {} : { reply_to: text(e.reply_to) }),
      ...(ask.length === 0 ? {} : { ask }),
      folder: link.folder,
      session_id: link.sessionId,
    })
    const sent = parse<{ id?: string; chat_id?: string }>(res)
    if (sent === undefined) {
      return { deny: `agent-link send failed (${res.status}): ${res.text.slice(0, 300)}` }
    }
    return { result: `agent-link: sent, id ${sent.id ?? '?'}, chat ${sent.chat_id ?? '-'}` }
  }).catch(() => ({ deny: 'agent-link send failed inside the module.' }))

  on('session.end', async ($, e, next) => {
    timer?.cancel()
    timer = undefined
    $.ui.status(undefined)
    const link = (await $.state.get(LINK)).value
    if (link !== undefined) {
      await call($, link, 'DELETE', `/sessions/${encodeURIComponent(e.sessionId)}`, {})
    }
    return next(e)
  })
}
