import { describe, expect, it, vi } from 'vitest'
import { LEGACY_CODE, SITE, fixture } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useProjectsStore } from '@/stores/projects'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))
const INVITE = fixture<{ invite: string }>('invite').invite

async function open(path: string) {
  const api = fakeBackend()
  const mounted = await mountApp(path)
  useAppStore().status = { configured: true, node: 'alice' }
  await useProjectsStore().refreshAll()
  await settle()
  return { ...mounted, ...api }
}

// menu opens a project's "⋯" menu and returns its item labels.
async function menu(pid: string): Promise<HTMLElement[]> {
  const trigger = $<HTMLButtonElement>('[data-project="' + pid + '"] .project-more')!
  trigger.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, key: 'Enter' }))
  await settle()
  return $$('[role="menuitem"]')
}

describe('the project menu', () => {
  it('offers members, invite, name, folder and leave; the legacy network has no name', async () => {
    await open('/p/' + SITE)
    const items = await menu(SITE)
    expect(items.map((i) => i.textContent!.trim())).toEqual([
      'project.menu.members', 'project.menu.invite', 'project.menu.name', 'project.menu.folder', 'project.menu.leave',
    ])
    items[1]!.click()
    await settle()
    expect(useProjectsStore().dialog).toBe('invite')
    useProjectsStore().closeDialog()
    await settle()
    const legacy = await menu('legacy')
    expect(legacy.map((i) => i.textContent!.trim())).not.toContain('project.menu.name')
  })

  it('masks the invite, reveals it once on the eye, copies it and forgets it on close', async () => {
    const { calls } = await open('/p/' + SITE)
    const projects = useProjectsStore()
    // The empty-state "Пригласить" and the menu open the same dialog.
    projects.openDialog('invite', SITE)
    await settle()
    const value = () => $<HTMLInputElement>('#invite_value')!.value
    expect(value()).toMatch(/^•+$/)
    expect(calls.filter((c) => c.endsWith('/invite'))).toHaveLength(0)
    $<HTMLButtonElement>('#invite_eye')!.click()
    await settle()
    expect(value()).toBe(INVITE)
    $<HTMLButtonElement>('#invite_eye')!.click()
    await settle()
    expect(value()).toMatch(/^•+$/)
    const writeText = vi.fn(async () => {})
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
    $<HTMLButtonElement>('#invite_copy')!.click()
    await settle()
    expect(writeText).toHaveBeenCalledWith(INVITE)
    expect(calls.filter((c) => c.endsWith('/invite'))).toHaveLength(1)
    projects.closeDialog()
    await settle()
    expect(projects.invite).toBe('')
    expect(document.body.textContent).not.toContain(INVITE)
    projects.openDialog('invite', 'legacy')
    await settle()
    $<HTMLButtonElement>('#invite_eye')!.click()
    await settle()
    expect(value()).toBe(LEGACY_CODE)
  })

  it('renames, binds a folder, says why a busy project cannot change, and leaves', async () => {
    const { backend, router, calls } = await open('/p/' + SITE)
    const projects = useProjectsStore()
    projects.openDialog('name', SITE)
    await settle()
    const input = $<HTMLInputElement>('#project_name')!
    expect(input.value).toBe('Сайт')
    input.value = 'Сайт 2'
    input.dispatchEvent(new Event('input'))
    $<HTMLFormElement>('#name_form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    expect(projects.byID(SITE)!.name).toBe('Сайт 2')
    expect(projects.dialog).toBe('')

    backend.projects = backend.projects.map((p) => (p.id === SITE ? { ...p, busy: true } : p))
    projects.openDialog('folder', SITE)
    await settle()
    $<HTMLButtonElement>('#project_dir_pick')!.click()
    await settle()
    expect($<HTMLInputElement>('#project_dir')!.value).toBe('C:\\work\\picked')
    $<HTMLFormElement>('#folder_form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    expect(projects.dialog).toBe('folder')
    expect(document.body.textContent).toContain(fixture<{ body: { error: string } }>('error_project_busy').body.error)

    backend.projects = backend.projects.map((p) => (p.id === SITE ? { ...p, busy: false } : p))
    projects.openDialog('leave', SITE)
    await settle()
    expect(document.body.textContent).toContain('project.leave.text')
    $<HTMLButtonElement>('#leave_confirm')!.click()
    await settle()
    expect(calls).toContain('POST projects/' + SITE + '/leave')
    expect(projects.byID(SITE)).toBeNull()
    expect(router.currentRoute.value.params.project).not.toBe(SITE)
    expect(router.currentRoute.value.name).toBe('project')
  })
})
