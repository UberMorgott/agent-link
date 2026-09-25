import { describe, expect, it } from 'vitest'
import { JOINING, SITE } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'
import type { ChatMessage } from '@/types'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))
const CHAT = '7b8b965ad4bca0e41ab51de7b31363a1'

async function open(path: string, override?: Parameters<typeof fakeBackend>[0]) {
  const api = fakeBackend(override)
  const mounted = await mountApp(path)
  useAppStore().status = { configured: true, node: 'alice' }
  await useProjectsStore().refreshAll()
  await settle()
  return { ...mounted, ...api }
}

describe('a project page', () => {
  it('shows an empty project as one line with its actions, and none of the old texts', async () => {
    await open('/p/' + JOINING)
    const empty = $('#project_empty')!
    expect(empty.querySelector('h1')!.textContent).toContain('projects.connecting')
    expect($('#project_state')!.textContent).toContain('project.state.connecting')
    const projects = useProjectsStore()
    projects.upsert({ ...projects.byID(JOINING)!, name: 'Дизайн', display: 'Дизайн', state: 'needs_folder' })
    await settle()
    expect(empty.querySelector('h1')!.textContent).toContain('project.empty')
    expect($('#project_state')!.textContent).toContain('project.state.needs_folder')
    expect($('#project_new_chat')).not.toBeNull()
    expect($('#project_chats')).toBeNull()
    const page = document.body.textContent || ''
    for (const gone of ['inbox.h1', 'inbox.list.label', 'inbox.select', 'inbox.select_hint', 'inbox.list.empty', 'nav.inbox']) {
      expect(page).not.toContain(gone)
    }
  })

  it('lists the chat of a project; «new chat» from the sidebar opens it, a project has one', async () => {
    const { router, calls } = await open('/p/' + SITE)
    expect($('#project_title')!.textContent).toContain('Мой сайт')
    expect($$('#project_chats .chat-row')).toHaveLength(1)
    $('[data-project="' + SITE + '"] .project-new-chat')!.click()
    await settle()
    expect($('#new_chat_form')).not.toBeNull()
    $<HTMLButtonElement>('#new_chat_create')!.click()
    await settle()
    expect(calls).toContain('POST projects/' + SITE + '/chats')
    expect(router.currentRoute.value.name).toBe('chat')
    expect(router.currentRoute.value.params.project).toBe(SITE)
    expect(router.currentRoute.value.params.chat).toBe(CHAT)
    expect(useProjectsStore().chats[SITE]).toHaveLength(1)
  })

  it('counts unread chats per project from the read cursors, not from the agent', async () => {
    const { backend } = await open('/p/' + SITE)
    const projects = useProjectsStore()
    // The history seen on the first visit counts as read.
    expect($$('.project-unread')).toHaveLength(0)
    const chat = backend.chats[SITE]![0]!
    const next: ChatMessage = { ...chat.last_message!, id: 'fresh', seq: 3, body: 'ещё' }
    backend.chats[SITE]![0] = { ...chat, last_seq: 3, last_message: next }
    await projects.refreshChats(SITE)
    await settle()
    expect($('[data-project="' + SITE + '"] .project-unread')!.textContent).toBe('1')
    expect($('[data-chat="' + CHAT + '"]')!.className).toContain('fresh')
    // Opening the chat moves the cursor; the badge goes.
    $<HTMLButtonElement>('[data-chat="' + CHAT + '"]')!.click()
    await settle()
    expect($('[data-project="' + SITE + '"] .project-unread')).toBeNull()
    expect(JSON.parse(localStorage.getItem('agentlink.reads.v2:alice')!)[SITE + ':' + CHAT]).toBe(3)
  })

  it('drops the answer of a chat when the project changed meanwhile', async () => {
    let release: () => void = () => {}
    let hold = false
    const { router } = await open('/p/' + SITE, (_method, path) => {
      if (!hold || !path.startsWith('projects/' + SITE + '/chats/' + CHAT)) return undefined
      return new Promise((resolve) => { release = () => resolve(undefined) })
    })
    const inbox = useInboxStore()
    hold = true
    void router.push('/p/' + SITE + '/c/' + CHAT)
    await settle()
    hold = false
    await router.push('/p/legacy/c/legacy-chat-1')
    await settle()
    release()
    await settle()
    expect(inbox.project).toBe('legacy')
    expect(inbox.chat!.id).toBe('legacy-chat-1')
    expect(inbox.messages.map((m) => m.body)).toEqual(['Привет из прежней сети'])
  })
})
