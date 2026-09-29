import { describe, expect, it } from 'vitest'
import { SITE } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useProjectsStore } from '@/stores/projects'

describe('agents navigation', () => {
  it('shows a newly created local chat separately from network chats and opens its existing conversation', async () => {
    const api = fakeBackend()
    const { router } = await mountApp('/agents')
    const projects = useProjectsStore()
    await projects.refreshAll()
    await settle()
    expect(document.querySelectorAll('#agents_projects [data-project]')).toHaveLength(0)
    expect(document.querySelector('#project_tree [data-project="' + SITE + '"]')).not.toBeNull()

    // A native discuss call creates this project outside the UI. The next
    // projects refresh must place it in the local section without navigation.
    const local = 'LOCAL_CHAT'
    const localChat = 'local-chat-1'
    const network = api.backend.projects.find((p) => p.id === SITE)!
    const existing = api.backend.chats[SITE]![0]!
    api.backend.projects.push({ ...network, id: local, scope: 'local', name: 'Agent-Link · Claude ↔ Codex', display: 'Agent-Link · Claude ↔ Codex', members: network.members.filter((m) => m.self) })
    api.backend.chats[local] = [{ ...existing, id: localChat, project: local, participants: ['alice'], members: existing.members?.filter((m) => m.self) }]
    api.backend.messages[localChat] = []
    api.backend.seats[local] = [
      { id: 'codex-1', provider: 'codex', label: 'Codex', status: 'idle' },
      { id: 'claude-1', provider: 'claude', label: 'Claude Code', status: 'running' },
    ]
    await projects.refreshList()
    await settle()

    const link = document.querySelector<HTMLAnchorElement>('a[data-route="agents"]')!
    expect(link.getAttribute('href')).toBe('/ui/agents')
    expect(link.className).toContain('active')
    expect(document.querySelectorAll('#agents_projects [data-project]')).toHaveLength(1)
    expect(document.querySelector('#agents_projects')!.textContent).toContain('Codex')
    expect(document.querySelector('#agents_projects')!.textContent).toContain('Claude Code')
    expect(document.querySelector('#project_tree [data-project="' + local + '"]')).toBeNull()
    expect(document.querySelector('#local_chat_tree [data-project="' + local + '"]')).not.toBeNull()
    expect(document.querySelector('#local_chat_tree [data-project="' + SITE + '"]')).toBeNull()

    document.querySelector<HTMLButtonElement>('#agents_projects [data-project="' + local + '"] button')!.click()
    await settle()
    expect(router.currentRoute.value.name).toBe('chat')
    expect(router.currentRoute.value.params.project).toBe(local)
    expect(router.currentRoute.value.params.chat).toBe(localChat)
    expect(api.calls.some((call) => call.startsWith('POST projects/' + local + '/chats'))).toBe(false)
  })
})
