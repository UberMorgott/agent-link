// What was handed over and has not been read yet: a prompt that never
// entered, a prompt that waits behind another turn, a node that cannot word a
// delivery. Nothing is acknowledged that no model request read, and nothing
// reaches the session twice.
import { expect, test } from 'claude-code/testing'

import { appended, start, step, turn, world } from './node'
import type { World } from './node'

const PEER = { id: 'm-1', from: 'peer', body: 'ping', chat_id: 'c-1', asks_you: true }

/** The node lists its unread only for a turn (the person's prompt), never as a wake. */
const forTurnsOnly = (w: World): void => {
  w.answer.set('/unread', c => ({
    status: 200,
    body: c.query.waiter === '1' ? { messages: [], total: 0 } : { messages: w.unread, total: w.unread.length },
  }))
}

const wakeClaims = (w: World): string[] =>
  w.of('POST', '/claim').flatMap(c => (typeof c.body.wake_token === 'string' ? [c.body.wake_token] : []))

test('a person\'s prompt a hook beneath dropped gives its messages back: the next turn acknowledges none of them', async ($, on) => {
  const w = world(on, [PEER])
  forTurnsOnly(w)
  await start($, w)
  w.dropPrompts = 'a UserPromptSubmit settings hook blocked it'
  const first = await $.prompt.submit({ text: 'do the thing', wait: false, origin: { kind: 'composer' } })
  expect(first.drop).toBe('a UserPromptSubmit settings hook blocked it')
  expect(w.prompts).toEqual([])
  // Nothing read it: it is the node's to hand over again.
  expect(w.released()).toEqual(['m-1'])
  w.dropPrompts = undefined
  // The person types again; the node keeps the message back for now (its
  // lease's backoff), so this prompt carries nothing.
  w.answer.set('/unread', () => ({ status: 200, body: { messages: [], total: 0 } }))
  await w.clock.advance(10_000)
  await $.prompt.submit({ text: 'second try', wait: false, origin: { kind: 'composer' } })
  await $.turn.start({ text: 'second try', turnId: 't-1' })
  await step($, 't-1', 0)
  expect(w.prompts).toEqual(['second try\nCLI']) // the session's first text, which the dropped one was not
  expect(appended(w)).toEqual([])
  // Nothing the model read names m-1: it is not acknowledged.
  expect(w.acked()).toEqual([])
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-1', reason: 'answer' })
  // Offered again, it comes with the next prompt and is read once.
  forTurnsOnly(w)
  await $.prompt.submit({ text: 'third try', wait: false, origin: { kind: 'composer' } })
  expect(w.prompts.at(-1)).toBe('third try\nbatch id m-1')
  await $.turn.start({ text: 'third try', turnId: 't-2' })
  await step($, 't-2', 0)
  expect(w.acked()).toEqual(['m-1'])
  expect(w.released()).toEqual(['m-1'])
})

test('what a person\'s prompt carried is acknowledged by the turn that prompt started, not by another', async ($, on) => {
  const w = world(on, [PEER])
  forTurnsOnly(w)
  await start($, w)
  // The prompt entered; another turn (a scheduled one, a peer's) starts first.
  const entered = await $.prompt.submit({ text: 'do the thing', wait: false, origin: { kind: 'composer' } })
  expect(entered.drop).toBeUndefined()
  await $.turn.start({ text: 'the nightly check', turnId: 't-other' })
  await step($, 't-other', 0)
  expect(w.acked()).toEqual([])
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-other', reason: 'answer' })
  expect(w.released()).toEqual([])
  await $.turn.start({ text: 'do the thing', turnId: 't-own' })
  await step($, 't-own', 0)
  expect(w.acked()).toEqual(['m-1'])
})

test('a wake queued behind the person\'s turn stays the wake\'s however long the turn takes: kept alive at the node, appended to no turn', async ($, on) => {
  const w = world(on, [PEER])
  await start($, w)
  expect(w.prompts).toHaveLength(1) // the wake prompt, submitted
  const [token = ''] = wakeClaims(w)
  expect(w.prompts[0]).toContain(token)
  // The person's own turn started first: the wake prompt waits behind it.
  await $.turn.start({ text: 'long work of the person', turnId: 't-0' })
  await step($, 't-0', 0)
  // Well past the node's grace for a wake (the node here lapses it as the
  // real one does): the same wake is taken again, so the running turn is
  // never listed or handed the message.
  await w.clock.advance(125_000)
  expect(w.wakeHeld('m-1')).toBe(true)
  expect(new Set(wakeClaims(w))).toEqual(new Set([token]))
  expect(wakeClaims(w).length).toBeGreaterThan(2)
  await w.clock.advance(600_000)
  expect(w.wakeHeld('m-1')).toBe(true)
  expect(appended(w)).toEqual([])
  expect(w.prompts).toHaveLength(1)
  expect(w.released()).toEqual([])
  expect(w.acked()).toEqual([])
  // The person's turn ends, the queued prompt runs: read once.
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-0', reason: 'answer' })
  await w.clock.settle()
  await turn($, w, 't-1', w.prompts[0] ?? '')
  expect(w.acked()).toEqual(['m-1'])
  expect(w.prompts).toHaveLength(1)
  expect(appended(w)).toEqual([])
})

test('a wake whose prompt has not entered yet is not given up; one that entered and started no turn is', async ($, on) => {
  const w = world(on, [PEER])
  let open = (): void => undefined
  w.promptGate = new Promise<void>(resolve => {
    open = resolve
  })
  await start($, w)
  await w.clock.advance(300_000)
  // Still on its way in: one wake, kept alive, nothing handed over again.
  expect(w.of('POST', '/render').filter(c => c.body.mode === 'wake')).toHaveLength(1)
  expect(w.wakeHeld('m-1')).toBe(true)
  expect(w.prompts).toEqual([])
  open()
  w.promptGate = undefined
  await w.clock.settle()
  expect(w.prompts).toHaveLength(1)
  // It entered the idle session and no turn started with it (a /clear took
  // it): after the wake's time the node offers the message again.
  await w.clock.advance(200_000)
  expect(w.prompts).toHaveLength(2)
  expect(w.prompts[1]).toContain('id m-1')
  expect(w.acked()).toEqual([])
})

test('a node that cannot word a delivery (older than POST /render) is handed nothing by the module: its shell hooks deliver', async ($, on) => {
  const w = world(on, [PEER])
  w.answer.set('/render', () => ({ status: 404 }))
  await start($, w)
  await w.clock.advance(10_000)
  // Idle: no wake, and nothing claimed away from the shell hooks.
  expect(w.prompts).toEqual([])
  expect(w.of('POST', '/claim')).toEqual([])
  // The person's own prompt goes in as typed.
  const entered = await $.prompt.submit({ text: 'do the thing', wait: false, origin: { kind: 'composer' } })
  expect(entered.drop).toBeUndefined()
  expect(w.prompts).toEqual(['do the thing'])
  // Mid-turn: nothing appended.
  await $.turn.start({ text: 'do the thing', turnId: 't-1' })
  await step($, 't-1', 0)
  await w.clock.advance(10_000)
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-1', reason: 'answer' })
  await w.clock.settle()
  expect(appended(w)).toEqual([])
  expect(w.of('POST', '/claim')).toEqual([])
  expect(w.acked()).toEqual([])
  expect(w.released()).toEqual([])
  expect(w.unread).toHaveLength(1)
})
