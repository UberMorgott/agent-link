import { expect, test } from 'claude-code/testing'

import { FOLDER, SESSION, start, step, TOOL, turn, world } from './node'

const PEER = { id: 'm-1', from: 'peer', body: 'ping', chat_id: 'c-1', asks_you: true }

test('the session is registered as the shell hooks register it, marked native', async ($, on) => {
  const w = world(on, [], { AGENTLINK_PROJECT_ID: 'p-1' })
  await start($, w)
  const first = w.of('POST', '/sessions')[0]
  expect(first?.query).toEqual({ project: 'p-1' })
  expect(first?.body).toEqual({
    session_id: SESSION,
    provider: 'claude',
    folder: FOLDER,
    wake: 'rewake',
    ttl_sec: 900,
    idle: true,
    native: true,
    agents: [],
  })
  // Busy from the turn's start, idle again at its end; each is an event, no keep-alive.
  await $.turn.start({ text: 'work', turnId: 't-1' })
  expect(w.last('POST', '/sessions')?.body).toMatchObject({ idle: false, native: true })
  expect(w.last('POST', '/sessions')?.body.heartbeat).toBeUndefined()
  await step($, 't-1', 0)
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-1', reason: 'answer' })
  expect(w.last('POST', '/sessions')?.body).toMatchObject({ idle: true, native: true })
  // Every listing and claim says whose it is: the node keeps the shell hooks out.
  await w.clock.advance(4000)
  for (const c of w.of('GET', '/unread')) expect(c.query).toMatchObject({ native: '1', project: 'p-1', folder: FOLDER })
})

test('a headless run registers nothing and takes no message, but has the tools', async ($, on) => {
  const w = world(on, [PEER], { CLAUDE_CODE_SESSION_ATTENDED: '0' })
  await start($, w, false)
  await w.clock.advance(10_000)
  expect(w.calls).toEqual([])
  expect(w.prompts).toEqual([])
  expect(w.tools).toHaveLength(9)
  const listed = await $.tool.call({ tool: `${TOOL}chats` })
  expect(listed.result).toBe('[{"id":"c-1"}]')
})

test('without the engine\'s word a run nobody draws is headless, until a client attaches', async ($, on) => {
  const w = world(on, [PEER], { CLAUDE_CODE_SESSION_ATTENDED: '' })
  await start($, w, false)
  await w.clock.advance(4000)
  expect(w.calls).toEqual([])
  await $.session.attach({ surface: 'desktop', clientId: 'desktop:default' })
  await w.clock.settle()
  expect(w.of('POST', '/sessions')[0]?.body).toMatchObject({ session_id: SESSION, native: true })
  expect(w.prompts).toHaveLength(1)
})

test('a worker\'s job is no session: nothing registered, a send names the job and no session', async ($, on) => {
  const w = world(on, [PEER], { AGENTLINK_JOB_ID: 'job-7', AGENTLINK_SEAT: 'seat-2' })
  await start($, w)
  await w.clock.advance(4000)
  expect(w.calls).toEqual([])
  await $.tool.call({ tool: `${TOOL}send`, chat: 'c-1', body: 'done' })
  const sent = w.last('POST', '/send')?.body
  expect(sent).toMatchObject({ chat_id: 'c-1', body: 'done', parent: 'job-7', seat: 'seat-2' })
  expect(sent?.session_id).toBeUndefined()
})

test('a proof\'s headless run is attended when it says so', async ($, on) => {
  const w = world(on, [PEER], { CLAUDE_CODE_SESSION_ATTENDED: '0', AGENTLINK_NATIVE_HEADLESS: '1' })
  await start($, w, false)
  expect(w.of('POST', '/sessions')).toHaveLength(1)
  expect(w.prompts).toHaveLength(1)
})

test('a /clear ends the session and the one that follows is registered and woken, with no session.start', async ($, on) => {
  const w = world(on)
  await start($, w)
  await turn($, w, 't-1', 'work')
  await $.session.end({ reason: 'clear', sessionId: SESSION, resume: { id: SESSION } })
  expect(w.of('DELETE', `/sessions/${SESSION}`)).toHaveLength(1)
  // Until the engine names the new id the ended one is not registered again.
  const before = w.of('POST', '/sessions').length
  await w.clock.advance(4000)
  expect(w.of('POST', '/sessions')).toHaveLength(before)
  expect(w.of('GET', '/unread').every(c => c.query.session === SESSION)).toBeTruthy()

  w.sessionId = 'sess-2'
  await $.classic.SessionStart({ source: 'clear', session_id: 'sess-2' })
  expect(w.last('POST', '/sessions')?.body).toMatchObject({ session_id: 'sess-2', idle: true, native: true })
  w.unread.push(PEER)
  await w.clock.advance(2000)
  expect(w.last('POST', '/claim')?.body).toMatchObject({ session_id: 'sess-2', ids: ['m-1'] })
  expect(w.prompts).toHaveLength(1)
  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-2' })
  await step($, 't-2', 0)
  expect(w.last('POST', '/ack')?.body).toEqual({ ids: ['m-1'], session_id: 'sess-2' })
})

