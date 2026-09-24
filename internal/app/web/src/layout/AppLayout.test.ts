import { describe, expect, it } from 'vitest'
import { SITE } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useProjectsStore } from '@/stores/projects'
import { UI_STORAGE_KEY, useLayout } from './composables/layout'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))

const CHAT = '7b8b965ad4bca0e41ab51de7b31363a1'
const PATHS: Record<string, string> = {
  dashboard: '/dashboard', welcome: '/welcome', project: '/p/' + SITE, chat: '/p/' + SITE + '/c/' + CHAT,
  participants: '/participants', settings: '/settings',
}

async function open(path: string) {
  const api = fakeBackend()
  const mounted = await mountApp(path)
  const app = useAppStore()
  app.status = { configured: true, connected: true, zerotier: true, node: 'alice', online: 1, total: 2 }
  app.settings = { node: 'alice' }
  app.update = { current: 'dev', enabled: true }
  app.dashboard = { status: { online: 1, total: 2, handler: 'claude' }, total_messages: 5, active_requests: 1, recent: [] }
  app.participants = [{ name: 'bob', online: true }]
  await useProjectsStore().refreshAll()
  await settle()
  return { ...mounted, api }
}

// labelled: a form control has a visible or an aria label.
function labelled(el: Element) {
  return el.hasAttribute('aria-label') || el.hasAttribute('aria-labelledby') || !!el.closest('label')
    || (!!el.id && !!document.querySelector('label[for="' + CSS.escape(el.id) + '"]'))
}

describe('the application shell', () => {
  for (const route of Object.keys(PATHS)) {
    it('keeps one shell, one live region and real routes on ' + route, async () => {
      await open(PATHS[route]!)
      expect($$('#app-shell')).toHaveLength(1)
      expect($$('[aria-live]')).toHaveLength(1)
      const nav = $('nav')!
      expect(nav.getAttribute('aria-labelledby') && document.getElementById(nav.getAttribute('aria-labelledby')!)).toBeTruthy()
      for (const name of ['dashboard', 'participants', 'settings']) {
        expect($('a[data-route="' + name + '"]')!.getAttribute('href')).toBe('/ui/' + name)
      }
      // The old "messages" page is gone from the navigation.
      expect($('a[data-route="inbox"]')).toBeNull()
      if ($('a[data-route="' + route + '"]')) expect($('a[data-route="' + route + '"]')!.className).toContain('active')
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

  it('lists the projects as a tree, legacy last, the open one unfolded', async () => {
    await open('/p/' + SITE)
    const tree = $$('#project_tree > li')
    expect(tree.map((li) => li.dataset.project)).toEqual(['NBSWY3DPEB3W64TMMQQGC3DUMU', SITE, 'legacy'])
    expect(tree[0]!.textContent).toContain('projects.connecting')
    expect(tree[1]!.querySelector('.project-name')!.textContent).toBe('Мой сайт')
    expect(tree[1]!.querySelector('.project-dot')!.className).toContain('on')
    expect(tree[1]!.querySelectorAll('.chat-row')).toHaveLength(1)
    expect(tree[2]!.querySelector('.project-chats')).toBeNull()
    tree[2]!.querySelector<HTMLButtonElement>('.project-fold')!.click()
    await settle()
    expect(tree[2]!.querySelectorAll('.chat-row')).toHaveLength(1)
  })

  it('picks the theme, accent and background in the appearance panel', async () => {
    await open('/dashboard')
    expect($('#theme_panel')).toBeNull()
    $<HTMLButtonElement>('#theme_config')!.click()
    await settle()
    expect($$('#theme_mode button').map((b) => b.dataset.mode)).toEqual(['light', 'dark', 'system'])
    expect($$('#theme_primary button')).toHaveLength(11)
    expect($$('#theme_surface button')).toHaveLength(5)
    for (const b of $$('#theme_panel button')) expect(labelled(b) || !!b.textContent!.trim(), b.outerHTML).toBe(true)
    $<HTMLButtonElement>('#theme_mode [data-mode="dark"]')!.click()
    $<HTMLButtonElement>('#theme_primary [data-color="rose"]')!.click()
    $<HTMLButtonElement>('#theme_surface [data-surface="zinc"]')!.click()
    await settle()
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(document.documentElement.style.getPropertyValue('--ui-color-primary-500')).toBe('#f43f5e')
    expect($('#theme_primary [data-color="rose"]')!.getAttribute('aria-pressed')).toBe('true')
    expect($('#theme_surface [data-surface="zinc"]')!.getAttribute('aria-pressed')).toBe('true')
    expect($('#theme_mode [data-mode="dark"]')!.getAttribute('aria-pressed')).toBe('true')
    expect($('#theme_saved')!.textContent).toContain('theme.saved')
    expect(JSON.parse(localStorage.getItem(UI_STORAGE_KEY)!)).toEqual({ theme: 'dark', primary: 'rose', surface: 'zinc' })
  })

  it('closes the phone drawer when a project dialog opens from it', async () => {
    await open('/p/' + SITE)
    const { layoutState } = useLayout()
    layoutState.mobileMenuActive = true
    useProjectsStore().openDialog('invite', SITE)
    await settle()
    expect(layoutState.mobileMenuActive).toBe(false)
  })

  it('sends /inbox to the last used project, or the welcome screen without any', async () => {
    const first = await open('/p/' + SITE)
    first.wrapper.unmount()
    const again = await open('/inbox')
    expect(again.router.currentRoute.value.params.project).toBe(SITE)
    again.wrapper.unmount()
    localStorage.clear()
    fakeBackend((_method, path) => (path === 'projects' ? [] : undefined))
    const { router } = await mountApp('/inbox')
    expect(router.currentRoute.value.name).toBe('welcome')
  })

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
    for (const card of ['identity', 'handler', 'application', 'legacy', 'updates', 'advanced']) {
      expect($('[data-settings-card="' + card + '"]'), card).not.toBeNull()
    }
    expect($('#settings_result')).not.toBeNull()
    for (const legacy of ['#members', '#add_peer', '[name="peer_addr"]', '#participant_addr']) expect($(legacy), legacy).toBeNull()
  })
})
