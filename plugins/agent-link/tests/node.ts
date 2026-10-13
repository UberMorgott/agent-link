// A node's local API in memory, and the engine beneath the module: what the
// module's tests stand on. The node here words a delivery in a made-up way
// (render): the tests check that the module hands its text on untouched.
import type { On, ToolCallResult } from 'claude-code'
import { mock } from 'claude-code/testing'
import type { Engine, MockClock, MockSession } from 'claude-code/testing'

export type Call = {
  method: string
  path: string
  query: Record<string, string>
  body: Record<string, unknown>
}

/** node.UnreadMessage, the fields the tests set. */
export type Message = {
  id: string
  from: string
  body: string
  chat_id?: string
  asks_you?: boolean
  paused?: boolean
  project?: string
  for_agent?: string
}

export type Reply = { status: number; body?: unknown }

export type World = {
  clock: MockClock
  session: MockSession
  /** Every request the module made of the node, oldest first. */
  calls: Call[]
  /** The texts the module submitted as prompts. */
  prompts: string[]
  /** What the person saw: the status line, and the lines logged. */
  status: () => string | undefined
  logged: string[]
  /** The tools the module registered. */
  tools: { name: string; description: string; inputSchema?: unknown }[]
  /** The node's unread messages; a test adds to it. */
  unread: Message[]
  /** Ids another session holds: a claim of them is not granted. */
  taken: Set<string>
  /** Ids the node's worker took: their ack says so. */
  worker: Set<string>
  /** How many messages one text carries. */
  fit: number
  /** The node does not answer. */
  isDown: boolean
  /** The node's switch (GET /native), asked only by a test that clears $AGENTLINK_NATIVE_CLAUDE. */
  isNative: boolean
  /** The id `$.session.id()` answers. */
  sessionId: string
  /** A test's own answer for a path, in place of the node's. */
  answer: Map<string, (c: Call) => Reply>
  /** What a submitted prompt answers: its drop reason, when set. */
  dropPrompts: string | undefined
  /** A submitted prompt enters only once this resolves (the engine is busy). */
  promptGate: Promise<void> | undefined
  /** Whether a wake holds message `id` at the node now (it lapses WAKE_GRACE_MS after its last claim). */
  wakeHeld: (id: string) => boolean
  /** The ids of every POST /ack, and of every POST /release, so far. */
  acked: () => string[]
  released: () => string[]
  of: (method: string, path: string) => Call[]
  last: (method: string, path: string) => Call | undefined
}

export const SESSION = 'sess-1'
export const FOLDER = 'C:\\work\\demo'
export const API = '127.0.0.1:1'
export const TOOL = 'mcp__agent-link__'
/** node.inboxWakeGrace: how long a wake claim holds a message after it was last taken. */
export const WAKE_GRACE_MS = 120_000

