// A node's local API in memory, and the engine beneath the module: what the
// tests of this spike stand on.
import type { On } from 'claude-code'
import { mock } from 'claude-code/testing'
import type { MockClock, MockSession } from 'claude-code/testing'

export type Call = {
  method: string
  path: string
  query: Record<string, string>
  body: Record<string, unknown>
}

export type Message = { id: string; from: string; body: string; chat_id?: string; asks_you?: boolean }

export type World = {
  clock: MockClock
  session: MockSession
  /** Every request the module made of the node, oldest first. */
  calls: Call[]
  /** The texts the module submitted as prompts. */
  prompts: string[]
  /** What the status line was last set to. */
  status: () => string | undefined
  /** The node's unread messages; a test adds to it. */
  unread: Message[]
  /** The ids of every POST /ack so far. */
  acked: () => string[]
  of: (method: string, path: string) => Call[]
}

export const SESSION = 'sess-1'
export const FOLDER = 'C:\\work\\demo'

export function world(on: On, unread: Message[] = []): World {
  const calls: Call[] = []
  const prompts: string[] = []
  let status: string | undefined
  const clock = mock.clock(on)
  const session = mock.session(on)
  mock.env(on, { AGENTLINK_API: '127.0.0.1:1' })
  on('session.id', () => ({ value: SESSION }))
  on('session.start', (_$, e) => ({ cwd: e.cwd }))
  on('tool.register', (_$, e) => ({ value: { tool: `mcp__agent-link-native__${e.name}` } }))
  on('ui.status', (_$, e) => {
    status = e.text
    return { value: undefined }
  })
  on('prompt.submit', (_$, e) => {
    prompts.push(e.text)
    return { text: e.text }
  })
  on('turn.start', (_$, e) => ({ turnId: e.turnId }))
  on('turn.complete', (_$, e) => ({ text: e.answer }))
  on('turn.step', async function* (_$, e) {
    yield { kind: 'text', index: 0, text: 'ok' }
    return {
      turnId: e.turnId,
      index: e.index,
      answer: 'ok',
      toolUses: [],
      stopReason: 'end_turn',
      usage: null,
    }
  })
  on('http.fetch', (_$, e) => {
    const [, rest = ''] = e.url.split('127.0.0.1:1')
    const [path = '', search = ''] = rest.split('?')
    const query: Record<string, string> = {}
    for (const pair of search.split('&')) {
      const [k = '', v = ''] = pair.split('=')
      if (k !== '') query[k] = decodeURIComponent(v)
    }
    const body = (e.init?.body === undefined ? {} : JSON.parse(e.init.body)) as Record<string, unknown>
    const method = e.init?.method ?? 'GET'
    calls.push({ method, path, query, body })
    const ids = (Array.isArray(body.ids) ? body.ids : []) as string[]
    let answer: unknown = {}
    if (path === '/unread') answer = { messages: unread, total: unread.length }
    if (path === '/claim') answer = ids
    if (path === '/ack') {
      for (const id of ids) {
        const at = unread.findIndex(m => m.id === id)
        if (at >= 0) unread.splice(at, 1)
      }
      answer = ids.map(id => ({ id, found: true, was_unread: true }))
    }
    if (path === '/send') answer = { id: 'm-out', chat_id: body.chat_id }
    return { value: { status: 200, ok: true, headers: {}, text: JSON.stringify(answer) } }
  })
  const of = (method: string, path: string): Call[] =>
    calls.filter(c => c.method === method && c.path === path)
  return {
    clock,
    session,
    calls,
    prompts,
    status: () => status,
    unread,
    acked: () => of('POST', '/ack').flatMap(c => c.body.ids as string[]),
    of,
  }
}
