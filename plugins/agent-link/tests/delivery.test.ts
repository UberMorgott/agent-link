import { expect, test } from 'claude-code/testing'

import { appended, SESSION, start, step, turn, world } from './node'

const PEER = { id: 'm-1', from: 'peer', body: 'ping', chat_id: 'c-1', asks_you: true }

test('an idle session is woken with the node\'s own text, asked for as its waiter', async ($, on) => {
  const w = world(on, [PEER, { id: 'm-2', from: 'peer', body: 'held', chat_id: 'c-2', asks_you: true, paused: true }])
  await start($, w)

  // The wake: what waits for an idle session, the held requests as information.
  const asked = w.of('GET', '/unread')[0]
  expect(asked?.query).toMatchObject({ session: SESSION, native: '1', actionable: '1', waiter: '1', wait: '25' })
  // The node holds the claim under the wake's token and words the prompt.
  const claimed = w.last('POST', '/claim')
  const token = String(claimed?.body.wake_token)
  expect(claimed?.body).toMatchObject({ ids: ['m-1', 'm-2'], session_id: SESSION, native: true })
  const said = w.last('POST', '/render')
  expect(said?.body).toMatchObject({ mode: 'wake', token, session_id: SESSION })
  // The messages go to the node as it listed them: this module words none.
  expect(said?.body.messages).toEqual(w.unread)
  expect(w.prompts).toEqual([`wake [${token}] id m-1, id m-2 held`])
  expect(w.logged).toEqual(['notice 2'])
  expect(w.status()).toBe('agent-link: непрочитанных — 2')
  // Handed over, not yet read: nothing is acknowledged, nothing asked twice.
  await w.clock.advance(6000)
  expect(w.prompts).toHaveLength(1)
  expect(w.acked()).toEqual([])
})

test('a wake is acknowledged at the first model request of the turn its prompt started', async ($, on) => {
  const w = world(on, [PEER])
  await start($, w)
  expect(w.prompts).toHaveLength(1)

  // Another turn (the person's own prompt) proves nothing about the wake.
  await turn($, w, 't-0', 'something else')
  expect(w.acked()).toEqual([])

  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-1' })
  expect(w.acked()).toEqual([])
  await step($, 't-1', 0)
  expect(w.of('POST', '/ack')).toHaveLength(1)
  expect(w.last('POST', '/ack')?.body).toEqual({ ids: ['m-1'], session_id: SESSION })
  await step($, 't-1', 1)
  expect(w.of('POST', '/ack')).toHaveLength(1)
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-1', reason: 'answer' })
  await w.clock.advance(6000)
  expect(w.prompts).toHaveLength(1)
  expect(w.released()).toEqual([])
  expect(w.status()).toBeUndefined()
})

for (const reason of ['aborted', 'error', 'refusal'] as const) {
  test(`a wake whose turn ended (${reason}) before a model request read it is given back and offered again`, async ($, on) => {
    const w = world(on, [PEER])
    await start($, w)
    await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-1' })
    await $.turn.complete(
      reason === 'refusal'
        ? { answer: '', durationMs: 1, isAborted: false, turnId: 't-1', reason, refusal: { category: null, explanation: null } }
        : { answer: '', durationMs: 1, isAborted: reason === 'aborted', turnId: 't-1', reason },
    )
    // Not lost: the node has it back at once.
    expect(w.released()).toEqual(['m-1'])
    expect(w.last('POST', '/release')?.body).toEqual({ ids: ['m-1'], session_id: SESSION })
    expect(w.acked()).toEqual([])
    // Not doubled: one new wake, under another token, and one ack when it is read.
    await w.clock.advance(2000)
    expect(w.prompts).toHaveLength(2)
    expect(w.prompts[1]).not.toBe(w.prompts[0])
    await $.turn.start({ text: w.prompts[1] ?? '', turnId: 't-2' })
    await step($, 't-2', 0)
    expect(w.acked()).toEqual(['m-1'])
    await w.clock.advance(6000)
    expect(w.prompts).toHaveLength(2)
  })
}

