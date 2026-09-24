import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'
import { nextTick } from 'vue'
import { ApiError } from '@/lib/api'
import { JOINING, SITE, apiError, fixture } from '@/test/backend'
import { fakeBackend } from '@/test/harness'
import { useProjectsStore } from './projects'
import type { ProjectView } from '@/types'

beforeEach(() => setActivePinia(createPinia()))

describe('the projects store', () => {
  it('loads the projects by display name with legacy last, and each one\'s chats', async () => {
    const { calls } = fakeBackend()
    const projects = useProjectsStore()
    await projects.refreshAll()
    expect(projects.list!.map((p) => p.id)).toEqual([JOINING, SITE, 'legacy'])
    expect(calls).toContain('GET projects/' + SITE + '/chats')
    expect(calls).toContain('GET projects/legacy/chats')
    expect(calls.some((c) => c.includes('archive=1'))).toBe(false)
    expect(projects.chats[SITE]!.map((c) => c.title)).toEqual(['Посмотри вёрстку главной'])
    await projects.toggleArchive(SITE)
    expect(calls).toContain('GET projects/' + SITE + '/chats?archive=1')
    expect(projects.archives[SITE]!.every((c) => c.archived)).toBe(true)
  })

  it('creates, renames and binds a project; a busy folder change keeps the code', async () => {
    const { backend } = fakeBackend()
    const projects = useProjectsStore()
    await projects.refreshList()
    const made = await projects.create({ name: 'Док', alias: '' })
    expect(made.state).toBe('needs_folder')
    expect(projects.byID(made.id)!.display).toBe('Док')
    await projects.rename(made.id, 'Документация')
    expect(projects.byID(made.id)!.name).toBe('Документация')
    await projects.bind(made.id, { dir: 'C:\\docs', alias: 'Мои доки' })
    expect(projects.byID(made.id)!.state).toBe('ready')
    expect(projects.list!.map((p) => p.display)).toContain('Мои доки')

    backend.projects = backend.projects.map((p) => (p.id === SITE ? { ...p, busy: true } : p))
    const error = await projects.bind(SITE, { dir: 'C:\\other' }).catch((e: ApiError) => e)
    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(409)
    expect((error as ApiError).code).toBe('project_busy')
    expect((error as ApiError).message).toBe(apiError('project_busy').message)
    await expect(projects.leave(SITE)).rejects.toMatchObject({ code: 'project_busy' })
  })

  it('does not read a project it left again on the app\'s late events', async () => {
    const { calls } = fakeBackend()
    const projects = useProjectsStore()
    await projects.refreshList()
    await projects.leave(SITE)
    const before = calls.length
    await projects.refreshScoped(SITE)
    expect(calls.length).toBe(before)
    expect(projects.byID(SITE)).toBeNull()
  })

  it('reveals an invite once per dialog and forgets it on close', async () => {
    const { calls } = fakeBackend()
    const projects = useProjectsStore()
    await projects.refreshList()
    const invite = await projects.revealInvite(SITE)
    expect(invite).toBe(fixture<{ invite: string }>('invite').invite)
    await projects.revealInvite(SITE)
    expect(calls.filter((c) => c === 'POST projects/' + SITE + '/invite')).toHaveLength(1)
    projects.hideInvite()
    expect(projects.invite).toBe('')
    await projects.revealInvite(SITE)
    expect(calls.filter((c) => c === 'POST projects/' + SITE + '/invite')).toHaveLength(2)
  })

  it('joins: waits for the shared name, then a folder; cancel leaves only a project it created', async () => {
    const { backend, calls } = fakeBackend()
    backend.projects = backend.projects.filter((p) => p.id !== JOINING)
    const projects = useProjectsStore()
    await projects.refreshList()
    const result = await projects.join('ALP1.NEW', '10.147.20.9')
    expect(result.created).toBe(true)
    expect(projects.joinStep).toBe('connecting')
    // The shared name arrives with the project's event.
    const named: ProjectView = { ...fixture<ProjectView>('project_needs_folder'), id: JOINING, name: 'Дизайн', display: 'Дизайн' }
    projects.upsert(named)
    await nextTick()
    expect(projects.joinStep).toBe('folder')
    await projects.joinCancel()
    expect(calls).toContain('POST projects/' + JOINING + '/leave')
    expect(projects.byID(JOINING)).toBeNull()

    // A pasted invite of a project already here only opens it.
    const again = await projects.join(fixture<{ invite: string }>('invite').invite, '')
    expect(again.created).toBe(false)
    expect(projects.joinStep).toBe('invite')
    await projects.joinCancel()
    expect(calls.filter((c) => c.endsWith('/leave'))).toHaveLength(1)
    expect(projects.byID(SITE)).not.toBeNull()
  })

  it('remembers the last project for /inbox and opens the next one after leaving', async () => {
    fakeBackend()
    const projects = useProjectsStore()
    await projects.refreshList()
    expect(projects.landing()).toBe(JOINING)
    projects.open(SITE)
    expect(projects.landing()).toBe(SITE)
    const next = await projects.leave(SITE)
    expect(next).toBe(JOINING)
    expect(projects.current).toBe('')
  })

  it('drops an older answer of the same list', async () => {
    let release: () => void = () => {}
    let held = false
    const { backend } = fakeBackend((_method, path) => {
      if (!held || path !== 'projects/' + SITE + '/chats') return undefined
      held = false
      const stale = backend.chats[SITE]
      return new Promise((resolve) => { release = () => resolve(stale) })
    })
    const projects = useProjectsStore()
    await projects.refreshList()
    held = true
    const first = projects.refreshChats(SITE)
    backend.chats[SITE] = []
    await projects.refreshChats(SITE)
    release()
    await first
    expect(projects.chats[SITE]).toEqual([])
  })
})
