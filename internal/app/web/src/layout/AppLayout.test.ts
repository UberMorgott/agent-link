import { describe, expect, it } from 'vitest'
import { fakeApi, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))

async function open(path: string) {
  fakeApi((method, path) => {
    if (path === 'agent') return { text: '' }
    if (path === 'hooks') return {}
    throw new Error('unexpected call ' + method + ' ' + path)
  })
  const mounted = await mountApp(path)
  const app = useAppStore()
  app.status = { configured: true, connected: true, zerotier: true, node: 'local', online: 1, total: 2 }
  app.settings = { node: 'local' }
  app.update = { current: 'dev', enabled: true }
  app.dashboard = { status: { online: 1, total: 2, handler: 'claude' }, total_messages: 5, active_requests: 1, recent: [] }
  app.participants = [{ name: 'bob', online: true }]
  await settle()
  return mounted
}

// labelled: a form control has a visible or an aria label.
function labelled(el: Element) {
  return el.hasAttribute('aria-label') || el.hasAttribute('aria-labelledby') || !!el.closest('label')
    || (!!el.id && !!document.querySelector('label[for="' + CSS.escape(el.id) + '"]'))
}

describe('the application shell', () => {
  for (const route of ['dashboard', 'inbox', 'participants', 'settings']) {
    it('keeps one shell, one live region and real routes on ' + route, async () => {
      await open('/' + route)
      expect($$('#app-shell')).toHaveLength(1)
      expect($$('[aria-live]')).toHaveLength(1)
      const nav = $('nav')!
      expect(nav.getAttribute('aria-labelledby') && document.getElementById(nav.getAttribute('aria-labelledby')!)).toBeTruthy()
      for (const name of ['dashboard', 'inbox', 'participants', 'settings']) {
        expect($('a[data-route="' + name + '"]')!.getAttribute('href')).toBe('/ui/' + name)
      }
      expect($('a[data-route="' + route + '"]')!.className).toContain('active')
      const views = $$('[data-view]')
      expect(views.map((v) => v.dataset.view)).toEqual([route])
      expect(views[0]!.querySelectorAll('h1')).toHaveLength(1)
      expect(document.title).toBe('page.title.' + route)
      expect($$('fieldset')).toHaveLength(0)
      // A Nuxt UI switch or checkbox is a labelled button; the input it keeps
      // for the form is hidden from everyone (aria-hidden, out of the tab order).
      for (const control of $$('input, select, textarea, [role="switch"], [role="checkbox"]')) {
        if ((control as HTMLInputElement).type === 'hidden' || control.getAttribute('aria-hidden') === 'true') continue
        expect(labelled(control), control.outerHTML).toBe(true)
      }
    })
  }

  it('shows the dashboard cards and recent conversations', async () => {
    await open('/dashboard')
    expect($$('#dashboard_cards article')).toHaveLength(4)
    expect($('#dashboard_cards')!.textContent).toContain('settings.handler.claude')
    expect($('#dashboard_recent')!.textContent).toContain('dashboard.recent.empty')
    useAppStore().dashboard = { status: {}, recent: [{ peer: 'bob', preview: 'hello' }] }
    await settle()
    expect($('#dashboard_recent')!.textContent).toContain('bob')
    expect($('#dashboard_recent')!.textContent).toContain('hello')
  })

  it('gives participants their own controls and none to settings', async () => {
    const first = await open('/participants')
    for (const id of ['participants', 'participant_addr', 'add_participant', 'participants_result']) expect($('#' + id), id).not.toBeNull()
    first.wrapper.unmount()
    await open('/settings')
    for (const card of ['identity', 'handler', 'application', 'projects', 'updates', 'advanced']) {
      expect($('[data-settings-card="' + card + '"]'), card).not.toBeNull()
    }
    expect($('#settings_result')).not.toBeNull()
    for (const legacy of ['#members', '#add_peer', '[name="peer_addr"]', '#participant_addr']) expect($(legacy), legacy).toBeNull()
  })
})
