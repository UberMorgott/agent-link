import { beforeEach, describe, expect, it } from 'vitest'
import { activityText } from './chat'
import { runtime } from './runtime'
import type { Job } from '@/types'

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
})
