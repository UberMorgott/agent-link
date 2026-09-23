import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { browser, runtime } from '@/lib/runtime'
import { parseSSERecord, slicesForTopics, statusLine, useAppStore } from './app'

// eventStream is a response whose body yields the pushed chunks, one read each.
function eventStream(headers: Record<string, string> = {}, status = 200) {
  const encoder = new TextEncoder()
  const waiting: ((r: ReadableStreamReadResult<Uint8Array>) => void)[] = []
  const queued: ReadableStreamReadResult<Uint8Array>[] = []
  const reader = {
    read: () => new Promise<ReadableStreamReadResult<Uint8Array>>((resolve) => {
      const next = queued.shift()
      if (next) resolve(next)
      else waiting.push(resolve)
    }),
  }
  const push = (result: ReadableStreamReadResult<Uint8Array>) => {
    const resolve = waiting.shift()
    if (resolve) resolve(result)
    else queued.push(result)
  }
  return {
    response: { ok: status === 200, status, headers: new Headers(headers), body: { getReader: () => reader } } as unknown as Response,
    send: (event: string) => push({ value: encoder.encode(event), done: false }),
    end: () => push({ value: undefined, done: true }),
  }
}

const change = (topics: string[]) => 'event: change\ndata: ' + JSON.stringify({ revision: 1, topics }) + '\n\n'

beforeEach(() => setActivePinia(createPinia()))

describe('event parsing', () => {
  it('reads change records only', () => {
    expect(parseSSERecord('event: change\ndata: {"topics":["chats"]}')).toEqual({ topics: ['chats'] })
    expect(parseSSERecord('event: hello\ndata: {}')).toBeNull()
    expect(parseSSERecord('event: change\r\ndata: nope')).toBeNull()
  })

  it('maps topics onto the slices they make stale', () => {
    expect(slicesForTopics(['all']).sort()).toEqual(['chats', 'dashboard', 'participants', 'sessions', 'settings', 'status', 'update'])
    expect(slicesForTopics(['chats'])).toEqual(['chats'])
    expect(slicesForTopics(['peer']).sort()).toEqual(['dashboard', 'participants', 'status'])
    expect(slicesForTopics(['messages']).sort()).toEqual(['chats', 'dashboard', 'participants'])
  })

  it('renders the link indicator', () => {
    expect(statusLine({ configured: false }).text).toBe('link.unconfigured')
    expect(statusLine({ configured: true, connected: true, zerotier: true, online: 1, total: 2 })).toEqual({ text: 'link.on_many', cls: 'on' })
    expect(statusLine({ configured: true, connected: true, zerotier: true, warning: 'w.key' })).toEqual({ text: 'link.on_many · w.key', cls: 'off' })
    expect(statusLine({ configured: true, zerotier: false, problem: 'p.key' }).text).toBe('p.key · link.no_zerotier')
  })
})

describe('reactive push', () => {
  it('opens the stream first, with the token header, then refreshes only the event topics', async () => {
    const stream = eventStream()
    const calls: { url: string; init?: RequestInit }[] = []
    vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init })
      if (url === '/ui/api/events') return stream.response
      return new Response('{}', { status: 200 })
    }))
    const timer = vi.spyOn(globalThis, 'setTimeout')
    const app = useAppStore()
    void app.connectEvents()
    await flushPromises()
    expect(calls[0]!.url).toBe('/ui/api/events')
    expect((calls[0]!.init!.headers as Record<string, string>)['X-Agentlink-Token']).toBe('test-token')
    stream.send(change(['all']))
    await flushPromises()
    const initial = calls.slice(1).map((c) => c.url).sort()
    expect(initial).toEqual(['chats', 'dashboard', 'participants', 'sessions', 'settings', 'status', 'update'].map((n) => '/ui/api/' + n).sort())
    calls.length = 0
    stream.send(change(['chats']))
    await flushPromises()
    expect(calls.map((c) => c.url)).toEqual(['/ui/api/chats'])
    // Nothing reads data on a timer.
    expect(timer).not.toHaveBeenCalled()
  })

  it('keeps a failure on screen until every failing request recovers', async () => {
    const stream = eventStream()
    const pending = new Map<string, (r: Response) => void>()
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/ui/api/events') return Promise.resolve(stream.response)
      return new Promise<Response>((resolve) => pending.set(url, resolve))
    }))
    const app = useAppStore()
    void app.connectEvents()
    await flushPromises()
    stream.send(change(['all']))
    await flushPromises()
    pending.get('/ui/api/dashboard')!(new Response('{"error":"dashboard down"}', { status: 500 }))
    await flushPromises()
    expect(app.banner).toBe('dashboard down')
    pending.get('/ui/api/status')!(new Response('{"configured":false}', { status: 200 }))
    await flushPromises()
    expect(app.banner).toBe('dashboard down')
    expect(app.status).toEqual({ configured: false })
  })

  it('reconnects with a doubling delay', async () => {
    const calls: { url: string; init?: RequestInit }[] = []
    vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init })
      throw new Error('offline')
    }))
    vi.useFakeTimers()
    const app = useAppStore()
    const timer = vi.spyOn(globalThis, 'setTimeout')
    void app.connectEvents()
    await vi.advanceTimersByTimeAsync(0)
    expect(timer.mock.calls.map((c) => c[1])).toEqual([500])
    await vi.advanceTimersByTimeAsync(500)
    expect(timer.mock.calls.map((c) => c[1])).toEqual([500, 1000])
    expect(calls).toHaveLength(2)
    for (const call of calls) {
      expect(call.url).toBe('/ui/api/events')
      expect((call.init!.headers as Record<string, string>)['X-Agentlink-Token']).toBe('test-token')
    }
    expect(app.banner).toBe('offline')
  })

  it('reloads only when another build answers', async () => {
    runtime.version = '0.7.0'
    const cases: [string, number, number][] = [['0.7.1', 403, 1], ['0.7.1', 200, 1], ['0.7.0', 200, 0], ['0.7.0', 403, 0]]
    for (const [version, status, reloads] of cases) {
      setActivePinia(createPinia())
      const reload = vi.spyOn(browser, 'reload').mockImplementation(() => {})
      const stream = eventStream({ 'X-Agentlink-Version': version }, status)
      stream.end()
      vi.stubGlobal('fetch', vi.fn(async () => stream.response))
      const timer = vi.spyOn(globalThis, 'setTimeout').mockImplementation((() => 0) as unknown as typeof setTimeout)
      const app = useAppStore()
      await app.connectEvents()
      expect(reload, version + ' ' + status).toHaveBeenCalledTimes(reloads)
      if (version === '0.7.0' && status === 200) expect(timer).toHaveBeenCalledTimes(1)
      if (status === 403 && !reloads) expect(app.reloadRequired).toBe(true)
      vi.restoreAllMocks()
    }
  })
})
