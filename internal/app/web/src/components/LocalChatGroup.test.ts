import { describe, expect, it, vi } from 'vitest'
import { HIDE_GRACE_MS, NO_PROJECT, groupLocalChats, liveState } from '@/lib/localChats'
import { fixture } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useProjectsStore } from '@/stores/projects'
import type { LocalChatView, ProjectView } from '@/types'

const NOW = Date.parse('2026-09-01T12:00:00Z')

function project(id: string, display: string, activity?: ProjectView['activity']): ProjectView {
  const base = fixture<ProjectView>('project_local')
  return { ...base, id, name: display, display, activity: activity === undefined ? base.activity : activity }
}

function chat(id: string, of: string, lc: Partial<LocalChatView>): ProjectView {
  const base = fixture<ProjectView>('project_local_chat')
  return { ...base, id, name: id, display: id, local_chat: { ...base.local_chat!, project: of, ...lc } }
}

describe('local chats grouping', () => {
  it('keeps every local chat only while live or within the grace after it, or while kept', () => {
    const ago = (ms: number) => new Date(NOW - ms).toISOString()
    const list = [
      project('SITE', 'Сайт', { live: true }),
      project('IDLE', 'Старый', { live: false, live_ended_at: ago(10 * 60_000) }),
      project('KEPT', 'Непрочитанный', { live: false, live_ended_at: ago(10 * 60_000) }),
      project('BUSY', 'Занятой', { live: true, waiting: true }),
      chat('live', 'SITE', { live: true }),
      // Its owner session is open, but nothing runs: not live, and never was.
      chat('owner_open', 'SITE', { live: false, last_active: ago(2 * 60_000) }),
      chat('ended', 'SITE', { live: false, live_ended_at: ago(HIDE_GRACE_MS + 1_000) }),
      chat('just_ended', 'SITE', { live: false, live_ended_at: ago(30_000) }),
      chat('topic', 'SITE', { scope: 'project', topic: 'Дизайн', live: false, live_ended_at: ago(10 * 60_000) }),
      chat('loose', '', { scope: 'folderless_temporary', live: true }),
      chat('retired', 'SITE', { live: false, retired: true, live_ended_at: ago(5_000) }),
    ]
    const keep = (p: ProjectView) => p.id === 'KEPT'
    const groups = groupLocalChats(list, NOW, keep)
    // An idle folder's project chat leaves like any other; an unread (kept) one and a busy one stay.
    expect(groups.map((g) => g.key)).toEqual(['BUSY', 'KEPT', 'SITE', NO_PROJECT])
    expect(groups[2]!.items.map((p) => p.id).sort()).toEqual(['SITE', 'just_ended', 'live'])
    // After the grace the chat that just ended leaves too.
    expect(groupLocalChats(list, NOW + 30_000, keep).find((g) => g.key === 'SITE')!.items.map((p) => p.id)).toEqual(['SITE', 'live'])
  })

  it('names what is at work: a waiting caller, a running turn, or nothing', () => {
    expect(liveState(chat('w', '', { live: true, waiting: true }))).toBe('waiting')
    expect(liveState(chat('r', '', { live: true, waiting: false }))).toBe('live')
    expect(liveState(chat('i', '', { live: false }))).toBe('')
  })
})
describe('local chats sidebar', () => {
  it('shows one project with several live chats as a group that opens, a single chat as a plain row, and hides ended chats', async () => {
    const api = fakeBackend()
    await mountApp('/agents')
    const projects = useProjectsStore()
    const old = new Date(Date.now() - 10 * 60_000).toISOString()
    const added = [
      project('SITE_LOCAL', 'Сайт', { live: true }),
      chat('c1', 'SITE_LOCAL', { live: true, waiting: true, owner: { session: 's1', provider: 'claude' } }),
      chat('c2', 'SITE_LOCAL', { live: true, owner: { session: 's2', agent: 'a1', agent_type: 'reviewer' } }),
      project('SHOP_LOCAL', 'Магазин', { live: true }),
      project('IDLE_LOCAL', 'Заброшенный', { live: false, last_active: old }),
      chat('c3', 'SHOP_LOCAL', { live: false, last_active: old }),
    ]
    for (const p of added) {
      api.backend.projects.push(p)
      api.backend.chats[p.id] = []
    }
    await projects.refreshList()
    await settle()

    const tree = document.querySelector('#local_chat_tree')!
    const head = tree.querySelector<HTMLButtonElement>('[data-group="SITE_LOCAL"] > .project-row > button')!
    const items = tree.querySelector<HTMLElement>('#local_group_SITE_LOCAL')!
    expect(head.getAttribute('aria-expanded')).toBe('false')
    expect(head.getAttribute('aria-controls')).toBe('local_group_SITE_LOCAL')
    expect(head.textContent).toContain('Сайт')
    expect(head.textContent).toContain('3')
    expect(items.style.display).toBe('none')
    expect(items.querySelectorAll('[data-project]')).toHaveLength(3)
    expect(items.querySelector('[data-project="c1"] [data-live="waiting"]')).not.toBeNull()
    expect(items.querySelector('[data-project="c2"]')!.textContent).toContain('reviewer')

    head.click()
    await settle()
    expect(head.getAttribute('aria-expanded')).toBe('true')
    expect(items.style.display).toBe('')
    expect(JSON.parse(localStorage.getItem('agentlink.localchats.expanded') || '{}')).toEqual({ SITE_LOCAL: true })

    head.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true }))
    await settle()
    expect(head.getAttribute('aria-expanded')).toBe('false')
    head.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }))
    await settle()
    expect(head.getAttribute('aria-expanded')).toBe('true')

    // Магазин has one chat left: its own, as a plain row; the ended temporary chat is gone.
    expect(tree.querySelector('[data-group="SHOP_LOCAL"]')).toBeNull()
    expect(tree.querySelector(':scope > [data-project="SHOP_LOCAL"]')).not.toBeNull()
    expect(tree.querySelector('[data-project="c3"]')).toBeNull()
    // An idle folder's project chat is not listed; a person reads along, so no row has a menu.
    expect(tree.querySelector('[data-project="IDLE_LOCAL"]')).toBeNull()
    expect(tree.querySelector('.project-more')).toBeNull()

    // The chat comes back once someone is in it again.
    api.backend.projects = api.backend.projects.map((p) => (p.id === 'c3' ? { ...p, local_chat: { ...p.local_chat!, live: true } } : p))
    await projects.refreshList()
    await settle()
    expect(tree.querySelector('[data-group="SHOP_LOCAL"] [data-project="c3"]')).not.toBeNull()
  })

  it('shows a green dot only while live, and drops an ended chat when its grace runs out', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
    const api = fakeBackend()
    await mountApp('/agents')
    const projects = useProjectsStore()
    const ended = new Date(Date.now() - 30_000).toISOString()
    for (const p of [chat('run', '', { scope: 'folderless_temporary', live: true }), chat('done', '', { scope: 'folderless_temporary', live: false, live_ended_at: ended })]) {
      api.backend.projects.push(p)
      api.backend.chats[p.id] = []
    }
    await projects.refreshList()
    await settle()
    const tree = document.querySelector('#local_chat_tree')!
    expect(tree.querySelector('[data-project="run"] .chat-live')).not.toBeNull()
    expect(tree.querySelector('[data-project="done"] .chat-live')).toBeNull()
    // A chat not at work says so, not who of the network is online.
    const idle = tree.querySelector('[data-project="done"] .project-dot')!
    expect(idle.getAttribute('title')).toBe('local_chat.idle')
    expect(idle.getAttribute('aria-label')).toBe('local_chat.idle')
    expect(tree.textContent).not.toContain('projects.online')
    vi.advanceTimersByTime(29_000)
    await settle()
    expect(tree.querySelector('[data-project="done"]')).not.toBeNull()
    vi.advanceTimersByTime(1_000)
    await settle()
    expect(tree.querySelector('[data-project="done"]')).toBeNull()
    expect(tree.querySelector('[data-project="run"]')).not.toBeNull()
  })
})