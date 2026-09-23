// One named dashboard tab: the launcher (open.ts) opens or activates it by
// name, and the tab tells it apart by a heartbeat in localStorage.
import { ROUTES } from './nav'

export const DASHBOARD_WINDOW_NAME = 'agentlink-dashboard'
export const DASHBOARD_PATH = '/ui/dashboard'
export const DASHBOARD_HEARTBEAT_KEY = 'agentlink' + '.dashboard.heartbeat'
export const DASHBOARD_ACTIVATION_KEY = 'agentlink' + '.dashboard.activate'
export const DASHBOARD_CHANNEL = 'agentlink' + '.dashboard'

export interface Activation { route?: string; at?: number; nonce?: string }

function writeDashboardHeartbeat() {
  try { localStorage.setItem(DASHBOARD_HEARTBEAT_KEY, String(Date.now())) } catch { /* storage unavailable */ }
}

function parseActivation(raw: string): Activation | null {
  try { return JSON.parse(raw) as Activation } catch { return null }
}

// claimDashboardWindow names this tab the dashboard and answers activations:
// go to the asked route and take the focus.
export function claimDashboardWindow(go: (route: string) => void) {
  window.name = DASHBOARD_WINDOW_NAME
  const activate = (message: Activation | null) => {
    if (!message || !(ROUTES as readonly string[]).includes(message.route || '')) return
    go(message.route!)
    try { window.focus() } catch { /* focus remains browser-controlled */ }
  }
  writeDashboardHeartbeat()
  setInterval(writeDashboardHeartbeat, 1000)
  document.addEventListener('visibilitychange', writeDashboardHeartbeat)
  addEventListener('storage', (event) => {
    if (event.key === DASHBOARD_ACTIVATION_KEY && event.newValue) activate(parseActivation(event.newValue))
  })
  if (typeof BroadcastChannel !== 'undefined') {
    try {
      const channel = new BroadcastChannel(DASHBOARD_CHANNEL)
      channel.addEventListener('message', (event: MessageEvent<Activation>) => activate(event.data))
    } catch { /* channel unavailable */ }
  }
}

function signalDashboard() {
  const message: Activation = { route: 'dashboard', at: Date.now(), nonce: Math.random().toString(36).slice(2) }
  try { localStorage.setItem(DASHBOARD_ACTIVATION_KEY, JSON.stringify(message)) } catch { /* storage unavailable */ }
  if (typeof BroadcastChannel !== 'undefined') {
    try {
      const channel = new BroadcastChannel(DASHBOARD_CHANNEL)
      channel.postMessage(message)
      channel.close()
    } catch { /* channel unavailable */ }
  }
}

// The part of a window the launcher uses; a test passes its own.
export type LauncherWindow = Pick<Window, 'open' | 'close'> & { name: string; location: Pick<Location, 'replace'> }

function adoptLauncher(win: LauncherWindow) {
  win.name = DASHBOARD_WINDOW_NAME
  win.location.replace(DASHBOARD_PATH)
}

// launchDashboard is the public /ui/open page: it opens (or activates) the
// named dashboard tab and closes itself; when the browser refuses a new
// window, the launcher becomes the dashboard.
export function launchDashboard(win: LauncherWindow = window) {
  let target: Window | null
  try {
    target = win.open(DASHBOARD_PATH, DASHBOARD_WINDOW_NAME)
  } catch {
    adoptLauncher(win)
    return
  }
  if (!target) {
    adoptLauncher(win)
    return
  }
  try { target.focus() } catch { /* focus remains browser-controlled */ }
  signalDashboard()
  try { win.close() } catch { /* browser may keep the launcher tab */ }
}
