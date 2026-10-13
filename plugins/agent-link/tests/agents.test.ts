import { expect, test } from 'claude-code/testing'

import type { Call, World } from './node'
import { inAgent, SESSION, start, step, TOOL, world } from './node'

const PEER = { id: 'm-1', from: 'peer', body: 'ping', chat_id: 'c-1', asks_you: true }
const REPLY = { id: 'r-1', from: 'peer', body: 'for the subagent', chat_id: 'c-1', for_agent: 'a-1' }

const activity = (w: World): Call[] => w.calls.filter(c => c.path.endsWith('/activity'))
const doing = (w: World): Record<string, unknown>[] => w.of('POST', `/sessions/${SESSION}/doing`).map(c => c.body)

test('a reply for a live subagent reaches it after its tool call, and the main agent only once it ended', async ($, on) => {
  const w = world(on)
  await start($, w)
  await $.turn.start({ text: 'work', turnId: 't-1' })
  await step($, 't-1', 0)

  await $.classic.SubagentStart({ agent_id: 'a-1', agent_type: 'Explore' })
  // The node hears the live ones at once: no wake of the session takes their replies.
  expect(w.last('POST', '/sessions')?.body).toMatchObject({ agents: ['a-1'], idle: false })

  // The subagent asks: the node records who did.
  await $.tool.call({ tool: `${TOOL}send`, chat: 'c-1', body: 'a question', ask: ['peer'], agentId: 'a-1' })
  expect(w.last('POST', '/send')?.body).toMatchObject({ session_id: SESSION, agent_id: 'a-1', agent_type: 'Explore', ask: ['peer'] })

  w.unread.push(REPLY)
  await w.clock.advance(2000)
  // The main agent's listing leaves the live subagent's replies out.
  expect(w.last('GET', '/unread')?.query.skip_agents).toBe('a-1')
  expect(w.session.appended()).toEqual([])

  const ran = await inAgent($, 'a-1', { tool: 'Read', file_path: 'a.go' })
  expect(w.last('GET', '/unread')?.query).toMatchObject({ for_agent: 'a-1', session: SESSION, native: '1' })
  expect(w.last('POST', '/render')?.body).toMatchObject({ agent: 'a-1', mode: '' })
  expect(ran.context).toEqual(['batch id r-1'])
  // Read with that result: acknowledged at once, never delivered again.
  expect(w.acked()).toEqual(['r-1'])
  const again = await inAgent($, 'a-1', { tool: 'Read', file_path: 'b.go' })
  expect(again.context).toBeUndefined()

  // It ended: a late reply is the main agent's, and the node is told it ended
  // (the node marks the reply as an ended subagent's).
  await $.classic.SubagentStop({ agent_id: 'a-1', agent_type: 'Explore', stop_hook_active: false, agent_transcript_path: '' })
  expect(w.last('POST', '/sessions')?.body).toMatchObject({ agents: [] })
  w.unread.push({ ...REPLY, id: 'r-2' })
  await w.clock.advance(2000)
  expect(w.last('GET', '/unread')?.query.skip_agents).toBeUndefined()
  expect(w.last('POST', '/render')?.body).toMatchObject({ live_agents: [] })
  expect(w.session.appended()).toHaveLength(1)
})

test('the node is told what the main agent does, by the tool\'s name alone', async ($, on) => {
  const w = world(on)
  await start($, w)
  await $.turn.start({ text: 'work', turnId: 't-1' })
  await $.tool.call({ tool: 'Read', file_path: 'C:\\work\\demo\\a.go' })
  await $.tool.call({ tool: 'Bash', command: 'go test ./...' })
  await $.tool.call({ tool: 'Bash', command: 'agentlink discuss --with codex --body-file q.md' })
  await $.tool.call({ tool: `${TOOL}discuss`, with: 'codex', body: 'q', async: true })
  expect(doing(w)).toEqual([
    { tool: 'Read' },
    { doing: 'thinking' },
    // A shell command's line stays here unless it asks another agent.
    { tool: 'Bash' },
    { doing: 'thinking' },
    { tool: 'Bash', input: { command: 'agentlink discuss --with codex --body-file q.md' } },
    { doing: 'thinking' },
    { tool: `${TOOL}discuss` },
    { doing: 'thinking' },
  ])
  // No chat is worked on: no activity line anywhere.
  expect(activity(w)).toEqual([])
})

test('the chats a delivery came from see what the session does, until its turn ends', async ($, on) => {
  const w = world(on, [PEER])
  await start($, w)
  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-1' })
  expect(activity(w)).toEqual([])
  await step($, 't-1', 0)
  // Read: the chat hears it, for the message the session works on.
  expect(activity(w).map(c => [c.path, c.body])).toEqual([
    ['/chats/c-1/activity', { session_id: SESSION, reply_to: 'm-1', event: 'reading' }],
  ])

  await w.clock.advance(1000)
  await $.tool.call({ tool: 'Edit', file_path: 'C:\\work\\demo\\pkg\\a.go', old_string: 'secret', new_string: 'x' })
  // The node words it from the tool and the path: nothing else of the input leaves.
  expect(activity(w).at(-1)?.body).toEqual({
    session_id: SESSION,
    reply_to: 'm-1',
    tool: 'Edit',
    input: { file_path: 'C:\\work\\demo\\pkg\\a.go' },
  })
  // The same again within moments is one line, not two.
  await w.clock.advance(1000)
  await $.tool.call({ tool: 'Edit', file_path: 'C:\\work\\demo\\pkg\\a.go', old_string: 'y', new_string: 'z' })
  expect(activity(w)).toHaveLength(2)

  await $.turn.complete({ answer: 'ok', durationMs: 1, isAborted: false, turnId: 't-1', reason: 'answer' })
  expect(activity(w).at(-1)?.body).toEqual({ session_id: SESSION, reply_to: 'm-1', event: 'done', phase: 'idle' })
  const before = activity(w).length
  await $.turn.start({ text: 'next', turnId: 't-2' })
  await $.tool.call({ tool: 'Read', file_path: 'a.go' })
  expect(activity(w)).toHaveLength(before)
})

test('a chat the session wrote in sees what it does next', async ($, on) => {
  const w = world(on)
  await start($, w)
  await $.turn.start({ text: 'work', turnId: 't-1' })
  await $.tool.call({ tool: `${TOOL}send`, chat: 'c-7', body: 'on it' })
  expect(activity(w).map(c => [c.path, c.body])).toEqual([
    ['/chats/c-7/activity', { session_id: SESSION, reply_to: 'm-out', event: 'thinking' }],
  ])
})

test('a subagent has its own line in the chats the session worked on when it started', async ($, on) => {
  const w = world(on, [PEER])
  await start($, w)
  await $.turn.start({ text: w.prompts[0] ?? '', turnId: 't-1' })
  await step($, 't-1', 0)
  const before = activity(w).length

  await $.classic.SubagentStart({ agent_id: 'a-1', agent_type: 'Explore' })
  await inAgent($, 'a-1', { tool: 'Grep', pattern: 'x' })
  await $.classic.SubagentStop({ agent_id: 'a-1', agent_type: 'Explore', stop_hook_active: false, agent_transcript_path: '' })
  const own = { session_id: SESSION, reply_to: 'm-1', agent_id: 'a-1', label: 'Explore' }
  expect(activity(w).slice(before).map(c => c.body)).toEqual([
    { ...own, event: 'agent' },
    { ...own, tool: 'Grep', input: {} },
    { ...own, event: 'done', phase: 'idle' },
  ])
  // The subagent's call is not the main agent's: its state is untouched.
  expect(doing(w)).toEqual([])
})
