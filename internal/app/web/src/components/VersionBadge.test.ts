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

  it('checks on open, then lists every newer release and offers «Обновить»', async () => {
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    const { calls } = await open(async (_method, path) => {
      if (path === 'update/check') {
        await gate
        return { current: '0.5.0', latest: '0.6.1', available: true, enabled: true, text: 'Доступна версия 0.6.1.' }
      }
      if (path === 'update/changelog') return NOTES
      return undefined
    })
    await click('#version_badge')
    expect(calls).toContain('POST update/check')
    expect(calls).toContain('GET update/changelog')
    expect($('#update_checking')).not.toBeNull()
    expect($('#update_install')).toBeNull()
    release()
    await settle()
    expect($('#update_checking')).toBeNull()
    const versions = [...document.querySelectorAll<HTMLElement>('.release-note')].map((e) => e.dataset.version)
    expect(versions).toEqual(['0.6.1', '0.6.0'])
    expect($('#update_changelog')!.textContent).toContain('update.changelog.newer')
    expect($('#update_changelog')!.textContent).toContain('* fix the tray')
    expect($('#update_install')).not.toBeNull()
    expect($('#version_badge_dot')).not.toBeNull()
  })

  it('up to date: the notes of this version and no «Обновить»', async () => {
    await open((_method, path) => {
      if (path === 'update/check') return { current: '0.6.1', latest: '0.6.1', enabled: true, text: 'У вас актуальная версия.' }
      if (path === 'update/changelog') return { current: '0.6.1', newer: false, releases: [NOTES.releases[0]!] }
      return undefined
    }, { current: '0.6.1', enabled: true })
    await click('#version_badge')
    expect($('#update_changelog')!.textContent).toContain('update.changelog.this')
    expect(document.querySelectorAll('.release-note')).toHaveLength(1)
    expect($('#update_install')).toBeNull()
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
    await click('#update_install')
    expect(calls).toContain('POST update/apply')
    // The "update" events bring the progress.
    app.update = { current: '0.5.0', latest: '0.6.1', enabled: true, busy: true, installing: true, downloaded: 3 << 20, size: 10 << 20 }
    await settle()
    expect($('#update_progress')!.textContent).toContain('update.progress')
    expect($('#update_progress [role="progressbar"]')!.getAttribute('aria-valuenow')).toBe('30')
    expect($('#update_install')).toBeNull()
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
    expect($('#update_install')).toBeNull()
  })

  it('shows an app error on install', async () => {
    await open((_method, path) => {
      if (path === 'update/check') return { current: '0.5.0', latest: '0.6.1', available: true, enabled: true }
      if (path === 'update/changelog') return NOTES
      if (path === 'update/apply') throw new HttpError(500, 'Приложение не отвечает')
      return undefined
    })
    await click('#version_badge')
    await click('#update_install')
    expect($('#update_error')!.textContent).toContain('Приложение не отвечает')
  })
})
