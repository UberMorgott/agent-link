// Every icon the app names, from the Lucide set. The names are written out in
// full so the Nuxt UI build scan (vite.config.ts) bundles each one: the page
// never fetches an icon at run time.
export const icons: Record<string, string> = {
  dashboard: 'i-lucide-layout-grid',
  participants: 'i-lucide-users',
  settings: 'i-lucide-settings',
  plus: 'i-lucide-plus',
  archive: 'i-lucide-archive',
  finish: 'i-lucide-circle-check',
  // The project menu.
  more: 'i-lucide-ellipsis',
  invite: 'i-lucide-user-plus',
  rename: 'i-lucide-pencil',
  folder: 'i-lucide-folder',
  leave: 'i-lucide-log-out',
  delete: 'i-lucide-trash-2',
  remove: 'i-lucide-user-minus',
  eye: 'i-lucide-eye',
  eyeoff: 'i-lucide-eye-off',
  copy: 'i-lucide-copy',
  join: 'i-lucide-log-in',
  back: 'i-lucide-chevron-left',
  info: 'i-lucide-info',
  send: 'i-lucide-arrow-up',
  menu: 'i-lucide-menu',
  expand: 'i-lucide-chevron-down',
  collapse: 'i-lucide-chevron-up',
  fold: 'i-lucide-chevron-right',
  close: 'i-lucide-x',
  sun: 'i-lucide-sun',
  moon: 'i-lucide-moon',
  system: 'i-lucide-monitor',
  palette: 'i-lucide-palette',
  // Chat marks: delivery ticks and who wrote a message.
  queued: 'i-lucide-clock',
  delivered: 'i-lucide-check',
  read: 'i-lucide-check-check',
  held: 'i-lucide-circle-alert',
  agent: 'i-lucide-bot',
  human: 'i-lucide-user',
}

export function icon(name: string): string {
  return icons[name] ?? 'i-lucide-circle'
}
