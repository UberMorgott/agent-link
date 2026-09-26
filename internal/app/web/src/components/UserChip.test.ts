import { describe, expect, it } from 'vitest'
import { SITE } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useProjectsStore } from '@/stores/projects'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))

async function open() {
  const api = fakeBackend()
  const mounted = await mountApp('/p/' + SITE)
  const app = useAppStore()
  await app.refreshSlice('status')
  await useProjectsStore().refreshAll()
  await settle()
  return { ...mounted, ...api, app }
}

async function openChip() {
  $<HTMLButtonElement>('#user_chip')!.click()
  await settle()
}

function type(sel: string, value: string) {
  const input = $<HTMLInputElement>(sel)!
  input.value = value
  input.dispatchEvent(new Event('input'))
}

describe('the member\'s own chip', () => {
  it('shows the nickname in the member\'s color and saves a new nickname and color at once', async () => {
    const { app, requests } = await open()
    const chip = $('#user_chip')!
    expect(chip.textContent).toContain('alice')
    expect(chip.getAttribute('style')).toContain('--who: var(--who-')
    await openChip()
    expect($<HTMLInputElement>('#profile_nickname')!.value).toBe('alice')
    expect($$('#profile_color [data-chat-color]')).toHaveLength(8)
    for (const b of $$('#profile_color button')) expect(b.getAttribute('aria-label')).toMatch(/^profile\.color\./)
    type('#profile_nickname', 'Алиса')
    $<HTMLButtonElement>('#profile_color [data-chat-color="teal"]')!.click()
    await settle()
    $<HTMLFormElement>('#profile_form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    const post = requests.find((r) => r.url.endsWith('/ui/api/profile'))!
    expect(JSON.parse(String(post.init.body))).toEqual({ nickname: 'Алиса', color: 'teal' })
    expect(app.status!.nickname).toBe('Алиса')
    expect($('#user_chip')!.textContent).toContain('Алиса')
    expect($('#user_chip')!.getAttribute('style')).toContain('var(--who-teal)')
    expect($('#profile_form')).toBeNull()
  })

  it('refuses an empty nickname and says why a taken one is refused', async () => {
    const { requests } = await open()
    await openChip()
    type('#profile_nickname', '  ')
    $<HTMLFormElement>('#profile_form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    expect($('#profile_result')!.textContent).toContain('profile.nickname.empty')
    expect(requests.some((r) => r.url.endsWith('/ui/api/profile'))).toBe(false)
    type('#profile_nickname', 'BOB')
    $<HTMLFormElement>('#profile_form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    expect($('#profile_result')!.textContent).toContain('Этот ник уже занят')
    expect($('#profile_form')).not.toBeNull()
  })
})