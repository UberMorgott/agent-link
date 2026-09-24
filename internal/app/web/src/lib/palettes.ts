// The colours the appearance panel offers (layout/AppConfigurator.vue): the
// same accent and background scales as the PrimeVue Aura theme, i.e. the
// Tailwind palettes. layout/composables/layout.ts writes the chosen scales over
// Nuxt UI's --ui-color-primary-* and --ui-color-neutral-*, which every
// background, border and text colour of the page is built from.

export type Palette = Record<Shade, string>
export type Shade = (typeof SHADES)[number]

export interface NamedPalette {
  name: string
  palette: Palette
}

export const SHADES = [50, 100, 200, 300, 400, 500, 600, 700, 800, 900, 950] as const

export const DEFAULT_PRIMARY = 'emerald'
export const DEFAULT_SURFACE = 'neutral'

/** Background (neutral) scales. */
export const surfaces: NamedPalette[] = [
  { name: 'slate', palette: { 50: '#f8fafc', 100: '#f1f5f9', 200: '#e2e8f0', 300: '#cbd5e1', 400: '#94a3b8', 500: '#64748b', 600: '#475569', 700: '#334155', 800: '#1e293b', 900: '#0f172a', 950: '#020617' } },
  { name: 'gray', palette: { 50: '#f9fafb', 100: '#f3f4f6', 200: '#e5e7eb', 300: '#d1d5db', 400: '#9ca3af', 500: '#6b7280', 600: '#4b5563', 700: '#374151', 800: '#1f2937', 900: '#111827', 950: '#030712' } },
  { name: 'zinc', palette: { 50: '#fafafa', 100: '#f4f4f5', 200: '#e4e4e7', 300: '#d4d4d8', 400: '#a1a1aa', 500: '#71717a', 600: '#52525b', 700: '#3f3f46', 800: '#27272a', 900: '#18181b', 950: '#09090b' } },
  { name: 'neutral', palette: { 50: '#fafafa', 100: '#f5f5f5', 200: '#e5e5e5', 300: '#d4d4d4', 400: '#a3a3a3', 500: '#737373', 600: '#525252', 700: '#404040', 800: '#262626', 900: '#171717', 950: '#0a0a0a' } },
  { name: 'stone', palette: { 50: '#fafaf9', 100: '#f5f5f4', 200: '#e7e5e4', 300: '#d6d3d1', 400: '#a8a29e', 500: '#78716c', 600: '#57534e', 700: '#44403c', 800: '#292524', 900: '#1c1917', 950: '#0c0a09' } },
]

