import { beforeEach, describe, expect, it } from 'vitest'
import { ACTIVITY_EXPIRE_MS, activityLines, activityText, liveJobs } from './chat'
import { runtime } from './runtime'
import type { ChatInfo, ChatMember, Job } from '@/types'

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
