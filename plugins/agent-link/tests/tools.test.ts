import type { Engine } from 'claude-code/testing'
import { expect, test } from 'claude-code/testing'

import type { World } from './node'
import { FOLDER, SESSION, start, TOOL, world } from './node'

/** Calls one of the module's tools: its text, or the error the model reads. */
async function call($: Engine, name: string, args: Record<string, unknown> = {}): Promise<string> {
  const res = await $.tool.call({ tool: `${TOOL}${name}`, ...args })
  return res.deny === undefined ? String(res.result) : `ERROR ${res.deny}`
}

const NAMES = ['ack', 'chats', 'discuss', 'history', 'members', 'projects', 'seats', 'send', 'unread']

/** The node answers a list for every listing a tool asks. */
function lists(w: World): void {
  for (const path of ['/projects', '/members', '/seats', '/chats/c-1/messages']) {
    w.answer.set(path, () => ({ status: 200, body: [{ at: path }] }))
  }
}

test('the nine tools of agentlink mcp are registered, by their names and schemas', async ($, on) => {
  const w = world(on)
  await start($, w)
  expect(w.tools.map(t => t.name)).toEqual(NAMES)
  for (const t of w.tools) {
    expect(t.description.length).toBeGreaterThan(10)
    expect(t.inputSchema).toMatchObject({ type: 'object' })
  }
  const send = w.tools.find(t => t.name === 'send')
  expect(send?.inputSchema).toHaveProperty(['properties', 'attachments', 'description'])
  expect(send?.description).toMatch(/^Send a message: into a chat \(chat\)/)
})

test('the listing tools ask the node as agentlink mcp does and answer its JSON', async ($, on) => {
  const w = world(on)
  lists(w)
  await start($, w)
  expect(await call($, 'projects')).toBe('[{"at":"/projects"}]')
  expect(w.last('GET', '/projects')?.query).toEqual({})

  // Without a project the session's folder hints at one; a named project stands alone.
  expect(await call($, 'members')).toBe('[{"at":"/members"}]')
  expect(w.last('GET', '/members')?.query).toEqual({ cwd: FOLDER })
  await call($, 'seats', { project: 'p-9' })
  expect(w.last('GET', '/seats')?.query).toEqual({ project: 'p-9' })

  expect(await call($, 'chats', { archive: true, legacy: true })).toBe('[{"id":"c-1"}]')
  expect(w.last('GET', '/chats')?.query).toEqual({ archive: '1', legacy: '1' })

  expect(await call($, 'history')).toBe('ERROR chat is required')
  expect(await call($, 'history', { chat: 'c-1', after_seq: 7 })).toBe('[{"at":"/chats/c-1/messages"}]')
  expect(w.last('GET', '/chats/c-1/messages')?.query).toEqual({ limit: '50', after: '7' })
  await call($, 'history', { chat: 'c-1', limit: 5, before_seq: 3, project: 'p-9' })
  expect(w.last('GET', '/chats/c-1/messages')?.query).toEqual({ limit: '5', before: '3', project: 'p-9' })

  // A list the node has none of is an empty list, an API error the tool's error.
  w.answer.set('/seats', () => ({ status: 200, body: null }))
  expect(await call($, 'seats')).toBe('[]')
  w.answer.set('/seats', () => ({ status: 409, body: { error: 'the project has no folder', code: 'needs_folder' } }))
  expect(await call($, 'seats')).toBe('ERROR the project has no folder (needs_folder)')
  w.isDown = true
  expect(await call($, 'seats')).toBe('ERROR agentlink: the local node (127.0.0.1:1) did not answer')
})

