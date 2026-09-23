// navigate moves the application shell to a route; main.ts points it at the
// router, a test at a recorder.
export const ROUTES = ['dashboard', 'inbox', 'welcome', 'project', 'chat', 'participants', 'settings'] as const
export type Route = (typeof ROUTES)[number]
export type Query = Record<string, string>
export type Params = Record<string, string>

type Navigator = { go: (route: Route, query?: Query, params?: Params) => unknown; current: () => string }

const navigator: Navigator = { go: () => {}, current: () => '' }

export function setNavigator(next: Navigator) {
  navigator.go = next.go
  navigator.current = next.current
}

export function navigate(route: string, query?: Query, params?: Params) {
  const known = (ROUTES as readonly string[]).includes(route) ? (route as Route) : 'dashboard'
  return navigator.go(known, query, params)
}

// openProject shows a project's page; openChat one of its chats.
export function openProject(project: string) {
  return navigate('project', undefined, { project })
}

export function openChat(project: string, chat: string, message = '') {
  return navigate('chat', message ? { message } : undefined, { project, chat })
}

// currentRoute is the route on screen, e.g. "chat".
export function currentRoute(): string {
  return navigator.current()
}
