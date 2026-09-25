import { beforeEach, describe, expect, it } from 'vitest'
import {
  ACTIVITY_EXPIRE_MS, CONCURRENT_MS, activityLines, activityText, agentTree, attemptText, keepLastKnown, liveJobs, presenceLines, ticksFor,
  type ActivityLine,
} from './chat'
import { runtime } from './runtime'
import type { ChatInfo, ChatMember, ChatMessage, Delivery, Job } from '@/types'

describe('delivery attempts', () => {
  beforeEach(() => {
    runtime.strings = {
      'inbox.tick.delivered': 'доставлено', 'inbox.tick.read': 'прочитано',
      'inbox.attempt.wake_requested': 'разбужена', 'inbox.attempt.launch_requested': 'открывается',
      'inbox.attempt.launch_failed': 'не открылась ({reason})', 'inbox.attempt.needs_human': 'нужен человек',
    }
  })
  const out = (d: Delivery): ChatMessage => ({ id: 'm1', direction: 'out', from: 'me', created_at: '', delivery: [d] }) as unknown as ChatMessage

  it('names the latest attempt until the message is read', () => {
    expect(attemptText({ peer: 'bob', status: 'sent', state: 'delivered', attempt: 'wake_requested' })).toBe('разбужена')
    expect(attemptText({ peer: 'bob', status: 'sent', state: 'delivered', attempt: 'launch_failed:no_agent' })).toBe('не открылась (no_agent)')
    expect(attemptText({ peer: 'bob', status: 'sent', state: 'read', attempt: 'wake_requested' })).toBe('')
    expect(attemptText({ peer: 'bob', status: 'sent', state: 'delivered', attempt: 'future_event' })).toBe('')
  })

  it('shows a failed attempt as a hold, not a delivered tick', () => {
    const waking = ticksFor(out({ peer: 'bob', status: 'sent', state: 'delivered', attempt: 'launch_requested' }), null)!
    expect(waking.state).toBe('delivered')
    expect(waking.label).toBe('bob: доставлено — открывается')
    const failed = ticksFor(out({ peer: 'bob', status: 'sent', state: 'delivered', attempt: 'launch_failed:timeout' }), null)!
    expect(failed.state).toBe('held')
    expect(failed.label).toBe('bob: доставлено — не открылась (timeout)')
    expect(ticksFor(out({ peer: 'bob', status: 'sent', state: 'delivered', attempt: 'needs_human' }), null)!.state).toBe('held')
    expect(ticksFor(out({ peer: 'bob', status: 'sent', state: 'read', attempt: 'needs_human' }), null)!.state).toBe('read')
  })

  it('puts the attempt under the chat instead of presence', () => {
    const info = { id: 'c', members: [{ name: 'bob', connected: true, presence: { session: 'rewake' } }] } as unknown as ChatInfo
    expect(presenceLines(info, [out({ peer: 'bob', status: 'sent', state: 'delivered', attempt: 'wake_requested' })]))
      .toEqual([{ name: 'bob', text: 'разбужена' }])
  })
})

const job = (type: string, text: string): Job => ({ reply_to: 'm1', job_status: 'running', activity_info: { type, text } }) as Job
const count = (s: string, word: string) => s.split(word).length - 1

describe('activityText', () => {
  beforeEach(() => {
    runtime.strings = {
      'inbox.activity.type.thinking': 'думает', 'inbox.activity.type.edit': 'правит',
      'inbox.activity.type.read': 'читает',
    }
  })

  it('names the verb once when the text already starts with it', () => {
    expect(activityText(job('thinking', 'думает'))).toBe('думает')
    expect(activityText(job('edit', 'правит app.go'))).toBe('правит app.go')
    expect(activityText(job('read', 'читает сообщения'))).toBe('читает сообщения')
    for (const [type, text, verb] of [['thinking', 'думает', 'думает'], ['edit', 'правит x.go', 'правит'], ['read', 'читает y', 'читает']]) {
      expect(count(activityText(job(type!, text!)), verb!)).toBe(1)
    }
  })

  it('prefixes the verb to a bare step and shows a type-only step as the verb', () => {
    expect(activityText(job('edit', 'app.go'))).toBe('правит app.go')
    expect(activityText(job('thinking', 'thinking'))).toBe('думает')
  })

  it('never puts a second verb before an older peer\'s own-verb text', () => {
    // v0.6.5 hooks sent «читает сообщения» and «запускает go» as type thinking.
    expect(activityText(job('thinking', 'читает сообщения'))).toBe('читает сообщения')
    expect(activityText(job('thinking', 'запускает go'))).toBe('запускает go')
    expect(activityText(job('tool', 'работает: X'))).toBe('работает: X')
  })
})

