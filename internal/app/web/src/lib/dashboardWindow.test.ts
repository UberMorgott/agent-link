import { describe, expect, it, vi } from 'vitest'
import {
  DASHBOARD_ACTIVATION_KEY, DASHBOARD_HEARTBEAT_KEY, DASHBOARD_PATH, DASHBOARD_WINDOW_NAME, claimDashboardWindow, launchDashboard,
  type LauncherWindow,
} from './dashboardWindow'

function launcher(open: LauncherWindow['open']) {
  return { name: '', open, close: vi.fn(), location: { replace: vi.fn() } }
}

describe('the launcher', () => {
  it('opens the named dashboard tab, activates it and closes itself', () => {
    const target = { focus: vi.fn() }
    const win = launcher(vi.fn(() => target as unknown as Window))
    launchDashboard(win)
    expect(win.open).toHaveBeenCalledWith(DASHBOARD_PATH, DASHBOARD_WINDOW_NAME)
    expect(target.focus).toHaveBeenCalled()
    expect(JSON.parse(localStorage.getItem(DASHBOARD_ACTIVATION_KEY)!).route).toBe('dashboard')
    expect(win.close).toHaveBeenCalled()
    expect(win.location.replace).not.toHaveBeenCalled()
  })

  it('becomes the dashboard when the browser opens no window', () => {
    for (const open of [vi.fn(() => null), vi.fn(() => { throw new Error('blocked') })]) {
      const win = launcher(open)
      launchDashboard(win)
      expect(win.name).toBe(DASHBOARD_WINDOW_NAME)
      expect(win.location.replace).toHaveBeenCalledWith(DASHBOARD_PATH)
      expect(win.close).not.toHaveBeenCalled()
    }
  })
})

describe('the dashboard tab', () => {
  it('names itself, beats and follows activations to known routes', () => {
    const go = vi.fn()
    claimDashboardWindow(go)
    expect(window.name).toBe(DASHBOARD_WINDOW_NAME)
    expect(localStorage.getItem(DASHBOARD_HEARTBEAT_KEY)).not.toBeNull()
    window.dispatchEvent(new StorageEvent('storage', { key: DASHBOARD_ACTIVATION_KEY, newValue: JSON.stringify({ route: 'settings' }) }))
    window.dispatchEvent(new StorageEvent('storage', { key: DASHBOARD_ACTIVATION_KEY, newValue: JSON.stringify({ route: 'nowhere' }) }))
    window.dispatchEvent(new StorageEvent('storage', { key: DASHBOARD_ACTIVATION_KEY, newValue: 'not json' }))
    expect(go.mock.calls).toEqual([['settings']])
  })
})
