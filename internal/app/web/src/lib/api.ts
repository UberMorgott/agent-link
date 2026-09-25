import { browser, runtime, t } from './runtime'

export const TOKEN_HEADER = 'X-Agentlink-Token'
export const VERSION_HEADER = 'X-Agentlink-Version'

// The state slices the page keeps; a GET of one of them is shared while it runs.
export const CORE_SLICES = ['status', 'dashboard', 'participants', 'update', 'settings', 'sessions'] as const
export type CoreSlice = (typeof CORE_SLICES)[number]

export class ApiError extends Error {
  status?: number
  code: string // the projects API's error code, e.g. "project_busy"; "" otherwise
  constructor(message: string, status?: number, code = '') {
    super(message)
    this.status = status
    this.code = code
  }
}

// projectPath is a path of one project's API: projects/{pid}[/rest].
export function projectPath(pid: string, rest = ''): string {
  return 'projects/' + encodeURIComponent(pid) + (rest ? '/' + rest : '')
}

// chatPath is a path of one chat inside a project.
export function chatPath(pid: string, chat: string, rest = ''): string {
  return projectPath(pid, 'chats/' + encodeURIComponent(chat) + (rest ? '/' + rest : ''))
}

const inFlight = new Map<string, Promise<unknown>>()

// reloadOnNewVersion reloads the page when the app answering is another build,
// as after a self-update: the old page cannot use the new app's token. A
// reconnect to the same build keeps the page.
export function reloadOnNewVersion(resp: Pick<Response, 'headers'> | undefined): boolean {
  const version = resp?.headers?.get?.(VERSION_HEADER) || ''
  if (!runtime.version || !version || version === runtime.version) return false
  browser.reload()
  return true
}

async function apiRequest<T>(method: string, path: string, body?: unknown): Promise<T> {
  const opts: RequestInit & { headers: Record<string, string> } = { method, headers: { [TOKEN_HEADER]: runtime.token } }
  if (body instanceof Blob) {
    // A file goes as it is: the raw body (uploadFile).
    opts.headers['Content-Type'] = 'application/octet-stream'
    opts.body = body
  } else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json'
    opts.body = JSON.stringify(body)
  }
  let resp: Response
  try {
    resp = await fetch('/ui/api/' + path, opts)
  } catch {
    throw new ApiError(t("error.no_app"))
  }
  if (reloadOnNewVersion(resp)) throw new ApiError(t("error.no_app"))
  const text = await resp.text()
  let data: unknown = text
  try { data = JSON.parse(text) } catch { /* plain-text error */ }
  if (!resp.ok) {
    // The token changes on every start: a tab left open from before gets 403.
    // A coded error reads as the page's own sentence for its code, else the
    // app's, else the generic one.
    const coded = data && typeof data === 'object' ? (data as { error?: string; code?: string }) : {}
    const code = typeof coded.code === 'string' ? coded.code : ''
    const known = code && t("error." + code) !== "error." + code ? t("error." + code) : ''
    const message = resp.status === 403 ? t("error.forbidden") : known || coded.error || t("error.internal")
    throw new ApiError(String(message), resp.status, code)
  }
  return data as T
}

export function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
  const key = method === 'GET' && body === undefined && (CORE_SLICES as readonly string[]).includes(path) ? path : ''
  if (key && inFlight.has(key)) return inFlight.get(key) as Promise<T>
  const request = apiRequest<T>(method, path, body)
  if (key) {
    inFlight.set(key, request)
    request.finally(() => {
      if (inFlight.get(key) === request) inFlight.delete(key)
    }).catch(() => {})
  }
  return request
}
