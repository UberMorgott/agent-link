// Test helpers: a fake /ui/api behind fetch, and the whole application shell
// mounted on an in-memory router.
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import PrimeVue from 'primevue/config'
import { vi } from 'vitest'
import { createMemoryHistory } from 'vue-router'
import App from '@/App.vue'
import { setNavigator, type Query } from '@/lib/nav'
import { createAppRouter } from '@/router'

export type Handler = (method: string, path: string, body: unknown) => unknown

export class HttpError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

// fakeApi answers /ui/api/<path> with handler's value as JSON; an HttpError
// becomes that status with {"error": message}. calls lists "METHOD path".
export function fakeApi(handler: Handler) {
  const calls: string[] = []
  const requests: { url: string; init: RequestInit }[] = []
  const fetchMock = vi.fn(async (url: string, init: RequestInit = {}) => {
    requests.push({ url, init })
    const method = init.method || 'GET'
    const path = url.replace(/^\/ui\/api\//, '')
    calls.push(method + ' ' + path)
    try {
      const body = typeof init.body === 'string' ? JSON.parse(init.body) : undefined
      const data = await handler(method, path, body)
      return new Response(JSON.stringify(data ?? {}), { status: 200 })
    } catch (error) {
      const status = error instanceof HttpError ? error.status : 500
      return new Response(JSON.stringify({ error: (error as Error).message }), { status })
    }
  })
  vi.stubGlobal('fetch', fetchMock)
  return { calls, requests, fetchMock }
}

// mountApp mounts the shell at path, e.g. "/inbox?chat=c1".
export async function mountApp(path: string) {
  const pinia = createPinia()
  setActivePinia(pinia)
  const router = createAppRouter(createMemoryHistory('/ui/'))
  setNavigator({
    go: (route: string, query?: Query) => router.push({ name: route, query: query || {} }),
    current: () => String(router.currentRoute.value.name || ''),
  })
  await router.push(path)
  await router.isReady()
  const wrapper = mount(App, {
    attachTo: document.body,
    global: { plugins: [pinia, router, [PrimeVue, { theme: 'none' }]] },
  })
  await flushPromises()
  return { wrapper, router, pinia }
}

// settle lets every pending request, watcher and render finish.
export async function settle() {
  for (let i = 0; i < 5; i++) await flushPromises()
}