/** Accent scales. "noir" has none of its own: it takes the background scale. */
export const primaryColors: NamedPalette[] = [
  { name: 'noir', palette: surfaces.find((s) => s.name === DEFAULT_SURFACE)!.palette },
  { name: 'emerald', palette: { 50: '#ecfdf5', 100: '#d1fae5', 200: '#a7f3d0', 300: '#6ee7b7', 400: '#34d399', 500: '#10b981', 600: '#059669', 700: '#047857', 800: '#065f46', 900: '#064e3b', 950: '#022c22' } },
  { name: 'green', palette: { 50: '#f0fdf4', 100: '#dcfce7', 200: '#bbf7d0', 300: '#86efac', 400: '#4ade80', 500: '#22c55e', 600: '#16a34a', 700: '#15803d', 800: '#166534', 900: '#14532d', 950: '#052e16' } },
  { name: 'teal', palette: { 50: '#f0fdfa', 100: '#ccfbf1', 200: '#99f6e4', 300: '#5eead4', 400: '#2dd4bf', 500: '#14b8a6', 600: '#0d9488', 700: '#0f766e', 800: '#115e59', 900: '#134e4a', 950: '#042f2e' } },
  { name: 'sky', palette: { 50: '#f0f9ff', 100: '#e0f2fe', 200: '#bae6fd', 300: '#7dd3fc', 400: '#38bdf8', 500: '#0ea5e9', 600: '#0284c7', 700: '#0369a1', 800: '#075985', 900: '#0c4a6e', 950: '#082f49' } },
  { name: 'blue', palette: { 50: '#eff6ff', 100: '#dbeafe', 200: '#bfdbfe', 300: '#93c5fd', 400: '#60a5fa', 500: '#3b82f6', 600: '#2563eb', 700: '#1d4ed8', 800: '#1e40af', 900: '#1e3a8a', 950: '#172554' } },
  { name: 'indigo', palette: { 50: '#eef2ff', 100: '#e0e7ff', 200: '#c7d2fe', 300: '#a5b4fc', 400: '#818cf8', 500: '#6366f1', 600: '#4f46e5', 700: '#4338ca', 800: '#3730a3', 900: '#312e81', 950: '#1e1b4b' } },
  { name: 'violet', palette: { 50: '#f5f3ff', 100: '#ede9fe', 200: '#ddd6fe', 300: '#c4b5fd', 400: '#a78bfa', 500: '#8b5cf6', 600: '#7c3aed', 700: '#6d28d9', 800: '#5b21b6', 900: '#4c1d95', 950: '#2e1065' } },
  { name: 'amber', palette: { 50: '#fffbeb', 100: '#fef3c7', 200: '#fde68a', 300: '#fcd34d', 400: '#fbbf24', 500: '#f59e0b', 600: '#d97706', 700: '#b45309', 800: '#92400e', 900: '#78350f', 950: '#451a03' } },
  { name: 'orange', palette: { 50: '#fff7ed', 100: '#ffedd5', 200: '#fed7aa', 300: '#fdba74', 400: '#fb923c', 500: '#f97316', 600: '#ea580c', 700: '#c2410c', 800: '#9a3412', 900: '#7c2d12', 950: '#431407' } },
  { name: 'rose', palette: { 50: '#fff1f2', 100: '#ffe4e6', 200: '#fecdd3', 300: '#fda4af', 400: '#fb7185', 500: '#f43f5e', 600: '#e11d48', 700: '#be123c', 800: '#9f1239', 900: '#881337', 950: '#4c0519' } },
]

export function surfacePalette(name: string): Palette {
  return (surfaces.find((s) => s.name === name) ?? surfaces.find((s) => s.name === DEFAULT_SURFACE)!).palette
}

/** The accent scale; noir follows the chosen background. */
export function primaryPalette(name: string, surface: string): Palette {
  if (name === 'noir') return surfacePalette(surface)
  return (primaryColors.find((c) => c.name === name) ?? primaryColors.find((c) => c.name === DEFAULT_PRIMARY)!).palette
}

// WCAG 2 contrast ratio of two #rrggbb colours.
function luminance(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const c = parseInt(hex.slice(i, i + 2), 16) / 255
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  }) as [number, number, number]
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

export function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number]
  return (hi + 0.05) / (lo + 0.05)
}

// Normal text needs 4.5:1 (WCAG AA).
export const MIN_CONTRAST = 4.5

/** What the page is drawn with: Nuxt UI's --ui-bg equals --ui-text-inverted
 * (white, or the background scale's 900 in the dark), so one colour is both
 * the page behind accent text and the label on an accent button. */
export function pageColor(surface: string, dark: boolean): string {
  return dark ? surfacePalette(surface)[900] : '#ffffff'
}

/** The shade used as the accent (--ui-primary): Nuxt UI's own 500/400 where
 * that reads, else the nearest darker (light) or lighter (dark) one that keeps
 * accent text and button labels at 4.5:1. Noir is near-black or near-white. */
export function primaryShade(name: string, surface: string, dark: boolean): Shade {
  if (name === 'noir') return dark ? 50 : 950
  const palette = primaryPalette(name, surface)
  const page = pageColor(surface, dark)
  const order: Shade[] = dark ? [400, 300, 200, 100] : [500, 600, 700, 800]
  return order.find((s) => contrast(palette[s], page) >= MIN_CONTRAST) ?? order[order.length - 1]!
}