describe('running jobs expire', () => {
  const at = Date.parse('2026-09-24T15:00:00Z')
  const running = (heard: string, status = 'running'): Job => ({ reply_to: 'm1', job_status: status, heard_at: heard }) as Job
  const member = (jobs: Job[]): ChatMember => ({ name: 'KPECTIK', connected: true, compatible: true, jobs }) as ChatMember
  const info = (jobs: Job[]): ChatInfo => ({ id: 'c1', participants: ['KPECTIK', 'me'], members: [member(jobs)] }) as unknown as ChatInfo

  it('drops a running job not heard of for ACTIVITY_EXPIRE_MS, keeps a queued one', () => {
    const fresh = running(new Date(at - 60_000).toISOString())
    const quiet = running(new Date(at - ACTIVITY_EXPIRE_MS).toISOString())
    const queued = running(new Date(at - ACTIVITY_EXPIRE_MS * 2).toISOString(), 'queued')
    expect(liveJobs([fresh, quiet, queued], at)).toEqual([fresh, queued])
    expect(activityLines(info([quiet]), [], 'me', [], null, at)).toEqual([])
  })

  it('times a running line by its last news, not by its request', () => {
    const heard = new Date(at - 12_000).toISOString()
    const [row] = activityLines(info([running(heard)]), [], 'me', [], null, at)
    expect(row!.heard).toBe(heard)
    expect(row!.cls).toBe('running')
  })
})

describe('every agent of the chat, own ones too', () => {
  const at = Date.parse('2026-09-24T15:00:00Z')
  const heard = new Date(at - 5_000).toISOString()
  const sjob = (session: string, text: string, reply = 'm1'): Job =>
    ({ reply_to: reply, job_status: 'running', heard_at: heard, activity_info: { type: 'edit', text, session } }) as Job
  beforeEach(() => {
    runtime.strings = { 'inbox.author.own_agent': 'ваш агент', 'inbox.author.agent': 'агент {name}', 'inbox.activity.type.edit': 'правит' }
  })

  it('shows this node\'s own sessions, one line each, named by session only when there are several', () => {
    const info = {
      id: 'c1', participants: ['KPECTIK', 'me'], members: [
        { name: 'KPECTIK', connected: true, compatible: true, jobs: [sjob('', 'правит a.go')] },
        { name: 'me', self: true, connected: true, compatible: true, jobs: [sjob('682d3b39', 'правит x.go'), sjob('59e3bac2', 'правит y.go', 'm2')] },
      ],
    } as unknown as ChatInfo
    const rows = activityLines(info, [], 'me', [], null, at)
    expect(rows.map((r) => r.who + ' ' + r.text)).toEqual([
      'агент KPECTIK правит a.go', 'ваш агент (682d3b39) правит x.go', 'ваш агент (59e3bac2) правит y.go',
    ])
    expect(new Set(rows.map((r) => r.key)).size).toBe(3)
    const one = { ...info, members: [{ name: 'me', self: true, connected: true, compatible: true, jobs: [sjob('682d3b39', 'правит x.go')] }] } as unknown as ChatInfo
    expect(activityLines(one, [], 'me', [], null, at).map((r) => r.who)).toEqual(['ваш агент'])
  })
})