export function world(on: On, unread: Message[] = [], env: Record<string, string> = {}): World {
  const calls: Call[] = []
  const prompts: string[] = []
  const logged: string[] = []
  const tools: World['tools'] = []
  // The wake claims, as the node keeps them: a message a wake holds is listed
  // for nobody, and granted only to that wake again (its token), which renews
  // it; a wake not taken again for WAKE_GRACE_MS lapsed, and the message is
  // listed and granted as any other.
  const wakes = new Map<string, { token: string; at: number }>()
  let status: string | undefined
  const clock = mock.clock(on)
  const wakeHeld = (id: string): boolean => {
    const k = wakes.get(id)
    return k !== undefined && clock.now() - k.at < WAKE_GRACE_MS
  }
  const session = mock.session(on)
  // Attended as the interactive CLI is, and switched on by the session's own
  // environment (so the node is not asked), unless a test says else.
  mock.env(on, { AGENTLINK_API: API, CLAUDE_CODE_SESSION_ATTENDED: '1', AGENTLINK_NATIVE_CLAUDE: '1', ...env })
  const of = (method: string, path: string): Call[] => calls.filter(c => c.method === method && c.path === path)
  const w: World = {
    clock,
    session,
    calls,
    prompts,
    status: () => status,
    logged,
    tools,
    unread,
    taken: new Set(),
    worker: new Set(),
    fit: 20,
    isDown: false,
    isNative: false,
    sessionId: SESSION,
    answer: new Map(),
    dropPrompts: undefined,
    promptGate: undefined,
    wakeHeld,
    acked: () => of('POST', '/ack').flatMap(c => c.body.ids as string[]),
    released: () => of('POST', '/release').flatMap(c => c.body.ids as string[]),
    of,
    last: (method, path) => of(method, path).at(-1),
  }
  on('session.id', () => ({ value: w.sessionId }))
  on('session.start', (_$, e) => ({ cwd: e.cwd }))
  on('session.end', (_$, e) => ({ sessionId: e.sessionId }))
  on('session.attach', (_$, e) => ({ clientId: e.clientId }))
  on('tool.register', (_$, e) => {
    tools.push({ name: e.name, description: e.description, inputSchema: e.inputSchema })
    return { value: { tool: `${TOOL}${e.name}` } }
  })
  on('tool.call', () => ({ result: 'done' }))
  on('ui.status', (_$, e) => {
    status = e.text
    return { value: undefined }
  })
  on('ui.log', (_$, e) => {
    logged.push(e.text)
    return { value: undefined }
  })
  on('prompt.submit', async (_$, e) => {
    if (w.promptGate !== undefined) await w.promptGate
    if (w.dropPrompts !== undefined) return { drop: w.dropPrompts }
    prompts.push([e.text, ...(e.context ?? [])].join('\n'))
    return { text: e.text }
  })
  on('turn.start', (_$, e) => ({ turnId: e.turnId }))
  on('turn.complete', (_$, e) => ({ text: e.answer }))
  on('turn.step', async function* (_$, e) {
    yield { kind: 'text', index: 0, text: 'ok' }
    return { turnId: e.turnId, index: e.index, answer: 'ok', toolUses: [], stopReason: 'end_turn', usage: null }
  })
  on('classic.SessionStart', () => ({}))
  on('classic.SubagentStart', () => ({}))
  on('classic.SubagentStop', () => ({}))
  on('http.fetch', (_$, e) => {
    if (w.isDown) throw new Error('connection refused')
    const [, rest = ''] = e.url.split(API)
    const [path = '', search = ''] = rest.split('?')
    const query: Record<string, string> = {}
    for (const pair of search.split('&')) {
      const [k = '', v = ''] = pair.split('=')
      if (k !== '') query[k] = decodeURIComponent(v)
    }
    const body = (e.init?.body === undefined ? {} : JSON.parse(e.init.body)) as Record<string, unknown>
    const method = e.init?.method ?? 'GET'
    const c: Call = { method, path, query, body }
    calls.push(c)
    const own = w.answer.get(path)
    if (own !== undefined) {
      const r = own(c)
      return { value: { status: r.status, ok: r.status >= 200 && r.status < 300, headers: {}, text: r.body === undefined ? '' : JSON.stringify(r.body) } }
    }
    const ids = (Array.isArray(body.ids) ? body.ids : []) as string[]
    let answer: unknown = {}
    if (path === '/unread') {
      const skip = (query.skip_agents ?? '').split(',')
      const list = unread.filter(m => {
        if (wakeHeld(m.id)) return false // a wake holds it
        if (query.for_agent !== undefined) return m.for_agent === query.for_agent
        return m.for_agent === undefined || !skip.includes(m.for_agent)
      })
      answer = { messages: list, total: list.length }
    }
    if (path === '/claim') {
      const token = typeof body.wake_token === 'string' ? body.wake_token : ''
      const granted = ids.filter(id => {
        if (w.taken.has(id) || !unread.some(m => m.id === id)) return false
        return !wakeHeld(id) || wakes.get(id)?.token === token
      })
      if (token !== '') for (const id of granted) wakes.set(id, { token, at: clock.now() })
      answer = granted
    }
    if (path === '/release') for (const id of ids) wakes.delete(id)
    if (path === '/ack') {
      answer = ids.map(id => {
        const at = unread.findIndex(m => m.id === id)
        const chat = at >= 0 ? unread[at]?.chat_id : undefined
        if (at >= 0) unread.splice(at, 1)
        wakes.delete(id)
        return {
          id,
          found: true,
          was_unread: at >= 0,
          ...(w.worker.has(id) ? { assigned: 'worker', note: `note ${id} ${chat ?? ''}` } : {}),
        }
      })
    }
    if (path === '/render') {
      const msgs = (Array.isArray(body.messages) ? body.messages : []) as Message[]
      const mode = typeof body.mode === 'string' ? body.mode : ''
      const token = typeof body.token === 'string' ? ` [${body.token}]` : ''
      const notes = (Array.isArray(body.notes) ? body.notes : []) as string[]
      if (mode === 'nudge') {
        answer = { text: `nudge ${String(body.total)}${token}`, shown: [] }
      } else {
        const shown = msgs.slice(0, w.fit)
        const words = shown.map(m => `id ${m.id}${m.paused === true ? ' held' : ''}`).join(', ')
        const parts = [
          ...notes,
          shown.length > 0 ? `${mode === '' ? 'batch' : mode}${token} ${words}` : '',
          body.cli === true ? 'CLI' : '',
        ]
        answer = {
          text: parts.filter(p => p !== '').join('\n'),
          notice: shown.length > 0 ? `notice ${shown.length}` : '',
          shown: shown.map(m => ({ id: m.id, ...(m.project === undefined ? {} : { project: m.project }) })),
          chats: Object.fromEntries(shown.flatMap(m => (m.chat_id === undefined ? [] : [[m.chat_id, m.id]]))),
        }
      }
    }
    if (path === '/native') answer = { claude: w.isNative }
    if (path === '/send') answer = { id: 'm-out', chat_id: body.chat_id ?? 'c-new' }
    if (path === '/chats') answer = method === 'POST' ? { id: 'c-made' } : [{ id: 'c-1' }]
    const isEmpty = method === 'DELETE' || path === '/release' || path.endsWith('/doing')
    return { value: { status: isEmpty ? 204 : 200, ok: true, headers: {}, text: isEmpty ? '' : JSON.stringify(answer) } }
  })
  return w
}