test('unread lists for this session without marking read; ack marks read', async ($, on) => {
  const w = world(on)
  await start($, w)
  await $.turn.start({ text: 'work', turnId: 't-1' })
  w.unread.push({ id: 'm-1', from: 'peer', body: 'ping', chat_id: 'c-1' })
  const listed = JSON.parse(await call($, 'unread', { limit: 5, after: 'cur-1', folder: 'sub' })) as { total: number }
  expect(listed.total).toBe(1)
  expect(w.last('GET', '/unread')?.query).toEqual({
    limit: '5',
    cwd: FOLDER,
    folder: `${FOLDER}/sub`,
    after: 'cur-1',
    session: SESSION,
    agent: '1',
  })
  expect(w.acked()).toEqual([])

  expect(await call($, 'ack')).toBe('ERROR ids is required')
  const acked = JSON.parse(await call($, 'ack', { ids: ['m-1'] })) as { id: string }[]
  expect(acked.map(a => a.id)).toEqual(['m-1'])
  expect(w.last('POST', '/ack')?.body).toEqual({ ids: ['m-1'], session_id: SESSION })
  w.answer.set('/chats/c-1/ack', () => ({ status: 200, body: [] }))
  await call($, 'ack', { ids: ['m-2'], chat: 'c-1', session: 'other', project: 'p-9' })
  expect(w.last('POST', '/chats/c-1/ack')).toMatchObject({ query: { project: 'p-9' }, body: { ids: ['m-2'], session_id: 'other' } })
})

test('send writes into a chat, to a member or a new chat, attachments by path', async ($, on) => {
  const w = world(on)
  await start($, w)
  expect(await call($, 'send', { body: 'hi' })).toBe('ERROR exactly one of chat, to, new_chat_with is required')
  expect(await call($, 'send', { chat: 'c-1', to: 'bob', body: 'hi' })).toBe('ERROR exactly one of chat, to, new_chat_with is required')
  expect(await call($, 'send', { chat: 'c-1' })).toBe('ERROR body is required (or attachments)')

  expect(await call($, 'send', { chat: 'c-1', body: 'hi', reply_to: 'm-1', ask: ['bob'], ask_seats: ['codex'] })).toBe('{"id":"m-out","chat":"c-1"}')
  expect(w.last('POST', '/send')?.body).toEqual({
    to: '',
    body: 'hi',
    reply_to: 'm-1',
    chat_id: 'c-1',
    ask: ['bob'],
    folder: FOLDER,
    session_id: SESSION,
    ask_seats: ['codex'],
  })

  // The node reads the files itself: the module passes absolute paths, no bytes.
  await call($, 'send', { to: 'bob', attachments: ['shot.png', 'D:\\tmp\\log.txt'] })
  expect(w.last('POST', '/send')?.body).toMatchObject({ to: 'bob', body: '', files: [`${FOLDER}/shot.png`, 'D:\\tmp\\log.txt'] })

  expect(await call($, 'send', { new_chat_with: ['bob', 'eve'], body: 'hello' })).toBe('{"id":"m-out","chat":"c-made"}')
  expect(w.last('POST', '/chats')).toMatchObject({ query: { cwd: FOLDER }, body: { participants: ['bob', 'eve'] } })
  expect(w.last('POST', '/send')?.body).toMatchObject({ chat_id: 'c-made', body: 'hello' })

  // Held: nobody answers it automatically, and the caller hears so.
  w.answer.set('/send', () => ({ status: 200, body: { id: 'm-held', chat_id: 'c-1', hold_reason: 'auto_limit' } }))
  expect(await call($, 'send', { chat: 'c-1', body: 'again' })).toBe('{"id":"m-held","chat":"c-1","hold_reason":"auto_limit"}')
  w.answer.set('/send', () => ({ status: 400, body: { error: 'unknown chat c-0' } }))
  expect(await call($, 'send', { chat: 'c-0', body: 'x' })).toBe('ERROR unknown chat c-0')
})

const ASKED = { project: 'p-local', chat: 'c-9', id: 'q-1', seat: 's-codex', scope: 'project' }
const ANSWER = {
  id: 'r-1',
  from: 'morgott',
  body: 'the answer',
  chat_id: 'c-9',
  agent: { label: 'Codex', provider: 'codex', model: 'gpt-5', effort: 'high' },
  attachments: [{ name: 'a.png', path: 'C:\\files\\a.png' }],
}

