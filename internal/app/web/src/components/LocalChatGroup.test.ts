import { describe, expect, it } from 'vitest'
import { HIDE_GRACE_MS, NO_PROJECT, groupLocalChats } from '@/lib/localChats'
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
  it('keeps every local chat only while live or within the grace, or while kept', () => {
    const old = new Date(NOW - 10 * 60_000).toISOString()
    const list = [
      project('SITE', 'Сайт'),
      project('IDLE', 'Старый', { live: false, last_active: old }),
      project('KEPT', 'Непрочитанный', { live: false, last_active: old }),
      project('BUSY', 'Занятой', { live: true, waiting: true, last_active: old }),
      chat('live', 'SITE', { live: true }),
      chat('ended', 'SITE', { live: false, last_active: old }),
      chat('just_ended', 'SITE', { live: false, last_active: old }),
      chat('recent', 'SITE', { live: false, last_active: new Date(NOW - 5_000).toISOString() }),
      chat('topic', 'SITE', { scope: 'project', topic: 'Дизайн', live: false, last_active: old }),
      chat('older_app', 'SITE', { live: undefined, last_active: old }),
      chat('loose', '', { scope: 'folderless_temporary', live: true }),
      chat('retired', 'SITE', { live: false, retired: true, last_active: new Date(NOW - 5_000).toISOString() }),
    ]
    const lastLive = new Map([['just_ended', NOW - HIDE_GRACE_MS / 2]])
    const keep = (p: ProjectView) => p.id === 'KEPT'
    const groups = groupLocalChats(list, NOW, lastLive, keep)
    // An idle folder's project chat leaves like any other; an unread (kept) one and a busy one stay.
    expect(groups.map((g) => g.key)).toEqual(['BUSY', 'KEPT', 'SITE', NO_PROJECT])
    expect(groups[2]!.items.map((p) => p.id)).toEqual(expect.arrayContaining(['SITE', 'live', 'just_ended', 'recent', 'older_app']))
    expect(groups[2]!.items[0]!.id).toBe('SITE')
    expect(groups[2]!.items.some((p) => p.id === 'ended')).toBe(false)
    // A shared topic nobody talks in is dead weight too.
    expect(groups[2]!.items.some((p) => p.id === 'topic')).toBe(false)
    // An app without the activity flag shows a project chat as before.
    expect(groupLocalChats([project('OLD', 'Прежний', null as never)], NOW + 3_600_000, lastLive).map((g) => g.key)).toEqual(['OLD'])
    // A retired chat leaves at once, its recent reply notwithstanding.
    expect(groups[2]!.items.some((p) => p.id === 'retired')).toBe(false)
    // After the grace the chat that just ended leaves too.
    expect(groupLocalChats(list, NOW + HIDE_GRACE_MS, lastLive, keep).find((g) => g.key === 'SITE')?.items.some((p) => p.id === 'just_ended') ?? false).toBe(false)
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
})