test('a turn interrupted after its model request read the wake keeps it read', async ($, on) => {
  const w = world(on, [PEER])
  await start($, w)
  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-1' })
  await step($, 't-1', 0)
  await $.turn.complete({ answer: '', durationMs: 1, isAborted: true, turnId: 't-1', reason: 'aborted' })
  await w.clock.advance(6000)
  expect(w.acked()).toEqual(['m-1'])
  expect(w.released()).toEqual([])
  expect(w.prompts).toHaveLength(1)
})

test('a prompt that did not enter gives its messages back', async ($, on) => {
  const w = world(on, [PEER])
  w.dropPrompts = 'a hook said no'
  await start($, w)
  expect(w.prompts).toEqual([])
  expect(w.released()).toEqual(['m-1'])
})

test('a message that comes mid-turn is appended once and acknowledged at the next model request', async ($, on) => {
  const w = world(on)
  await start($, w)
  expect(w.prompts).toEqual([])
  await $.turn.start({ text: 'work', turnId: 't-1' })
  await step($, 't-1', 0)

  w.unread.push({ id: 'm-2', from: 'peer', body: 'mid-turn', chat_id: 'c-1' })
  await w.clock.advance(2000)
  // Inside a turn everything unread is the turn's: no waiter flags, no wake.
  const asked = w.last('GET', '/unread')
  expect(asked?.query.waiter).toBeUndefined()
  expect(asked?.query.actionable).toBeUndefined()
  expect(w.last('POST', '/claim')?.body).toEqual({ ids: ['m-2'], session_id: SESSION, folder: 'C:\\work\\demo', native: true })
  // The first text of a session names the CLI; the node adds it when asked.
  expect(w.last('POST', '/render')?.body).toMatchObject({ mode: '', cli: true })
  expect(appended(w)).toEqual(['batch id m-2\nCLI'])
  expect(w.prompts).toEqual([])
  expect(w.acked()).toEqual([])

  // Still unread at the node while no request carried it: never appended twice.
  await w.clock.advance(6000)
  expect(appended(w)).toHaveLength(1)

  await step($, 't-1', 1)
  expect(w.acked()).toEqual(['m-2'])
  await step($, 't-1', 2)
  expect(w.of('POST', '/ack')).toHaveLength(1)
})

