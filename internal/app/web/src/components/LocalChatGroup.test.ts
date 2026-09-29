import { describe, expect, it } from 'vitest'
import { HIDE_GRACE_MS, NO_PROJECT, groupLocalChats } from '@/lib/localChats'
import { fixture } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useProjectsStore } from '@/stores/projects'
import type { LocalChatView, ProjectView } from '@/types'

const NOW = Date.parse('2026-09-01T12:00:00Z')

function project(id: string, display: string): ProjectView {
  return { ...fixture<ProjectView>('project_local'), id, name: display, display }
}

function chat(id: string, of: string, lc: Partial<LocalChatView>): ProjectView {
  const base = fixture<ProjectView>('project_local_chat')
  return { ...base, id, name: id, display: id, local_chat: { ...base.local_chat!, project: of, ...lc } }
}

describe('local chats grouping', () => {
  it('keeps project chats and topics, and temporary chats only while live or within the grace', () => {
    const old = new Date(NOW - 10 * 60_000).toISOString()
    const list = [
      project('SITE', 'Сайт'),
      chat('live', 'SITE', { live: true }),
      chat('ended', 'SITE', { live: false, last_active: old }),
      chat('just_ended', 'SITE', { live: false, last_active: old }),
      chat('recent', 'SITE', { live: false, last_active: new Date(NOW - 5_000).toISOString() }),
      chat('topic', 'SITE', { scope: 'project', topic: 'Дизайн', live: false, last_active: old }),
      chat('older_app', 'SITE', { live: undefined, last_active: old }),
      chat('loose', '', { scope: 'folderless_temporary', live: true }),
    ]
    const lastLive = new Map([['just_ended', NOW - HIDE_GRACE_MS / 2]])
    const groups = groupLocalChats(list, NOW, lastLive)
    expect(groups.map((g) => g.key)).toEqual(['SITE', NO_PROJECT])
    expect(groups[0]!.items.map((p) => p.id)).toEqual(expect.arrayContaining(['SITE', 'live', 'just_ended', 'recent', 'topic', 'older_app']))
    expect(groups[0]!.items[0]!.id).toBe('SITE')
    expect(groups[0]!.items.some((p) => p.id === 'ended')).toBe(false)
    // After the grace the chat that just ended leaves too.
    expect(groupLocalChats(list, NOW + HIDE_GRACE_MS, lastLive)[0]!.items.some((p) => p.id === 'just_ended')).toBe(false)
  })
})

describe('local chats sidebar', () => {
  it('shows one project with several live chats as a group that opens, a single chat as a plain row, and hides ended chats', async () => {
    const api = fakeBackend()
    await mountApp('/agents')
    const projects = useProjectsStore()
    const old = new Date(Date.now() - 10 * 60_000).toISOString()
    const added = [
      project('SITE_LOCAL', 'Сайт'),
      chat('c1', 'SITE_LOCAL', { live: true, waiting: true, owner: { session: 's1', provider: 'claude' } }),
      chat('c2', 'SITE_LOCAL', { live: true, owner: { session: 's2', agent: 'a1', agent_type: 'reviewer' } }),
      project('SHOP_LOCAL', 'Магазин'),
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

    // The chat comes back once someone is in it again.
    api.backend.projects = api.backend.projects.map((p) => (p.id === 'c3' ? { ...p, local_chat: { ...p.local_chat!, live: true } } : p))
    await projects.refreshList()
    await settle()
    expect(tree.querySelector('[data-group="SHOP_LOCAL"] [data-project="c3"]')).not.toBeNull()
  })
})
