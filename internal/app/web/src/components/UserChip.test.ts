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

function enter(sel: string) {
  $(sel)!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
}

const posts = <R extends { url: string }>(requests: R[]) => requests.filter((r) => r.url.endsWith('/ui/api/profile'))

describe('the member\'s own chip', () => {
  it('opens one menu: the name and connection, the profile in place, the theme; arrows move through it', async () => {
    await open()
    expect($('#account_menu')).toBeNull()
    await openChip()
    expect($('#user_chip')!.getAttribute('aria-expanded')).toBe('true')
    expect($('#account_head')!.textContent).toContain('alice')
    expect($('#account_status')!.textContent!.trim()).not.toBe('')
    expect($('#profile_nickname')).not.toBeNull()
    expect($('#account_back')).toBeNull()
    expect($$('#theme_mode button')).toHaveLength(3)
    $<HTMLButtonElement>('#profile_color button')!.focus()
    $('#account_menu')!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Home', bubbles: true }))
    expect(document.activeElement).toBe($('#profile_color button'))
    $('#account_menu')!.dispatchEvent(new KeyboardEvent('keydown', { key: 'End', bubbles: true }))
    expect(document.activeElement).toBe($$('#theme_font button').at(-1))
  })

  it('saves the nickname on Enter and a color on its click, staying open', async () => {
    const { app, requests } = await open()
    const chip = $('#user_chip')!
    expect(chip.textContent).toContain('alice')
    expect(chip.getAttribute('style')).toContain('--who: var(--who-')
    await openChip()
    expect($<HTMLInputElement>('#profile_nickname')!.value).toBe('alice')
    expect($$('#profile_color [data-chat-color]')).toHaveLength(8)
    for (const b of $$('#profile_color button')) expect(b.getAttribute('aria-label')).toMatch(/^profile\.color\./)
    // Unchanged: nothing to send.
    $('#profile_nickname')!.dispatchEvent(new Event('blur'))
    await settle()
    expect(posts(requests)).toHaveLength(0)
    type('#profile_nickname', 'Алиса')
    enter('#profile_nickname')
    await settle()
    expect(JSON.parse(String(posts(requests)[0]!.init.body))).toEqual({ nickname: 'Алиса', color: '' })
    expect(app.status!.nickname).toBe('Алиса')
    expect($('#user_chip')!.textContent).toContain('Алиса')
    $<HTMLButtonElement>('#profile_color [data-chat-color="teal"]')!.click()
    await settle()
    expect(JSON.parse(String(posts(requests)[1]!.init.body))).toEqual({ nickname: 'Алиса', color: 'teal' })
    expect($('#user_chip')!.getAttribute('style')).toContain('var(--who-teal)')
    expect($('#profile_color [data-chat-color="teal"]')!.getAttribute('aria-pressed')).toBe('true')
    expect($('#account_menu')).not.toBeNull()
  })

  it('refuses an empty nickname and says why a taken one is refused', async () => {
    const { requests } = await open()
    await openChip()
    type('#profile_nickname', '  ')
    enter('#profile_nickname')
    await settle()
    expect($('#profile_result')!.textContent).toContain('profile.nickname.empty')
    expect(posts(requests)).toHaveLength(0)
    type('#profile_nickname', 'BOB')
    $('#profile_nickname')!.dispatchEvent(new Event('blur'))
    await settle()
    expect($('#profile_result')!.textContent).toContain('Этот ник уже занят')
    expect($<HTMLInputElement>('#profile_nickname')!.value).toBe('BOB')
  })
})
