import { describe, expect, it, vi } from 'vitest'
import { browser, runtime } from '@/lib/runtime'
import { fakeApi, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import type { ParticipantView } from '@/types'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))

async function openParticipants(people: ParticipantView[]) {
  let release: (() => void) | null = null
  const api = fakeApi((method, path) => {
    if (method === 'POST' && path.startsWith('members/')) return new Promise((resolve) => { release = () => resolve({ node: 'local' }) })
    if (path === 'participants') return people
    if (path === 'dashboard') return {}
    throw new Error('unexpected call ' + method + ' ' + path)
  })
  const mounted = await mountApp('/participants')
  useAppStore().participants = people
  await settle()
  return { ...mounted, api, release: () => release!() }
}

describe('participants', () => {
  it('rows are keyboard buttons that open the chat with that person', async () => {
    runtime.strings = { 'participants.open_chat': 'Открыть чат с {name}' }
    const { router } = await openParticipants([{ name: 'bob', online: true, total: 2, sent: 1, received: 1 }])
    const open = $<HTMLButtonElement>('#participants .participant-main')!
    expect(open.tagName).toBe('BUTTON')
    expect(open.type).toBe('button')
    expect(open.tabIndex).toBeGreaterThanOrEqual(0)
    expect(open.getAttribute('aria-label')).toContain('bob')
    open.click()
    await settle()
    expect(router.currentRoute.value.name).toBe('project')
    expect(router.currentRoute.value.params.project).toBe('legacy')
    expect(router.currentRoute.value.query.peer).toBe('bob')
  })

  it('guard adding and removing while the request runs', async () => {
    runtime.strings = { 'participants.remove_named': 'Удалить {name}' }
    const people = [{ name: 'bob', online: true }, { name: 'carl', online: false, seen: '2026-01-02T03:04:05Z', addrs: ['10.0.0.2'] }]
    const { api, release } = await openParticipants(people)
    const addr = $<HTMLInputElement>('#participant_addr')!
    const add = $<HTMLButtonElement>('#add_participant')!
    expect(addr.placeholder).toBe('participants.add.placeholder')
    expect($$('.participant-remove')[0]!.getAttribute('aria-label')).toBe('Удалить bob')
    expect($$('.participant-detail')[0]!.textContent).toContain('participants.addresses')

    $<HTMLFormElement>('#participant_add')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    expect($('#participants_result')!.textContent).toBe('participants.add.empty')
    expect(api.calls).toHaveLength(0)

    addr.value = ' 10.0.0.9 '
    addr.dispatchEvent(new Event('input'))
    $<HTMLFormElement>('#participant_add')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    expect(add.disabled).toBe(true)
    expect(addr.disabled).toBe(true)
    expect($('#participant_add')!.getAttribute('aria-busy')).toBe('true')
    release()
    await settle()
    expect(add.disabled).toBe(false)
    expect(addr.value).toBe('')
    expect($('#participant_add')!.hasAttribute('aria-busy')).toBe(false)
    expect(api.calls).toEqual(['POST members/add', 'GET participants', 'GET dashboard'])

    const confirm = vi.spyOn(browser, 'confirm').mockReturnValue(false)
    $$<HTMLButtonElement>('.participant-remove')[0]!.click()
    await settle()
    expect(confirm).toHaveBeenCalledWith('participants.confirm')
    expect(api.calls).toHaveLength(3)
    confirm.mockReturnValue(true)
    const remove = $$<HTMLButtonElement>('.participant-remove')[0]!
    remove.click()
    await settle()
    expect(remove.disabled).toBe(true)
    expect($$('.participant-card')[0]!.getAttribute('aria-busy')).toBe('true')
    release()
    await settle()
    expect($('#participants_result')!.textContent).toBe('participants.removed')
    // The focus moves to the row now in the removed one's place.
    expect(document.activeElement).toBe($$('.participant-main')[0])
  })
})
