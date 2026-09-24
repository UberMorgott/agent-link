import { describe, expect, it } from 'vitest'
import { JOINING, SITE, fixture } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useProjectsStore } from '@/stores/projects'
import type { ProjectView } from '@/types'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)

async function open(path: string, projects?: (list: ProjectView[]) => ProjectView[]) {
  const api = fakeBackend()
  if (projects) api.backend.projects = projects(api.backend.projects)
  const mounted = await mountApp(path)
  useAppStore().status = { configured: true, node: 'alice' }
  await useProjectsStore().refreshAll()
  await settle()
  return { ...mounted, ...api }
}

async function type(sel: string, value: string) {
  const input = $<HTMLInputElement>(sel)!
  input.value = value
  input.dispatchEvent(new Event('input'))
  await settle()
}

async function submit(sel: string) {
  $<HTMLFormElement>(sel)!.dispatchEvent(new Event('submit', { cancelable: true }))
  await settle()
}

describe('making and joining projects', () => {
  it('starts on the welcome screen without projects and creates one with only a name', async () => {
    const { router, backend } = await open('/inbox', () => [])
    expect(router.currentRoute.value.name).toBe('welcome')
    $<HTMLButtonElement>('#welcome_create')!.click()
    await settle()
    expect($<HTMLButtonElement>('#create_submit')!.disabled).toBe(true)
    await type('#create_name', 'Документы')
    await submit('#create_form')
    const made = backend.projects[0]!
    expect(made.name).toBe('Документы')
    expect(made.state).toBe('needs_folder')
    expect(router.currentRoute.value.params.project).toBe(made.id)
    expect($('#create_form')).toBeNull()
  })

  it('joins: waits for the name, binds a folder on «Готово»; a known invite only opens its project', async () => {
    const { router, backend, calls } = await open('/p/' + SITE, (list) => list.filter((p) => p.id !== JOINING))
    const projects = useProjectsStore()
    $<HTMLButtonElement>('#join_project')!.click()
    await settle()
    await type('#join_invite', 'ALP1.NEWPROJECT')
    await type('#join_addr', '10.147.20.9')
    await submit('#join_form')
    expect(calls).toContain('POST projects/join')
    expect($('#join_connecting')).not.toBeNull()
    // The shared name arrives with the project's event.
    backend.projects = backend.projects.map((p) => (p.id === JOINING ? { ...p, name: 'Дизайн', display: 'Дизайн', state: 'needs_folder' } : p))
    await projects.refreshScoped(JOINING)
    await settle()
    expect($('#join_joined')!.textContent).toContain('project.join.joined')
    await type('#join_dir', 'C:\\design')
    await submit('#join_folder')
    expect(projects.byID(JOINING)!.dir).toBe('C:\\design')
    expect(router.currentRoute.value.params.project).toBe(JOINING)
    expect(projects.dialog).toBe('')

    $<HTMLButtonElement>('#join_project')!.click()
    await settle()
    await type('#join_invite', fixture<{ invite: string }>('invite').invite)
    await submit('#join_form')
    expect(router.currentRoute.value.params.project).toBe(SITE)
    expect(projects.dialog).toBe('')
    expect(calls.filter((c) => c.endsWith('/leave'))).toHaveLength(0)
  })

  it('«Отмена» after a join that created the project leaves it', async () => {
    const { calls } = await open('/p/' + SITE, (list) => list.filter((p) => p.id !== JOINING))
    const projects = useProjectsStore()
    projects.openDialog('join', '')
    await settle()
    await type('#join_invite', 'ALP1.NEWPROJECT')
    await submit('#join_form')
    expect(projects.byID(JOINING)).not.toBeNull()
    $<HTMLButtonElement>('#join_cancel')!.click()
    await settle()
    expect(calls).toContain('POST projects/' + JOINING + '/leave')
    expect(projects.byID(JOINING)).toBeNull()
    expect(projects.dialog).toBe('')
  })

  it('names a bad invite', async () => {
    await open('/p/' + SITE)
    useProjectsStore().openDialog('join', '')
    await settle()
    await type('#join_invite', 'nonsense')
    await submit('#join_form')
    expect(document.body.textContent).toContain(fixture<{ body: { error: string } }>('error_invite').body.error)
  })
})
