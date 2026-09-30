import { describe, expect, it, vi } from 'vitest'
import {
  DASHBOARD_ACTIVATION_KEY, DASHBOARD_PATH, DASHBOARD_WINDOW_NAME, claimDashboardWindow, launchDashboard,
  type LauncherWindow,
} from './dashboardWindow'

function launcher(open: LauncherWindow['open']) {
  return { name: '', open, close: vi.fn(), location: { replace: vi.fn() } }
}

describe('the launcher', () => {
  it('brings the named tab forward as it is and closes itself', () => {
    const target = { focus: vi.fn(), location: { href: 'http://127.0.0.1/ui/p/P/c/C', replace: vi.fn() } }
    const win = launcher(vi.fn(() => target as unknown as Window))
    launchDashboard(win)
    expect(win.open).toHaveBeenCalledWith('', DASHBOARD_WINDOW_NAME)
    expect(target.location.replace).not.toHaveBeenCalled()
    expect(target.focus).toHaveBeenCalled()
    expect(localStorage.getItem(DASHBOARD_ACTIVATION_KEY)).not.toBeNull()
    expect(win.close).toHaveBeenCalled()
    expect(win.location.replace).not.toHaveBeenCalled()
  })

  it('opens a new named tab on the dashboard', () => {
    const target = { focus: vi.fn(), location: { href: 'about:blank', replace: vi.fn() } }
    const win = launcher(vi.fn(() => target as unknown as Window))
    launchDashboard(win)
    expect(target.location.replace).toHaveBeenCalledWith(DASHBOARD_PATH)
  })

  it('becomes the named tab when the browser opens no window', () => {
    for (const open of [vi.fn(() => null), vi.fn(() => { throw new Error('blocked') })]) {
      const win = launcher(open)
      launchDashboard(win)
      expect(win.name).toBe(DASHBOARD_WINDOW_NAME)
      expect(win.location.replace).toHaveBeenCalledWith(DASHBOARD_PATH)
      expect(win.close).not.toHaveBeenCalled()
    }
  })
})

describe('the tabs', () => {
  const tab = (name: string) => {
    const target = new EventTarget()
    return Object.assign(target, { name, focus: vi.fn() })
  }
  const activation = () => new StorageEvent('storage', { key: DASHBOARD_ACTIVATION_KEY, newValue: 'n1' })

  it('only the named tab takes the focus; it keeps its page', () => {
    const named = tab(DASHBOARD_WINDOW_NAME)
    const other = tab('')
    claimDashboardWindow(named as unknown as Window)
    claimDashboardWindow(other as unknown as Window)
    named.dispatchEvent(activation())
    other.dispatchEvent(activation())
    named.dispatchEvent(new StorageEvent('storage', { key: 'other', newValue: 'x' }))
    expect(named.focus).toHaveBeenCalledTimes(1)
    expect(other.focus).not.toHaveBeenCalled()
  })
})