test('discuss asks a local agent and waits for its answer, as many held requests as it takes', async ($, on) => {
  const w = world(on)
  await start($, w)
  expect(await call($, 'discuss', { with: 'gemini', body: 'q' })).toBe('ERROR --with must be claude or codex')
  expect(await call($, 'discuss', { with: 'codex', body: ' ' })).toBe('ERROR --body is required')
  expect(await call($, 'discuss', { with: 'codex', body: 'q', timeout: '16m' })).toBe('ERROR --timeout must be greater than 0 and at most 15m0s')

  w.answer.set('/discuss', () => ({ status: 200, body: ASKED }))
  let waits = 0
  w.answer.set('/discuss/reply', () => (++waits < 3 ? { status: 204 } : { status: 200, body: ANSWER }))
  const got = JSON.parse(await call($, 'discuss', { with: 'codex', body: 'what now', topic: 'plan', shared: true })) as unknown
  expect(w.last('POST', '/discuss')?.body).toEqual({
    folder: FOLDER,
    provider: 'codex',
    body: 'what now',
    session_id: SESSION,
    source: 'claude',
    seat: '',
    agent_id: '',
    agent_type: '',
    chat: '',
    topic: 'plan',
    temporary: false,
    shared: true,
  })
  // Each request is held a short while by the node; the wait is three of them.
  expect(w.of('GET', '/discuss/reply')).toHaveLength(3)
  expect(w.last('GET', '/discuss/reply')?.query).toEqual({
    project: 'p-local',
    chat: 'c-9',
    id: 'q-1',
    seat: 's-codex',
    timeout: '25s',
    client_ack: '1',
  })
  expect(got).toEqual({
    chat: 'c-9',
    id: 'q-1',
    reply: 'the answer',
    reply_id: 'r-1',
    from: 'Codex',
    model: 'gpt-5',
    effort: 'high',
    attachments: ['C:\\files\\a.png'],
  })
  // The answer reached its asker: read.
  expect(w.last('POST', '/ack')).toMatchObject({ query: { project: 'p-local' }, body: { ids: ['r-1'], session_id: SESSION } })

  const full = JSON.parse(await call($, 'discuss', { with: 'codex', body: 'again', full: true })) as { reply: unknown; project: string }
  expect(full.project).toBe('p-local')
  expect(full.reply).toEqual(ANSWER)
})

test('discuss says when nothing will answer: async, queued, held, out of time', async ($, on) => {
  const w = world(on)
  await start($, w)
  w.answer.set('/discuss', () => ({ status: 200, body: ASKED }))
  expect(await call($, 'discuss', { with: 'claude', body: 'q', async: true })).toBe('{"chat":"c-9","id":"q-1"}')
  expect(w.of('GET', '/discuss/reply')).toEqual([])

  w.answer.set('/discuss/reply', () => ({ status: 202 }))
  expect(await call($, 'discuss', { with: 'claude', body: 'q' })).toBe('{"chat":"c-9","id":"q-1","queued":true}')

  w.answer.set('/discuss/reply', () => ({
    status: 409,
    body: { error: 'the agent failed: usage limit', hold_reason: 'seat_failed', seat_error: 'usage limit', retry_at: '2026-10-13T12:00:00Z' },
  }))
  expect(JSON.parse(await call($, 'discuss', { with: 'claude', body: 'q' }))).toEqual({
    chat: 'c-9',
    id: 'q-1',
    held: true,
    hold_reason: 'seat_failed',
    note: 'the agent failed: usage limit',
    seat_error: 'usage limit',
    retry_at: '2026-10-13T12:00:00Z',
  })

  // Held at once: the node's own words, no wait.
  w.answer.set('/discuss', () => ({ status: 200, body: { ...ASKED, hold_reason: 'auto_limit', held: true, note: 'pause' } }))
  const before = w.of('GET', '/discuss/reply').length
  expect(JSON.parse(await call($, 'discuss', { with: 'claude', body: 'q' }))).toEqual({
    chat: 'c-9',
    id: 'q-1',
    held: true,
    hold_reason: 'auto_limit',
    note: 'pause',
  })
  expect(w.of('GET', '/discuss/reply')).toHaveLength(before)

  // Out of time: each held request ends with nothing until the wait is over.
  w.answer.set('/discuss', () => ({ status: 200, body: ASKED }))
  w.answer.set('/discuss/reply', () => ({ status: 204 }))
  const asked = w.of('GET', '/discuss/reply').length
  expect(await call($, 'discuss', { with: 'claude', body: 'q', timeout: '1m' })).toBe('{"chat":"c-9","id":"q-1","timed_out":true}')
  expect(w.of('GET', '/discuss/reply').length - asked).toBe(5)
})