describe('agent tree', () => {
  const at = Date.parse('2026-09-24T15:00:00Z')
  const ago = (ms: number) => new Date(at - ms).toISOString()
  const tjob = (reply: string, heard: number, info: Job['activity_info']): Job =>
    ({ reply_to: reply, job_status: 'running', heard_at: ago(heard), activity_info: { type: 'edit', ...info } }) as Job
  const chat = (jobs: Job[], connected = true): ChatInfo => ({
    id: 'c1', participants: ['bob', 'me'], members: [{ name: 'bob', connected, compatible: true, jobs }],
  }) as unknown as ChatInfo
  beforeEach(() => {
    runtime.strings = {
      'inbox.author.agent': 'агент {name}', 'inbox.activity.type.edit': 'правит', 'inbox.activity.subagent': 'субагент',
      'inbox.activity.last': 'последнее: {text}',
    }
  })

  it('collapses the jobs of one session into one main row, the latest', () => {
    const jobs = [tjob('m1', 30_000, { text: 'a.go', session: 's1' }), tjob('m2', 5_000, { text: 'b.go', session: 's1' }), tjob('m3', 20_000, { text: 'c.go', session: 's1' })]
    const tree = agentTree(jobs, at)
    expect(tree).toHaveLength(1)
    expect(tree[0]!.job).toBe(jobs[1])
    const rows = activityLines(chat(jobs), [], 'me', [], null, at)
    expect(rows.map((r) => r.who + ' ' + r.text)).toEqual(['агент bob правит b.go'])
  })

  it('without role fields keeps the old lines, deduped', () => {
    const jobs = [tjob('m1', 5_000, { text: 'a.go' }), tjob('m2', 8_000, { text: 'b.go' })]
    expect(activityLines(chat(jobs), [], 'me', [], null, at).map((r) => r.who + ' ' + r.text)).toEqual(['агент bob правит a.go'])
  })

  it('nests subagents under their parent session, one per agent id', () => {
    const jobs = [
      tjob('m1', 5_000, { text: 'main.go', session: 'abcd1234', role: 'main' }),
      tjob('m1', 9_000, { text: 'x.go', parent_session: 'abcd1234-ffff', agent_id: 'A', role: 'subagent', label: 'Explore' }),
      tjob('m1', 3_000, { text: 'y.go', parent_session: 'abcd1234-ffff', agent_id: 'A', role: 'subagent', label: 'Explore' }),
      tjob('m1', 4_000, { text: 'z.go', session: 'abcd1234', parent_session: 'abcd1234-ffff', agent_id: 'B', role: 'subagent' }),
    ]
    const rows = activityLines(chat(jobs), [], 'me', [], null, at)
    expect(rows).toHaveLength(1)
    expect(rows[0]!.text).toBe('правит main.go')
    expect(rows[0]!.children!.map((c) => c.who + ' ' + c.text)).toEqual(['Explore правит y.go', 'субагент правит z.go'])
  })

  it('shows another session only while it is concurrently active', () => {
    const old = tjob('m1', CONCURRENT_MS + 1_000, { text: 'old.go', session: 's1' })
    const cur = tjob('m2', 2_000, { text: 'new.go', session: 's2' })
    expect(agentTree([old, cur], at).map((g) => g.session)).toEqual(['s2'])
    const both = tjob('m1', 10_000, { text: 'old.go', session: 's1' })
    expect(activityLines(chat([both, cur]), [], 'me', [], null, at).map((r) => r.who)).toEqual(['агент bob (s2)', 'агент bob (s1)'])
  })

  it('keeps the last running line as idle while the member stays connected', () => {
    const memory = new Map<string, ActivityLine>()
    const running = chat([tjob('m1', 5_000, { text: 'a.go', session: 's1' })])
    expect(keepLastKnown(activityLines(running, [], 'me', [], null, at), running, memory)).toHaveLength(1)
    const done = chat([])
    const [idle] = keepLastKnown(activityLines(done, [], 'me', [], null, at), done, memory)
    expect(idle!.cls).toBe('idle')
    expect(idle!.text).toBe('последнее: правит a.go')
    expect(idle!.heard).toBe(ago(5_000))
    expect(keepLastKnown([], chat([], false), memory)).toEqual([])
    expect(memory.size).toBe(0)
  })
})
