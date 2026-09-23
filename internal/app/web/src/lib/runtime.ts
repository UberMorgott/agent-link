// What the app wrote into the page: the per-run token, the build version, the
// CSP nonce for injected styles and the text dictionary (internal/app/strings.go).

function meta(name: string): string {
  return document.querySelector<HTMLMetaElement>('meta[name="agentlink-' + name + '"]')?.content ?? ''
}

function readStrings(): Record<string, string> {
  try {
    const value: unknown = JSON.parse(meta('strings') || '{}')
    return value && typeof value === 'object' ? (value as Record<string, string>) : {}
  } catch {
    return {}
  }
}

export const runtime = {
  token: meta('token'),
  // The app build this page was served by.
  version: meta('version'),
  nonce: meta('nonce'),
  strings: readStrings(),
}

// t returns the text for a key; fmt also fills its {name} placeholders.
export function t(key: string): string {
  return Object.prototype.hasOwnProperty.call(runtime.strings, key) ? runtime.strings[key]! : key
}

export function fmt(key: string, vars: Record<string, string | number>): string {
  return t(key).replace(/\{(\w+)\}/g, (m, name: string) => (name in vars ? String(vars[name]) : m))
}

// The page actions a test replaces.
export const browser = {
  reload: () => location.reload(),
  confirm: (text: string) => window.confirm(text),
}
