// navigate moves the application shell to a route; main.ts points it at the
// router, a test at a recorder.
export const ROUTES = ['dashboard', 'inbox', 'participants', 'settings'] as const
export type Route = (typeof ROUTES)[number]
export type Query = Record<string, string>

type Navigator = { go: (route: Route, query?: Query) => unknown; current: () => string }

const navigator: Navigator = { go: () => {}, current: () => '' }

export function setNavigator(next: Navigator) {
  navigator.go = next.go
  navigator.current = next.current
}

export function navigate(route: string, query?: Query) {
  const known = (ROUTES as readonly string[]).includes(route) ? (route as Route) : 'dashboard'
  return navigator.go(known, query)
}

// currentRoute is the route on screen, e.g. "inbox".
export function currentRoute(): string {
  return navigator.current()
}
