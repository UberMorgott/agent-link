import { describe, expect, it } from 'vitest'
import { SITE } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useProjectsStore } from '@/stores/projects'

describe('agents navigation', () => {
  it('opens the existing project chat for a project with local agents', async () => {
    const api = fakeBackend()
    api.backend.seats[SITE] = [
      { id: 'codex-1', provider: 'codex', label: 'Codex', status: 'idle' },
      { id: 'claude-1', provider: 'claude', label: 'Claude Code', status: 'running' },
    ]
    const { router } = await mountApp('/agents')
    await useProjectsStore().refreshAll()
    await settle()

    const link = document.querySelector<HTMLAnchorElement>('a[data-route="agents"]')!
    expect(link.getAttribute('href')).toBe('/ui/agents')
    expect(link.className).toContain('active')
    expect(document.querySelectorAll('#agents_projects > li')).toHaveLength(1)
    expect(document.querySelector('#agents_projects')!.textContent).toContain('Codex')
    expect(document.querySelector('#agents_projects')!.textContent).toContain('Claude Code')

    document.querySelector<HTMLButtonElement>('#agents_projects [data-project="' + SITE + '"] button')!.click()
    await settle()
    expect(router.currentRoute.value.name).toBe('chat')
    expect(router.currentRoute.value.params.project).toBe(SITE)
    expect(router.currentRoute.value.params.chat).toBe(api.backend.chats[SITE]![0]!.id)
    expect(api.calls.some((call) => call.startsWith('POST projects/' + SITE + '/chats'))).toBe(false)
  })
})
