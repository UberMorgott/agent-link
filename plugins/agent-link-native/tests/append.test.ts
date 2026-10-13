import { expect, test } from 'claude-code/testing'

import { FOLDER, world } from './node'

test('a message that arrives mid-turn is appended to the running turn, not submitted', async ($, on) => {
  const w = world(on)
  await $.session.start({ cwd: FOLDER, surface: null, isInteractive: false })
  await w.clock.settle()
  await $.turn.start({ text: 'do the work', turnId: 't-1' })

  w.unread.push({ id: 'm-2', from: 'peer', body: 'while you work', chat_id: 'c-1' })
  await w.clock.advance(2000)

  expect(w.prompts).toHaveLength(0)
  const rows = w.session.appended()
  expect(rows).toHaveLength(1)
  expect(rows[0]?.message.type).toBe('user')
  expect(JSON.stringify(rows[0]?.message.content)).toContain('id m-2')
  expect(JSON.stringify(rows[0]?.message.content)).toContain('while you work')
  // A plain claim: no wake token, since no prompt of its own carries it.
  expect(w.of('POST', '/claim')[0]?.body.wake_token).toBeUndefined()

  // Not appended again at the next poll.
  await w.clock.advance(4000)
  expect(w.session.appended()).toHaveLength(1)
  expect(w.acked()).toEqual([])
})

test('the tools are served by the module from the node', async ($, on) => {
  const w = world(on, [{ id: 'm-9', from: 'peer', body: 'read me', chat_id: 'c-1' }])
  await $.session.start({ cwd: FOLDER, surface: null, isInteractive: false })
  await $.turn.start({ text: 'hi', turnId: 't-1' })

  const sent = await $.tool.call({
    tool: 'mcp__agent-link-native__send',
    body: 'pong',
    chat_id: 'c-1',
    reply_to: 'm-9',
  })
  expect(sent.result).toBe('agent-link: sent, id m-out, chat c-1')
  expect(w.of('POST', '/send')[0]?.body).toMatchObject({
    body: 'pong',
    chat_id: 'c-1',
    reply_to: 'm-9',
    folder: FOLDER,
  })

  const listed = await $.tool.call({ tool: 'mcp__agent-link-native__unread' })
  expect(String(listed.result)).toContain('read me')
  expect(w.acked()).toContain('m-9')
})
