import { describe, expect, it, vi } from 'vitest'
import { browser } from '@/lib/runtime'
import { fakeApi, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useProjectsStore } from '@/stores/projects'
import { fixture } from '@/test/backend'
import type { AppSettings, ProjectView, SaveResult } from '@/types'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))

// edit types into a field: "input" on every keystroke, "change" on leaving it.
async function edit(input: HTMLInputElement, value: string) {
  input.value = value
  input.dispatchEvent(new Event('input', { bubbles: true }))
  input.dispatchEvent(new Event('change', { bubbles: true }))
  await settle()
}

const field = (name: string) => $<HTMLInputElement>('#form [name="' + name + '"]')!

async function openSettings(saved: AppSettings, answer: (body: AppSettings) => SaveResult, legacy = true) {
  const sent: AppSettings[] = []
  let picked = 'E:\\docs'
  const api = fakeApi((method, path, body) => {
    if (path === 'projects') return legacy ? [fixture<ProjectView>('project_legacy')] : []
    if (path === 'projects/legacy/chats') return []
    if (path === 'settings' && method === 'POST') { sent.push(body as AppSettings); return answer(body as AppSettings) }
    if (path === 'pick-folder') return { path: picked }
    if (path === 'agent') return { text: '' }
    if (path === 'hooks') return {}
    throw new Error('unexpected call ' + method + ' ' + path)
  })
  await mountApp('/settings')
  await useProjectsStore().refreshAll()
  useAppStore().settings = saved
  await settle()
  return { api, sent, pick: (path: string) => { picked = path } }
}

describe('settings', () => {
  it('render saved projects as rows and save a row only when it is complete and unique', async () => {
    const { sent } = await openSettings({ node: 'n', areas: ['site'], projects: { site: { dir: 'E:\\site' } } }, (body) => ({ saved: true, settings: body }))
    const rows = () => $$('#projects > li')
    const parts = (li: HTMLElement) => {
      const inputs = li.querySelectorAll<HTMLInputElement>('input')
      const buttons = li.querySelectorAll<HTMLButtonElement>('button')
      return { area: inputs[0]!, dir: inputs[1]!, pick: buttons[0]!, remove: buttons[1]!, hooks: li.querySelector('.hint')! }
    }
    expect(rows()).toHaveLength(1)
    expect($('#projects_empty')).toBeNull()
    const first = parts(rows()[0]!)
    expect([first.area.value, first.dir.value]).toEqual(['site', 'E:\\site'])
    // A project row holds only its area, its folder, the hook status and «Удалить».
    expect(rows()[0]!.querySelectorAll('input')).toHaveLength(2)
    expect(rows()[0]!.querySelectorAll('button')).toHaveLength(2)

    await edit(first.dir, '')
    expect(sent).toHaveLength(0) // a row without a folder is not sent
    await edit(first.dir, 'E:\\site')
    expect(sent).toHaveLength(0) // an unchanged form is not sent again
    $<HTMLButtonElement>('#add_project')!.click()
    await settle()
    expect(rows()).toHaveLength(2)
    expect(sent).toHaveLength(0)
    const second = parts(rows()[1]!)
    await edit(second.area, ' docs ')
    expect(sent).toHaveLength(0)
    second.pick.click()
    await settle()
    expect(second.dir.value).toBe('E:\\docs')
    expect(sent).toHaveLength(1)
    expect(sent[0]!.projects).toEqual({ site: { dir: 'E:\\site' }, docs: { dir: 'E:\\docs' } })
    parts(rows()[0]!).remove.click()
    await settle()
    expect(rows()).toHaveLength(1)
    expect(sent).toHaveLength(2)
    expect(sent[1]!.projects).toEqual({ docs: { dir: 'E:\\docs' } })
    $<HTMLButtonElement>('#add_project')!.click()
    await settle()
    const third = parts(rows()[1]!)
    third.dir.value = 'E:\\other'
    third.dir.dispatchEvent(new Event('input', { bubbles: true }))
    await edit(third.area, 'docs')
    expect(sent).toHaveLength(2) // one area twice is not sent...
    expect($('#settings_result')!.textContent).toBe('error.projects_twice') // ...but named
  })

  it('save by themselves, apply the answer and reload only for another API address', async () => {
    let answer: SaveResult = {
      saved: true, settings: { node: 'saved', api: '127.0.0.1:7520', areas: ['dev'] },
      status: { configured: true, node: 'saved' }, dashboard: { total_messages: 7 },
    }
    const { api, sent } = await openSettings({ node: 'old', api: '127.0.0.1:7520', areas: [] }, () => answer)
    const reload = vi.spyOn(browser, 'reload').mockImplementation(() => {})
    const app = useAppStore()
    expect($('#settings_save')).toBeNull()
    expect($$('#form button[type="submit"]')).toHaveLength(0)
    field('node').value = 'saved'
    field('node').dispatchEvent(new Event('input', { bubbles: true }))
    await edit(field('areas'), 'dev')
    expect(sent).toHaveLength(1)
    // The answer is applied as it is; nothing is read again.
    expect(api.calls.filter((c) => c.startsWith('GET ') && c !== 'GET hooks' && !c.startsWith('GET projects'))).toEqual([])
    expect(app.settings?.node).toBe('saved')
    expect(app.status?.node).toBe('saved')
    expect(app.dashboard?.total_messages).toBe(7)
    expect(reload).not.toHaveBeenCalled()

    answer = { saved: true }
    await edit(field('node'), 'request-only')
    expect(sent).toHaveLength(2)
    expect(app.settings?.node).toBe('saved') // the request is not the answer

    answer = { saved: true, settings: { node: 'saved', api: '127.0.0.1:7520', areas: [] } }
    await edit(field('api'), '')
    expect(reload).not.toHaveBeenCalled() // the server fills in the same default
    answer = { saved: true, settings: { node: 'saved', api: '127.0.0.1:7599', areas: [] } }
    await edit(field('api'), '127.0.0.1:7599')
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('have no pairing code, and show the legacy folders only with a legacy network', async () => {
    const { sent } = await openSettings({ node: 'n', work_dir: 'E:\\work', areas: ['dev'] }, (body) => ({ saved: true, settings: body }), false)
    for (const gone of ['#code', '#generate', '#copy', '[data-settings-card="legacy"]', '#work_dir', '[name="areas"]']) expect($(gone), gone).toBeNull()
    expect(document.body.textContent).not.toContain('settings.code')
    await edit(field('node'), 'm')
    expect(sent).toHaveLength(1)
    expect(sent[0]).not.toHaveProperty('code')
    // The legacy fields travel unchanged.
    expect(sent[0]!.work_dir).toBe('E:\\work')
    expect(sent[0]!.areas).toEqual(['dev'])
  })
})