test('what was appended after a turn\'s last request starts a turn that names it, not repeats it', async ($, on) => {
  const w = world(on)
  await start($, w)
  await $.turn.start({ text: 'work', turnId: 't-1' })
  await step($, 't-1', 0)
  w.unread.push({ id: 'm-3', from: 'peer', body: 'too late', chat_id: 'c-1' })
  await w.clock.advance(2000)
  expect(appended(w)).toHaveLength(1)
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-1', reason: 'answer' })
  await w.clock.settle()

  expect(w.acked()).toEqual([])
  expect(w.released()).toEqual([])
  expect(w.prompts).toHaveLength(1)
  expect(w.prompts[0]).toMatch(/^nudge 1 \[/)
  expect(w.prompts[0]).not.toContain('m-3')
  // Its claim is kept while the turn is on its way: nobody else takes it.
  await w.clock.advance(32_000)
  expect(w.of('POST', '/claim').length).toBeGreaterThan(1)
  expect(w.prompts).toHaveLength(1)
  expect(appended(w)).toHaveLength(1)

  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-2' })
  await step($, 't-2', 0)
  expect(w.acked()).toEqual(['m-3'])
})

test('a turn that failed gives back what it was appended and did not read', async ($, on) => {
  const w = world(on)
  await start($, w)
  await $.turn.start({ text: 'work', turnId: 't-1' })
  await step($, 't-1', 0)
  w.unread.push({ id: 'm-4', from: 'peer', body: 'unlucky', chat_id: 'c-1' })
  await w.clock.advance(2000)
  await $.turn.complete({ answer: '', durationMs: 1, isAborted: false, turnId: 't-1', reason: 'error' })
  expect(w.released()).toEqual(['m-4'])
  expect(w.acked()).toEqual([])
})

test('the person\'s own prompt carries what waits, read by the turn it starts', async ($, on) => {
  const w = world(on, [PEER])
  // Unread the waiter may not wake with (spent wakes): the node lists it to a turn only.
  w.answer.set('/unread', c => ({
    status: 200,
    body: c.query.waiter === '1' ? { messages: [], total: 0 } : { messages: w.unread, total: w.unread.length },
  }))
  await start($, w)
  await w.clock.advance(4000)
  expect(w.prompts).toEqual([])

  const entered = await $.prompt.submit({ text: 'do the thing', wait: false, origin: { kind: 'composer' } })
  expect(entered.drop).toBeUndefined()
  expect(w.prompts).toEqual(['do the thing\nbatch id m-1\nCLI'])
  expect(w.acked()).toEqual([])
  await $.turn.start({ text: 'do the thing', turnId: 't-1' })
  await step($, 't-1', 0)
  expect(w.acked()).toEqual(['m-1'])

  // The CLI's path leads a session once; a prompt with nothing waiting is left alone.
  w.answer.delete('/unread')
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-1', reason: 'answer' })
  await $.prompt.submit({ text: 'and again', wait: false, origin: { kind: 'composer' } })
  expect(w.prompts[1]).toBe('and again')
})

test('the loop and the person\'s own prompt see the same message at once: it is handed over once', async ($, on) => {
  const w = world(on)
  await start($, w)
  w.unread.push(PEER)
  await Promise.all([
    w.clock.advance(2000),
    $.prompt.submit({ text: 'do the thing', wait: false, origin: { kind: 'composer' } }),
  ])
  await w.clock.settle()
  expect(w.prompts.filter(p => p.includes('id m-1'))).toHaveLength(1)
  expect(w.session.appended()).toEqual([])
})
test('one text carries what fits; the rest comes with the next, and another session\'s is left out', async ($, on) => {
  const w = world(on, [
    { id: 'm-1', from: 'peer', body: 'one', chat_id: 'c-1' },
    { id: 'm-2', from: 'peer', body: 'two', chat_id: 'c-1' },
    { id: 'm-3', from: 'peer', body: 'three', chat_id: 'c-1' },
  ])
  w.fit = 2
  w.taken.add('m-2')
  await start($, w)
  // Two fit, one of them is another session's: only the granted one is worded.
  expect(w.of('POST', '/claim')[0]?.body.ids).toEqual(['m-1', 'm-2'])
  expect(w.prompts).toHaveLength(1)
  expect(w.prompts[0]).toMatch(/^wake \[\w+\] id m-1$/)
  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-1' })
  await step($, 't-1', 0)
  expect(w.acked()).toEqual(['m-1'])
})

test('a message the worker took after the session was told about it becomes a note for the model', async ($, on) => {
  const w = world(on, [PEER])
  w.worker.add('m-1')
  await start($, w)
  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-1' })
  await step($, 't-1', 0)
  expect(w.acked()).toEqual(['m-1'])
  // The node's line, as a row of the running turn; no activity for that chat.
  expect(w.last('POST', '/render')?.body).toMatchObject({ messages: [], notes: ['note m-1 c-1'] })
  expect(appended(w)).toEqual(['note m-1 c-1'])
  expect(w.calls.filter(c => c.path.endsWith('/activity'))).toEqual([])
})

test('a node that is down is asked again, and a node that holds no request is polled', async ($, on) => {
  const w = world(on)
  w.isDown = true
  await start($, w)
  expect(w.calls).toEqual([])
  expect(w.status()).toBeUndefined()
  w.isDown = false
  await w.clock.advance(12_000)
  expect(w.of('POST', '/sessions').length).toBeGreaterThan(0)
  // Answered at once (nothing held): one request per poll, not a busy loop.
  const before = w.of('GET', '/unread').length
  await w.clock.advance(10_000)
  expect(w.of('GET', '/unread').length - before).toBe(5)
  // A keep-alive, not an event of the session.
  await w.clock.advance(60_000)
  expect(w.last('POST', '/sessions')?.body).toMatchObject({ heartbeat: true, idle: true, native: true })
})
