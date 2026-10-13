import { expect, test } from 'claude-code/testing'

import { FOLDER, SESSION, world } from './node'

test('an idle session is woken with the unread messages as a prompt', async ($, on) => {
  const w = world(on)
  await $.session.start({ cwd: FOLDER, surface: null, isInteractive: false })
  await w.clock.settle()

  expect(w.of('POST', '/sessions')[0]?.body).toMatchObject({
    session_id: SESSION,
    provider: 'claude',
    folder: FOLDER,
    wake: 'rewake',
    idle: true,
  })
  expect(w.prompts).toHaveLength(0)

  w.unread.push({ id: 'm-1', from: 'peer', body: 'ping from peer', chat_id: 'c-1', asks_you: true })
  await w.clock.advance(2000)

  expect(w.of('GET', '/unread').at(-1)?.query).toMatchObject({
    folder: FOLDER,
    session: SESSION,
    actionable: '1',
    waiter: '1',
  })
  const claim = w.of('POST', '/claim')[0]?.body
  expect(claim).toMatchObject({ ids: ['m-1'], session_id: SESSION })
  expect(w.prompts).toHaveLength(1)
  const prompt = w.prompts[0] ?? ''
  expect(prompt).toContain(`[agent-link wake ${String(claim?.wake_token)}]`)
  expect(prompt).toContain('От peer')
  expect(prompt).toContain('в чате c-1')
  expect(prompt).toContain('id m-1')
  expect(prompt).toContain('ping from peer')
  expect(prompt).toContain('mcp__agent-link-native__send (chat_id c-1, reply_to m-1)')
  expect(w.status()).toBe('agent-link: 1 unread')

  // Still unread at the node, and not handed over twice while it waits.
  await w.clock.advance(6000)
  expect(w.prompts).toHaveLength(1)
  expect(w.acked()).toEqual([])
})
