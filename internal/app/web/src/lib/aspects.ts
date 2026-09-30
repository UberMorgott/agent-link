import { contrast, MIN_CONTRAST, pageColor, surfacePalette } from '@/lib/palettes'

// How an aspect's colour meets text, for the contrast guard:
// text  - the colour is the text on the page (links, statuses, notices);
// tint  - a light wash of it sits behind the body text (bubbles, selection);
// badge - it fills a badge whose label is the page colour (unread counts).
export type AspectKind = 'text' | 'tint' | 'badge'

export interface Aspect {
  name: string
  // The CSS variable the colour goes into (styles.css defines its default).
  token: string
  // What the swatch shows while nothing is picked.
  shown: string
  kind: AspectKind
  // Share of the colour in a tint (styles.css mixes it into the page colour).
  share?: number
  // Nuxt UI's own variable that follows the pick, so its buttons match.
  ui?: string
}

// ASPECTS are the parts of the app the palette colours one by one. The message
// aspects have no single default: each member's messages keep its own colour
// (styles.css --who) until the user picks one for all of them.
export const ASPECTS: readonly Aspect[] = [
  { name: 'accent', token: '--ui-primary', shown: 'var(--app-accent)', kind: 'text' },
  { name: 'mine', token: '--app-msg-mine', shown: '', kind: 'tint', share: 0.21 },
  { name: 'other', token: '--app-msg-other', shown: '', kind: 'tint', share: 0.13 },
  { name: 'agent', token: '--app-msg-agent', shown: '', kind: 'tint', share: 0.21 },
  { name: 'notice', token: '--app-notice', shown: 'var(--app-notice)', kind: 'text' },
  { name: 'link', token: '--app-link', shown: 'var(--app-link)', kind: 'text' },
  { name: 'online', token: '--app-status-online', shown: 'var(--app-status-online)', kind: 'text' },
  { name: 'working', token: '--app-status-working', shown: 'var(--app-status-working)', kind: 'text' },
  { name: 'waiting', token: '--app-status-waiting', shown: 'var(--app-status-waiting)', kind: 'text' },
  { name: 'paused', token: '--app-status-paused', shown: 'var(--app-status-paused)', kind: 'text', ui: '--ui-warning' },
  { name: 'error', token: '--app-status-error', shown: 'var(--app-status-error)', kind: 'text', ui: '--ui-error' },
  { name: 'unread', token: '--app-unread', shown: 'var(--app-unread)', kind: 'badge' },
  { name: 'focus', token: '--app-focus', shown: 'var(--app-focus)', kind: 'tint', share: 0.3 },
]

export type AspectColors = Record<string, string>

const HEX = /^#[0-9a-f]{6}$/

// cleanColors keeps the known aspects with a #rrggbb colour.
export function cleanColors(value: unknown): AspectColors {
  const out: AspectColors = {}
  if (!value || typeof value !== 'object' || Array.isArray(value)) return out
  for (const [name, colour] of Object.entries(value as Record<string, unknown>)) {
    if (typeof colour !== 'string') continue
    const hex = colour.toLowerCase()
    if (HEX.test(hex) && ASPECTS.some((a) => a.name === name)) out[name] = hex
  }
  return out
}

// mix is color-mix(in srgb, a share, b) of two #rrggbb colours.
export function mix(a: string, b: string, share: number): string {
  let out = '#'
  for (const i of [1, 3, 5]) {
    const v = Math.round(parseInt(a.slice(i, i + 2), 16) * share + parseInt(b.slice(i, i + 2), 16) * (1 - share))
    out += v.toString(16).padStart(2, '0')
  }
  return out
}

// aspectContrast is the contrast of the text an aspect's colour meets: Nuxt
// UI's page (--ui-bg) and body text (--ui-text, the background scale's 700 or
// 200 in the dark).
export function aspectContrast(aspect: Aspect, colour: string, surface: string, dark: boolean): number {
  const page = pageColor(surface, dark)
  const text = surfacePalette(surface)[dark ? 200 : 700]
  if (aspect.kind === 'tint') return contrast(text, mix(colour, page, aspect.share ?? 0.2))
  return contrast(colour, page)
}

export function lowContrast(aspect: Aspect, colour: string, surface: string, dark: boolean): boolean {
  return aspectContrast(aspect, colour, surface, dark) < MIN_CONTRAST
}

// resolveColour is the #rrggbb the page draws a CSS colour with ('' where the
// browser cannot say, e.g. without a canvas).
export function resolveColour(css: string): string {
  if (!css || typeof OffscreenCanvas === 'undefined') return ''
  const probe = document.createElement('span')
  probe.style.color = css
  probe.style.display = 'none'
  document.body.appendChild(probe)
  const computed = getComputedStyle(probe).color
  probe.remove()
  const ctx = new OffscreenCanvas(1, 1).getContext('2d')
  if (!ctx || !computed) return ''
  ctx.fillStyle = computed
  ctx.fillRect(0, 0, 1, 1)
  const [r, g, b] = ctx.getImageData(0, 0, 1, 1).data
  return '#' + [r, g, b].map((v) => (v ?? 0).toString(16).padStart(2, '0')).join('')
}
