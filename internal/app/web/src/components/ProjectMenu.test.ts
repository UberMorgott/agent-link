import { describe, expect, it, vi } from 'vitest'
import { browser } from '@/lib/runtime'
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
  it('offers archiving the history, members, invite, name, folder and delete; the legacy network has no name', async () => {
    await open('/p/' + SITE)
    const items = await menu(SITE)
    expect(items.map((i) => i.textContent!.trim())).toEqual([
      'inbox.archive_history', 'project.menu.members', 'project.menu.invite', 'project.menu.agents', 'project.menu.name', 'project.menu.folder', 'project.menu.autonomy', 'project.menu.delete',
    ])
    items[2]!.click()
    await settle()
    expect(useProjectsStore().dialog).toBe('invite')
    useProjectsStore().closeDialog()
    await settle()
    const legacy = (await menu('legacy')).map((i) => i.textContent!.trim())
    expect(legacy).not.toContain('project.menu.name')
    expect(legacy).not.toContain('project.menu.agents')
    expect(legacy).not.toContain('inbox.archive_history')
    expect(legacy).toContain('project.menu.leave')
  })

  it('opens the agents\' autonomy on the settings page; the legacy network has none', async () => {
    const { router } = await open('/p/' + SITE)
    const item = (await menu(SITE)).find((i) => i.textContent!.includes('project.menu.autonomy'))!
    item.click()
    await settle()
    expect(router.currentRoute.value.name).toBe('settings')
    expect(document.querySelector('[data-autonomy="' + SITE + '"]')).not.toBeNull()
    await router.push('/p/' + SITE)
    await settle()
    expect((await menu('legacy')).map((i) => i.textContent!.trim())).not.toContain('project.menu.autonomy')
  })

  it('removes a member from the project only after a confirmation', async () => {
    const { calls } = await open('/p/' + SITE)
    const projects = useProjectsStore()
    projects.openDialog('members', SITE)
    await settle()
    expect($('#member_remove_alice')).toBeNull()
    const confirm = vi.spyOn(browser, 'confirm').mockReturnValue(false)
    $<HTMLButtonElement>('#member_remove_bob')!.click()
    await settle()
    expect(confirm).toHaveBeenCalledWith('project.members.remove_confirm')
    expect(calls).not.toContain('POST projects/' + SITE + '/members/remove')
    confirm.mockReturnValue(true)
    $<HTMLButtonElement>('#member_remove_bob')!.click()
    await settle()
    expect(calls).toContain('POST projects/' + SITE + '/members/remove')
    expect(projects.byID(SITE)!.members.map((m) => m.name)).not.toContain('bob')
    expect($('#member_remove_bob')).toBeNull()
    expect(document.body.textContent).toContain('project.members.removed')
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
    expect(document.body.textContent).toContain('project.delete.text')
    $<HTMLButtonElement>('#leave_confirm')!.click()
    await settle()
    expect(calls).toContain('POST projects/' + SITE + '/leave')
    expect(projects.byID(SITE)).toBeNull()
    expect(router.currentRoute.value.params.project).not.toBe(SITE)
    expect(router.currentRoute.value.name).toBe('project')
  })

  it('binds only the working folder of the legacy network, without an own name', async () => {
    const { requests, backend } = await open('/p/legacy')
    const projects = useProjectsStore()
    projects.openDialog('folder', 'legacy')
    await settle()
    expect($('#project_alias')).toBeNull()
    const input = $<HTMLInputElement>('#project_dir')!
    input.value = 'D:\\old'
    input.dispatchEvent(new Event('input'))
    $<HTMLFormElement>('#folder_form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    const last = requests[requests.length - 1]!
    expect(last.url).toBe('/ui/api/projects/legacy/binding')
    expect(JSON.parse(String(last.init.body))).toEqual({ dir: 'D:\\old' })
    expect(backend.projects.find((p) => p.legacy)!.dir).toBe('D:\\old')
    expect(projects.dialog).toBe('')
  })
})
