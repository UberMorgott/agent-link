import { browser, runtime, t } from './runtime'

export const TOKEN_HEADER = 'X-Agentlink-Token'
export const VERSION_HEADER = 'X-Agentlink-Version'

// The state slices the page keeps; a GET of one of them is shared while it runs.
export const CORE_SLICES = ['status', 'dashboard', 'participants', 'update', 'settings', 'chats', 'sessions'] as const
export type CoreSlice = (typeof CORE_SLICES)[number]

export class ApiError extends Error {
  status?: number
  constructor(message: string, status?: number) {
    super(message)
    this.status = status
  }
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
  if (body !== undefined) {
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
    const message = resp.status === 403 ? t("error.forbidden")
      : (data && typeof data === 'object' && 'error' in data && (data as { error?: string }).error) || t("error.internal")
    throw new ApiError(String(message), resp.status)
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