test('the id the process goes on under is taken up even when no event names it', async ($, on) => {
  const w = world(on)
  await start($, w)
  await $.session.end({ reason: 'clear', sessionId: SESSION, resume: { id: SESSION } })
  w.sessionId = 'sess-3'
  await w.clock.advance(2000)
  expect(w.last('POST', '/sessions')?.body).toMatchObject({ session_id: 'sess-3', native: true })
  expect(w.last('GET', '/unread')?.query.session).toBe('sess-3')
})

test('a wake on its way when a /clear came is the next session\'s: read and acknowledged there, never handed over twice', async ($, on) => {
  const w = world(on, [PEER])
  await start($, w)
  expect(w.prompts).toHaveLength(1)
  await $.session.end({ reason: 'clear', sessionId: SESSION, resume: { id: SESSION } })
  w.sessionId = 'sess-2'
  await $.classic.SessionStart({ source: 'clear', session_id: 'sess-2' })
  // Its prompt runs in the session that follows; the node gave the message
  // back when the old one ended, and this module does not take it a second time.
  await w.clock.advance(4000)
  expect(w.prompts).toHaveLength(1)
  expect(w.session.appended()).toEqual([])
  expect(w.last('POST', '/claim')?.body).toMatchObject({ ids: ['m-1'], session_id: 'sess-2' })
  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-9' })
  await step($, 't-9', 0)
  expect(w.last('POST', '/ack')?.body).toEqual({ ids: ['m-1'], session_id: 'sess-2' })
  expect(w.of('POST', '/ack')).toHaveLength(1)
})

test('a wake the /clear swallowed is offered again once its time is up', async ($, on) => {
  const w = world(on, [PEER])
  await start($, w)
  await $.session.end({ reason: 'clear', sessionId: SESSION, resume: { id: SESSION } })
  w.sessionId = 'sess-2'
  await $.classic.SessionStart({ source: 'clear', session_id: 'sess-2' })
  await w.clock.advance(60_000)
  expect(w.prompts).toHaveLength(1)
  await w.clock.advance(62_000)
  expect(w.released()).toEqual(['m-1'])
  expect(w.prompts).toHaveLength(2)
})
test('when the engine replaces the module mid-session, what it was handed is still known and acknowledged', async ($, on) => {
  const w = world(on, [PEER])
  // What `$.state` kept of the copy that was lost: the session, a wake on its way.
  const kept: Record<string, unknown> = {
    link: { api: '127.0.0.1:1', project: '', sessionId: SESSION, folder: FOLDER, isRegistered: true, isAttended: true, isGreeted: true },
    deliveries: [{ token: 'tok-kept-1', mode: 'wake', keys: ['|m-1'], chats: { 'c-1': 'm-1' }, claim: 'wake', at: 0, claimedAt: 0 }],
    turn: { id: '', step: -1 },
  }
  on('state.get', (_$, e, next) =>
    e.plugin === 'agent-link' && kept[e.key] !== undefined ? { value: { value: kept[e.key], version: 1 } } : next(e),
  )
  // session.start runs again in the fresh copy.
  await start($, w)
  expect(w.last('POST', '/sessions')?.body).toMatchObject({ session_id: SESSION, native: true })
  // The message is the lost copy's wake: not handed over a second time.
  expect(w.prompts).toEqual([])
  expect(w.of('POST', '/claim')).toEqual([])
  await $.turn.start({ text: 'wake [tok-kept-1] id m-1', turnId: 't-1' })
  await step($, 't-1', 0)
  expect(w.acked()).toEqual(['m-1'])
})

test('the end of the session ends it at the node and stops the loop', async ($, on) => {
  const w = world(on)
  await start($, w)
  await $.session.end({ reason: 'prompt_input_exit', sessionId: SESSION, resume: { id: SESSION } })
  expect(w.of('DELETE', `/sessions/${SESSION}`)).toHaveLength(1)
  expect(w.status()).toBeUndefined()
  const before = w.calls.length
  await w.clock.advance(120_000)
  expect(w.calls).toHaveLength(before)
})
