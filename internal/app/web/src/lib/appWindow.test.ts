import { describe, expect, it, vi } from 'vitest'
import {
  APP_ACTIVATION_KEY, APP_PATH, APP_WINDOW_NAME, claimAppWindow, launchApp,
  type LauncherWindow,
} from './appWindow'

function launcher(open: LauncherWindow['open']) {
  return { name: '', open, close: vi.fn(), location: { replace: vi.fn() } }
}

describe('the launcher', () => {
  it('brings the named tab forward as it is and closes itself', () => {
    const target = { focus: vi.fn(), location: { href: 'http://127.0.0.1/ui/p/P/c/C', replace: vi.fn() } }
    const win = launcher(vi.fn(() => target as unknown as Window))
    launchApp(win)
    expect(win.open).toHaveBeenCalledWith('', APP_WINDOW_NAME)
    expect(target.location.replace).not.toHaveBeenCalled()
    expect(target.focus).toHaveBeenCalled()
    expect(localStorage.getItem(APP_ACTIVATION_KEY)).not.toBeNull()
    expect(win.close).toHaveBeenCalled()
    expect(win.location.replace).not.toHaveBeenCalled()
  })

  it('opens a new named tab on the inbox', () => {
    const target = { focus: vi.fn(), location: { href: 'about:blank', replace: vi.fn() } }
    const win = launcher(vi.fn(() => target as unknown as Window))
    launchApp(win)
    expect(target.location.replace).toHaveBeenCalledWith(APP_PATH)
    expect(APP_PATH).toBe('/ui/inbox')
  })

  it('becomes the named tab when the browser opens no window', () => {
    for (const open of [vi.fn(() => null), vi.fn(() => { throw new Error('blocked') })]) {
      const win = launcher(open)
      launchApp(win)
      expect(win.name).toBe(APP_WINDOW_NAME)
      expect(win.location.replace).toHaveBeenCalledWith(APP_PATH)
      expect(win.close).not.toHaveBeenCalled()
    }
  })
})

describe('the tabs', () => {
  const tab = (name: string) => {
    const target = new EventTarget()
    return Object.assign(target, { name, focus: vi.fn() })
  }
  const activation = () => new StorageEvent('storage', { key: APP_ACTIVATION_KEY, newValue: 'n1' })

  it('only the named tab takes the focus; it keeps its page', () => {
    const named = tab(APP_WINDOW_NAME)
    const other = tab('')
    claimAppWindow(named as unknown as Window)
    claimAppWindow(other as unknown as Window)
    named.dispatchEvent(activation())
    other.dispatchEvent(activation())
    named.dispatchEvent(new StorageEvent('storage', { key: 'other', newValue: 'x' }))
    expect(named.focus).toHaveBeenCalledTimes(1)
    expect(other.focus).not.toHaveBeenCalled()
  })
})
