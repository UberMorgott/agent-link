import type { Engine } from 'claude-code/testing'
import { expect, test } from 'claude-code/testing'

import { FOLDER, SESSION, world } from './node'

async function step($: Engine, turnId: string, index: number): Promise<void> {
  const stream = $.turn.step({ turnId, index, model: 'test-model', messageCount: 1 })
  for await (const chunk of stream) void chunk
}

test('a wake is acknowledged at the first step of the turn its prompt started', async ($, on) => {
  const w = world(on, [{ id: 'm-1', from: 'peer', body: 'ping', chat_id: 'c-1', asks_you: true }])
  await $.session.start({ cwd: FOLDER, surface: null, isInteractive: false })
  await w.clock.settle()
  expect(w.prompts).toHaveLength(1)

  // Another turn (the person's own prompt) proves nothing about the wake.
  await $.turn.start({ text: 'something else', turnId: 't-0' })
  await step($, 't-0', 0)
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-0', reason: 'answer' })
  expect(w.acked()).toEqual([])

  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-1' })
  expect(w.acked()).toEqual([])
  await step($, 't-1', 0)

  expect(w.of('POST', '/ack')).toHaveLength(1)
  expect(w.of('POST', '/ack')[0]?.body).toEqual({ ids: ['m-1'], session_id: SESSION })
  await step($, 't-1', 1)
  expect(w.of('POST', '/ack')).toHaveLength(1)
})

test('an appended message is acknowledged at the next step, or wakes the session when none came', async ($, on) => {
  const w = world(on)
  await $.session.start({ cwd: FOLDER, surface: null, isInteractive: false })
  await w.clock.settle()
  await $.turn.start({ text: 'work', turnId: 't-1' })
  await step($, 't-1', 0)

  w.unread.push({ id: 'm-2', from: 'peer', body: 'mid-turn', chat_id: 'c-1' })
  await w.clock.advance(2000)
  expect(w.session.appended()).toHaveLength(1)
  expect(w.acked()).toEqual([])

  await step($, 't-1', 1)
  expect(w.acked()).toEqual(['m-2'])

  // Appended while the turn's last step was in flight: no request carried it.
  w.unread.push({ id: 'm-3', from: 'peer', body: 'too late', chat_id: 'c-1' })
  await w.clock.advance(2000)
  expect(w.session.appended()).toHaveLength(2)
  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-1', reason: 'answer' })
  await w.clock.settle()

  expect(w.acked()).toEqual(['m-2'])
  expect(w.prompts).toHaveLength(1)
  expect(w.prompts[0]).toContain('id m-3')
})
