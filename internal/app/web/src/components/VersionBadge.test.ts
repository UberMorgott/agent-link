import { describe, expect, it } from 'vitest'
import { SITE } from '@/test/backend'
import { HttpError, fakeBackend, mountApp, settle, type Handler } from '@/test/harness'
import { runtime } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { useProjectsStore } from '@/stores/projects'
import type { Changelog, UpdateStatus } from '@/types'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)

const NOTES: Changelog = {
  current: '0.5.0',
  newer: true,
  releases: [
    { version: '0.6.1', body: '* fix the tray', published: '2026-09-22T10:00:00Z' },
    { version: '0.6.0', body: '## What\'s Changed\n* projects' },
  ],
}

async function open(override: Handler, update: UpdateStatus = { current: '0.5.0', enabled: true }) {
  const api = fakeBackend(override)
  const mounted = await mountApp('/p/' + SITE)
  useAppStore().status = { configured: true, node: 'alice' }
  useAppStore().update = update
  await useProjectsStore().refreshAll()
  await settle()
  return { ...mounted, ...api }
}

async function click(sel: string) {
  $<HTMLElement>(sel)!.click()
  await settle()
}

describe('version badge', () => {
  it('shows the build version next to the logo, "dev" without one', async () => {
    await open(() => undefined)
    expect($('#version_badge')!.textContent!.trim()).toBe('v0.5.0')
    useAppStore().update = { current: 'dev', enabled: false }
    await settle()
    expect($('#version_badge')!.textContent!.trim()).toBe('dev')
    useAppStore().update = null
    runtime.version = '0.4.2'
    await settle()
    expect($('#version_badge')!.textContent!.trim()).toBe('v0.4.2')
  })

  it('checks on open, lists every newer release and installs it at once', async () => {
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    const { calls } = await open(async (_method, path) => {
      if (path === 'update/check') {
        await gate
        return { current: '0.5.0', latest: '0.6.1', available: true, enabled: true, text: 'Доступна версия 0.6.1.' }
      }
      if (path === 'update/changelog') return NOTES
      if (path === 'update/apply') return new Promise(() => {}) // the download runs
      return undefined
    })
    await click('#version_badge')
    expect(calls).toContain('POST update/check')
    expect(calls).toContain('GET update/changelog')
    expect($('#update_checking')).not.toBeNull()
    expect(calls).not.toContain('POST update/apply')
    release()
    await settle()
    expect($('#update_checking')).toBeNull()
    expect(calls).toContain('POST update/apply')
    expect(sessionStorage.getItem('agentlink.update.reopen')).toBe('1')
    const versions = [...document.querySelectorAll<HTMLElement>('.release-note')].map((e) => e.dataset.version)
    expect(versions).toEqual(['0.6.1', '0.6.0'])
    expect($('#update_changelog')!.textContent).toContain('update.changelog.newer')
    // The notes are Markdown: a list and a heading, not the raw text.
    expect($('.release-note[data-version="0.6.1"] li')!.textContent).toBe('fix the tray')
    expect($('.release-note[data-version="0.6.0"] h2')!.textContent).toBe("What's Changed")
    expect($('#update_changelog')!.textContent).not.toContain('* fix the tray')
    expect(document.querySelector('#update_install, #update_retry')).toBeNull()
  })

  it('up to date: the notes of this version and nothing to install', async () => {
    const { calls } = await open((_method, path) => {
      if (path === 'update/check') return { current: '0.6.1', latest: '0.6.1', enabled: true, text: 'У вас актуальная версия.' }
      if (path === 'update/changelog') return { current: '0.6.1', newer: false, releases: [NOTES.releases[0]!] }
      return undefined
    }, { current: '0.6.1', enabled: true })
    await click('#version_badge')
    expect($('#update_changelog')!.textContent).toContain('update.changelog.this')
    expect(document.querySelectorAll('.release-note')).toHaveLength(1)
    expect(calls).not.toContain('POST update/apply')
    expect($('#update_retry')).toBeNull()
    expect(sessionStorage.getItem('agentlink.update.reopen')).toBeNull()
    expect(document.body.textContent).toContain('У вас актуальная версия.')
  })

  it('installs with a download progress bar, then restarts', async () => {
    const { calls } = await open((_method, path) => {
      if (path === 'update/check') return { current: '0.5.0', latest: '0.6.1', available: true, enabled: true }
      if (path === 'update/changelog') return NOTES
      if (path === 'update/apply') return new Promise(() => {}) // the download runs
      return undefined
    })
    const app = useAppStore()
    await click('#version_badge')
    expect(calls).toContain('POST update/apply')
    // Before the first "update" event: an indeterminate bar.
    expect($('#update_restarting')).not.toBeNull()
    // The "update" events bring the progress.
    app.update = { current: '0.5.0', latest: '0.6.1', enabled: true, busy: true, installing: true, downloaded: 3 << 20, size: 10 << 20 }
    await settle()
    expect($('#update_progress')!.textContent).toContain('update.progress')
    expect($('#update_progress [role="progressbar"]')!.getAttribute('aria-valuenow')).toBe('30')
    expect($('#update_retry')).toBeNull()
    app.update = { current: '0.5.0', latest: '0.6.1', enabled: true, busy: true, restarting: true, text: 'Версия 0.6.1 установлена' }
    await settle()
    expect($('#update_progress')).toBeNull()
    expect($('#update_restarting')).not.toBeNull()
    expect(document.body.textContent).toContain('Версия 0.6.1 установлена')
  })

  it('shows a failed check and a rate-limited changelog', async () => {
    await open((_method, path) => {
      if (path === 'update/check') return { current: '0.5.0', enabled: true, failed: true, text: 'GitHub временно ограничил запросы, повторите после 15:04.' }
      if (path === 'update/changelog') return { current: '0.5.0', newer: false, releases: [], failed: true, text: 'GitHub временно ограничил запросы, повторите после 15:04.' }
      return undefined
    })
    await click('#version_badge')
    expect($('#update_error')!.textContent).toContain('повторите после 15:04')
    expect($('#update_changelog_error')!.textContent).toContain('повторите после 15:04')
    expect($('#update_retry')).not.toBeNull()
  })

  it('shows an app error on install and retries', async () => {
    let fail = true
    const { calls } = await open((_method, path) => {
      if (path === 'update/check') return { current: '0.5.0', latest: '0.6.1', available: true, enabled: true }
      if (path === 'update/changelog') return NOTES
      if (path === 'update/apply') {
        if (fail) throw new HttpError(500, 'Приложение не отвечает')
        return new Promise(() => {})
      }
      return undefined
    })
    await click('#version_badge')
    expect($('#update_error')!.textContent).toContain('Приложение не отвечает')
    expect(sessionStorage.getItem('agentlink.update.reopen')).toBeNull()
    fail = false
    await click('#update_retry')
    expect(calls.filter((c) => c === 'POST update/apply')).toHaveLength(2)
    expect($('#update_error')).toBeNull()
    expect($('#update_retry')).toBeNull()
  })

  it('opens again on the page loaded after the update', async () => {
    sessionStorage.setItem('agentlink.update.reopen', '1')
    const { calls } = await open((_method, path) => {
      if (path === 'update/check') return { current: '0.6.1', latest: '0.6.1', enabled: true, text: 'У вас актуальная версия.' }
      if (path === 'update/changelog') return { current: '0.6.1', newer: false, releases: [NOTES.releases[0]!] }
      return undefined
    }, { current: '0.6.1', enabled: true })
    await settle()
    expect(calls).toContain('GET update/changelog')
    expect($('#update_changelog')!.textContent).toContain('update.changelog.this')
    expect($('.release-note[data-version="0.6.1"]')).not.toBeNull()
    expect(sessionStorage.getItem('agentlink.update.reopen')).toBeNull()
  })
})
