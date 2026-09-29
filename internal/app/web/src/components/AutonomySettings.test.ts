import { describe, expect, it } from 'vitest'
import { SITE, JOINING, fixture } from '@/test/backend'
import type { ProjectView } from '@/types'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useProjectsStore } from '@/stores/projects'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)

async function open() {
  const api = fakeBackend()
  await mountApp('/settings')
  useAppStore().status = { configured: true, node: 'alice' }
  await useProjectsStore().refreshAll()
  await settle()
  return api
}

const card = () => $('[data-autonomy="' + SITE + '"]')!
async function openProjectAutonomy() {
  useProjectsStore().openDialog('autonomy', SITE)
  await settle()
}
const bodies = (requests: { url: string; init: RequestInit }[], url: string) =>
  requests.filter((r) => r.url === url).map((r) => JSON.parse(String(r.init.body || '{}')))

// change sets a field or select and fires its "change".
async function change(el: HTMLInputElement | HTMLSelectElement, value: string) {
  el.value = value
  el.dispatchEvent(new Event('input', { bubbles: true }))
  el.dispatchEvent(new Event('change', { bubbles: true }))
  await settle()
}

describe('agent autonomy settings', () => {
  it('sets the autonomy of the local folders\' chats on the settings page, not of their temporary chats', async () => {
    const api = fakeBackend()
    const local = { ...fixture<ProjectView>('project_local'), id: 'LOCAL_SITE' }
    const temp = { ...fixture<ProjectView>('project_local_chat'), id: 'LOCAL_TEMP', autonomy: local.autonomy }
    api.backend.projects.push(local, temp)
    await mountApp('/settings')
    useAppStore().status = { configured: true, node: 'alice' }
    await useProjectsStore().refreshAll()
    await settle()
    expect($('[data-autonomy="LOCAL_SITE"] select.autonomy-mode')).not.toBeNull()
    expect($('[data-autonomy="LOCAL_TEMP"]')).toBeNull()
    expect(card()).toBeNull() // a network project's controls live in its menu
    expect($('#autonomy_local_hint')!.textContent).toContain('autonomy.local.hint')
    // The local chats have no composer: their agents' pause is here, and a
    // stopped temporary chat is listed to resume it.
    $<HTMLButtonElement>('[data-autonomy="LOCAL_SITE"] .autonomy-stop')!.click()
    await settle()
    expect(api.backend.projects.find((p) => p.id === 'LOCAL_SITE')!.autonomy!.stopped).toBe(true)
    api.backend.projects = api.backend.projects.map((p) => (p.id === 'LOCAL_TEMP' ? { ...p, autonomy: { ...p.autonomy!, stopped: true } } : p))
    await useProjectsStore().refreshList()
    await settle()
    $<HTMLButtonElement>('[data-autonomy="LOCAL_TEMP"] .autonomy-stop')!.click()
    await settle()
    expect(api.backend.projects.find((p) => p.id === 'LOCAL_TEMP')!.autonomy!.stopped).toBe(false)
  })

  it('lists the projects with every option described, and saves each change on its own', async () => {
    const { requests } = await open()
    expect(card()).toBeNull() // project controls live in the project menu
    await openProjectAutonomy()
    expect($('[data-autonomy="legacy"]')).toBeNull()
    expect($('[data-autonomy="' + JOINING + '"]')).toBeNull() // still connecting: no autonomy yet
    const text = card().textContent!
    for (const key of ['autonomy.mode.off.hint', 'autonomy.mode.asked.hint', 'autonomy.mode.full.hint', 'autonomy.depth.hint',
      'autonomy.turns.hint', 'autonomy.run.hint', 'autonomy.used']) expect(text).toContain(key)
    expect(document.body.textContent).toContain('autonomy.stop.hint')
    const mode = card().querySelector<HTMLSelectElement>('select.autonomy-mode')!
    expect(mode.value).toBe('full')

    await change(mode, 'asked')
    const binding = '/ui/api/projects/' + SITE + '/binding'
    expect(bodies(requests, binding)).toEqual([{ autonomy: 'asked' }])
    expect(useProjectsStore().byID(SITE)!.autonomy!.max_auto_depth).toBe(8)
    expect(card().textContent).not.toContain('autonomy.used') // full mode only

    const depth = card().querySelector<HTMLInputElement>('.autonomy-depth input, input.autonomy-depth')!
    await change(depth, '12')
    await change(depth, '')
    const turns = card().querySelector<HTMLInputElement>('.autonomy-turns input, input.autonomy-turns')!
    await change(turns, '1.5')
    expect(card().textContent).toContain('error.autonomy')
    await change(turns, '5')
    expect(bodies(requests, binding).slice(1)).toEqual([{ max_auto_depth: 12 }, { max_auto_depth: -1 }, { turns_per_hour: 5 }])
    // Its own requests: the settings form is never saved by them.
    expect(requests.some((r) => r.url === '/ui/api/settings' && r.init.method === 'POST')).toBe(false)
  })

  it('shows a budget pause with «Продолжить», which resumes it', async () => {
    const { backend, requests } = await open()
    await openProjectAutonomy()
    const projects = useProjectsStore()
    backend.projects = backend.projects.map((p) => (p.id === SITE ? { ...p, autonomy: { ...p.autonomy!, paused: true, pause_reason: 'run' } } : p))
    await projects.refreshList()
    await settle()
    expect(card().querySelector('.autonomy-paused')!.textContent).toContain('autonomy.paused')
    card().querySelector<HTMLButtonElement>('.autonomy-resume')!.click()
    await settle()
    expect(requests.map((r) => r.url)).toContain('/ui/api/projects/' + SITE + '/autonomy/resume')
    expect(projects.byID(SITE)!.autonomy!.paused).toBe(false)
    expect(card().querySelector('.autonomy-paused')).toBeNull()
  })

  it('turns the emergency stop on and off', async () => {
    const { backend, requests } = await open()
    expect($('#stop_all_on')).toBeNull()
    $<HTMLButtonElement>('#stop_all')!.click()
    await settle()
    expect(bodies(requests, '/ui/api/autonomy/stop')).toEqual([{ on: true }])
    expect(backend.stopAll).toBe(true)
    expect(useAppStore().status!.stop_all).toBe(true)
    expect($('#stop_all_on')).not.toBeNull()
    $<HTMLButtonElement>('#stop_all')!.click()
    await settle()
    expect(bodies(requests, '/ui/api/autonomy/stop')).toEqual([{ on: true }, { on: false }])
    expect($('#stop_all_on')).toBeNull()
  })
})