/** Starts the session as the interactive CLI does and lets the first pass end. */
export async function start($: Engine, w: World, isInteractive = true): Promise<void> {
  await $.session.start({ cwd: FOLDER, surface: isInteractive ? 'terminal' : null, isInteractive })
  await w.clock.settle()
}

/** One model request of a turn, read to its end. */
export async function step($: Engine, turnId: string, index: number): Promise<void> {
  const stream = $.turn.step({ turnId, index, model: 'test-model', messageCount: 1 })
  for await (const chunk of stream) void chunk
}

/** A whole turn of one request that started with `text`. */
export async function turn($: Engine, w: World, turnId: string, text: string): Promise<void> {
  await $.turn.start({ text, turnId })
  await step($, turnId, 0)
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId, reason: 'answer' })
  await w.clock.settle()
}

/** The text of every row the module appended to the conversation. */
export const appended = (w: World): string[] =>
  w.session.appended().map(r => r.message.content.map(b => ('text' in b ? String(b.text) : '')).join(''))

/** A tool call made inside subagent `agentId`, as the engine raises it there. */
export const inAgent = ($: Engine, agentId: string, input: Record<string, unknown>): Promise<ToolCallResult> =>
  ($.tool.call as (i: unknown) => Promise<ToolCallResult>)({ ...input, agentId })
