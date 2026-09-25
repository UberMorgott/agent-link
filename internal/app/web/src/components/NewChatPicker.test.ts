import { describe, expect, it, vi } from 'vitest'
import { browser } from '@/lib/runtime'
import { SITE } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))
const ticked = () => $$('#new_chat_members [role="checkbox"]').filter((box) => box.getAttribute('aria-checked') === 'true').map((box) => box.id)
const CHAT = '7b8b965ad4bca0e41ab51de7b31363a1'

async function open(path: string, noActive = false) {
  const api = fakeBackend()
  // A project has one active chat; without one «new chat» starts it.
  if (noActive) for (const c of api.backend.chats[SITE] || []) Object.assign(c, { closed: true, archived: true })
  const mounted = await mountApp(path)
  useAppStore().status = { configured: true, node: 'alice' }
  await useProjectsStore().refreshAll()
  await settle()
  return { ...mounted, ...api }
}

describe('the new chat picker', () => {
  it('starts a project chat with nobody else; its owner invites and removes members later', async () => {
    const { backend, calls } = await open('/p/' + SITE, true)
    $<HTMLButtonElement>('#project_new_chat')!.click()
    await settle()
    for (const box of $$('#new_chat_members [role="checkbox"]')) box.click()
    await settle()
    expect(ticked()).toEqual([])
    const create = $<HTMLButtonElement>('#new_chat_create')!
    expect(create.disabled).toBe(false)
    create.click()
    await settle()
    const made = backend.chats[SITE]![0]!
    expect(made.participants).toEqual(['alice'])
    expect(made.owner).toBe('alice')

    const inbox = useInboxStore()
    inbox.infoOpen = true
    await settle()
    expect($$('#chat_invite button').map((b) => b.id)).toEqual(['chat_invite_bob', 'chat_invite_carol'])
    // carol is away: she is invited all the same.
    $<HTMLButtonElement>('#chat_invite_carol')!.click()
    await settle()
    expect(calls).toContain('POST projects/' + SITE + '/chats/' + made.id + '/members')
    expect(made.participants).toEqual(['alice', 'carol'])
    expect($$('#chat_invite button').map((b) => b.id)).toEqual(['chat_invite_bob'])

    const confirm = vi.spyOn(browser, 'confirm').mockReturnValue(true)
    $<HTMLButtonElement>('#chat_remove_carol')!.click()
    await settle()
    expect(confirm).toHaveBeenCalledWith('inbox.members.remove_confirm')
    expect(made.participants).toEqual(['alice'])
    expect($('#chat_remove_alice')).toBeNull()
  })

  it('lists every member with their state, all chosen; the list is sent as is', async () => {
    const { backend } = await open('/p/' + SITE, true)
    $<HTMLButtonElement>('#project_new_chat')!.click()
    await settle()
    const people = $$('#new_chat_members [role="checkbox"]')
    expect(people.map((p) => p.id)).toEqual(['new_chat_bob', 'new_chat_carol'])
    expect(ticked()).toEqual(['new_chat_bob', 'new_chat_carol'])
    const text = $('#new_chat_members')!.textContent!
    expect(text).toContain('inbox.member.online')
    expect(text).toContain('inbox.member.away')
    expect($('#new_chat_count')!.textContent).toContain('inbox.new.count')
    for (const box of people) box.click()
    await settle()
    expect(ticked()).toEqual([])
    $<HTMLButtonElement>('#new_chat_carol')!.click()
    await settle()
    $<HTMLButtonElement>('#new_chat_create')!.click()
    await settle()
    const made = backend.chats[SITE]![0]!
    expect(made.participants).toEqual(['alice', 'carol'])
    expect(made.mode).toBe('project')
    expect(useInboxStore().newChatOpen).toBe(false)
  })

  it('asking for a new chat in a project with one opens that chat', async () => {
    const { backend, router } = await open('/p/' + SITE)
    const before = backend.chats[SITE]!.length
    $<HTMLButtonElement>('#project_new_chat')!.click()
    await settle()
    $<HTMLButtonElement>('#new_chat_create')!.click()
    await settle()
    expect(backend.chats[SITE]!.length).toBe(before)
    expect(router.currentRoute.value.params.chat).toBe(CHAT)
  })

  it('archives a project chat\'s history and opens a fresh chat with the same people at once', async () => {
    const { backend, router, calls } = await open('/p/' + SITE + '/c/' + CHAT)
    expect($('#chat_close')).toBeNull()
    const archive = $<HTMLButtonElement>('#chat_archive')!
    expect(archive.getAttribute('aria-label')).toBe('inbox.archive_history')
    const confirm = vi.spyOn(browser, 'confirm').mockReturnValue(true)
    archive.click()
    await settle()
    expect(confirm).toHaveBeenCalledWith('inbox.archive_history.confirm')
    expect(calls).toContain('POST projects/' + SITE + '/chats/' + CHAT + '/archive')
    const fresh = backend.chats[SITE]![0]!
    expect(fresh.id).not.toBe(CHAT)
    expect(fresh.prev).toBe(CHAT)
    expect(router.currentRoute.value.params.chat).toBe(fresh.id)
    // The archived history, reopened, leads back to the project's chat.
    await router.push('/p/' + SITE + '/c/' + CHAT)
    await settle()
    expect($('#chat_note_text')!.textContent).toContain('inbox.archived_note')
    $<HTMLButtonElement>('#chat_note_current')!.click()
    await settle()
    expect(router.currentRoute.value.params.chat).toBe(fresh.id)
  })
})
