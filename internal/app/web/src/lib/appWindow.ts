// One named app tab: the launcher (open.ts, the tray's /ui/open) brings it
// forward, or opens it once on the inbox. Only that tab answers the launcher;
// it keeps whatever page it shows, and no other tab is touched.

// The name and keys keep their old "dashboard" spelling so a tab opened by an
// earlier build is still found instead of a second one being opened.
export const APP_WINDOW_NAME = 'agentlink-dashboard'
export const APP_PATH = '/ui/inbox'
export const APP_ACTIVATION_KEY = 'agentlink' + '.dashboard.activate'
export const APP_CHANNEL = 'agentlink' + '.dashboard'

// claimAppWindow makes the named tab take the focus when the launcher asks;
// any other tab ignores the launcher.
export function claimAppWindow(win: Pick<Window, 'name' | 'focus' | 'addEventListener'> = window) {
  if (win.name !== APP_WINDOW_NAME) return
  const activate = () => {
    try { win.focus() } catch { /* focus remains browser-controlled */ }
  }
  win.addEventListener('storage', (event) => {
    if ((event as StorageEvent).key === APP_ACTIVATION_KEY && (event as StorageEvent).newValue) activate()
  })
  if (typeof BroadcastChannel !== 'undefined') {
    try { new BroadcastChannel(APP_CHANNEL).addEventListener('message', activate) } catch { /* channel unavailable */ }
  }
}

// signalAppWindow asks the named tab, wherever the browser keeps it, to come forward.
function signalAppWindow() {
  const nonce = Date.now() + ':' + Math.random().toString(36).slice(2)
  try { localStorage.setItem(APP_ACTIVATION_KEY, nonce) } catch { /* storage unavailable */ }
  if (typeof BroadcastChannel !== 'undefined') {
    try {
      const channel = new BroadcastChannel(APP_CHANNEL)
      channel.postMessage(nonce)
      channel.close()
    } catch { /* channel unavailable */ }
  }
}

// The part of a window the launcher uses; a test passes its own.
export type LauncherWindow = Pick<Window, 'open' | 'close'> & { name: string; location: Pick<Location, 'replace'> }

function adoptLauncher(win: LauncherWindow) {
  win.name = APP_WINDOW_NAME
  win.location.replace(APP_PATH)
}

// launchApp is the public /ui/open page: it brings the named tab forward as it
// is (a new one opens on the inbox) and closes itself; when the browser
// refuses a window, the launcher becomes the named tab.
export function launchApp(win: LauncherWindow = window) {
  let target: Window | null
  try {
    // An empty URL finds the named tab without loading anything into it.
    target = win.open('', APP_WINDOW_NAME)
  } catch {
    adoptLauncher(win)
    return
  }
  if (!target) {
    adoptLauncher(win)
    return
  }
  try {
    if (target.location.href === 'about:blank') target.location.replace(APP_PATH)
  } catch { /* another page's tab: leave it */ }
  try { target.focus() } catch { /* focus remains browser-controlled */ }
  signalAppWindow()
  try { win.close() } catch { /* browser may keep the launcher tab */ }
}
