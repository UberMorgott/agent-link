import { describe, expect, it, vi } from 'vitest'
import { api } from './api'

describe('api', () => {
  it('never hands a later read an answer that started before a change', async () => {
    const releases: (() => void)[] = []
    const fetchMock = vi.fn((url: string) => new Promise<Response>((resolve) => {
      releases.push(() => resolve(new Response(JSON.stringify({ url }), { status: 200 })))
    }))
    vi.stubGlobal('fetch', fetchMock)
    const before = api('GET', 'participants')
    const change = api('POST', 'members/add', { addr: 'x' })
    const after = api('GET', 'participants')
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual(['/ui/api/participants', '/ui/api/members/add', '/ui/api/participants'])
    for (const release of releases) release()
    await Promise.all([before, change, after])
  })
})
