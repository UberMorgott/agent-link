import { computed, reactive, ref, watch } from 'vue'
import { DEFAULT_PRIMARY, DEFAULT_SURFACE, SHADES, primaryColors, primaryPalette, primaryShade, surfacePalette, surfaces } from '@/lib/palettes'

export type ThemeMode = 'light' | 'dark' | 'system'

// The UI fonts (assets/fonts.css bundles the first three; «system» is the
// computer's own). stack is the font-family the page uses.
export const FONTS = [
  { name: 'inter', stack: "'Inter Variable', 'Segoe UI', system-ui, sans-serif" },
  { name: 'manrope', stack: "'Manrope Variable', 'Segoe UI', system-ui, sans-serif" },
  { name: 'plex', stack: "'IBM Plex Sans Variable', 'Segoe UI', system-ui, sans-serif" },
  { name: 'system', stack: "system-ui, 'Segoe UI', Roboto, sans-serif" },
] as const
export type FontName = (typeof FONTS)[number]['name']
export const DEFAULT_FONT: FontName = 'inter'

export interface UiState {
  theme: ThemeMode
  primary: string
  surface: string
  font: FontName
}

export const UI_STORAGE_KEY = 'agentlink' + '.ui'

const defaults: UiState = { theme: 'system', primary: DEFAULT_PRIMARY, surface: DEFAULT_SURFACE, font: DEFAULT_FONT }
const systemDark = ref(false)
// storageFailed is set while the browser refuses to keep the choice.
export const storageFailed = ref(false)
let systemQuery: MediaQueryList | undefined

// readUiState reads the saved appearance; anything unknown falls back to the defaults.
export function readUiState(storage: Pick<Storage, 'getItem'>): UiState {
  try {
    const value = storage.getItem(UI_STORAGE_KEY)
    const stored: unknown = value ? JSON.parse(value) : null
    if (!stored || typeof stored !== 'object' || Array.isArray(stored)) return { ...defaults }
    const { theme, primary, surface, font } = stored as Record<string, unknown>
    return {
      theme: theme === 'light' || theme === 'dark' ? theme : 'system',
      primary: primaryColors.some((c) => c.name === primary) ? (primary as string) : defaults.primary,
      surface: surfaces.some((s) => s.name === surface) ? (surface as string) : defaults.surface,
      font: FONTS.some((f) => f.name === font) ? (font as FontName) : defaults.font,
    }
  } catch {
    return { ...defaults }
  }
}

export const layoutConfig = reactive<UiState>({ ...defaults })
export const layoutState = reactive({ mobileMenuActive: false })

const isDarkTheme = computed(() => layoutConfig.theme === 'dark' || (layoutConfig.theme === 'system' && systemDark.value))

// Nuxt UI's theme and Tailwind's dark: variant key on the "dark" class of
// <html>; color-scheme gives native controls and scrollbars the same mode.
// The chosen scales go over Nuxt UI's colour variables as inline properties of
// <html> (CSSOM, so the style-src nonce does not apply), and --ui-primary
// names the shade that keeps accent text readable (lib/palettes.ts).
function applyTheme() {
  const root = document.documentElement
  const dark = isDarkTheme.value
  root.classList.toggle('dark', dark)
  root.style.colorScheme = dark ? 'dark' : 'light'
  const surface = surfacePalette(layoutConfig.surface)
  const primary = primaryPalette(layoutConfig.primary, layoutConfig.surface)
  for (const shade of SHADES) {
    root.style.setProperty(`--ui-color-neutral-${shade}`, surface[shade])
    root.style.setProperty(`--ui-color-primary-${shade}`, primary[shade])
  }
  root.style.setProperty('--ui-primary', `var(--ui-color-primary-${primaryShade(layoutConfig.primary, layoutConfig.surface, dark)})`)
  root.style.setProperty('--app-font', (FONTS.find((f) => f.name === layoutConfig.font) || FONTS[0]).stack)
}

watch(layoutConfig, (value) => {
  applyTheme()
  try {
    localStorage.setItem(UI_STORAGE_KEY, JSON.stringify(value))
    storageFailed.value = false
  } catch {
    storageFailed.value = true
  }
}, { flush: 'sync' })

function savedUiState(): UiState {
  try {
    return readUiState(localStorage)
  } catch {
    storageFailed.value = true
    return { ...defaults }
  }
}

// applyUiState reads the saved appearance and follows the system while the
// theme is "system". main.ts calls it before the app mounts, so the first
// paint already has the chosen colours.
export function applyUiState() {
  Object.assign(layoutConfig, savedUiState())
  watchNarrow()
  if (!systemQuery && typeof window.matchMedia === 'function') {
    systemQuery = window.matchMedia('(prefers-color-scheme: dark)')
    systemDark.value = systemQuery.matches
    systemQuery.addEventListener('change', () => {
      systemDark.value = systemQuery!.matches
      applyTheme()
    })
  }
  applyTheme()
}

// NARROW_QUERY matches the phone layout: below Tailwind's md breakpoint the
// sidebar becomes a drawer and the inbox shows either the list or one chat.
export const NARROW_QUERY = '(max-width: 767px)'
export const isNarrow = ref(false)
let narrowQuery: MediaQueryList | undefined

export function watchNarrow() {
  if (narrowQuery || typeof window.matchMedia !== 'function') return
  narrowQuery = window.matchMedia(NARROW_QUERY)
  isNarrow.value = narrowQuery.matches
  narrowQuery.addEventListener('change', () => { isNarrow.value = narrowQuery!.matches })
}

export function useLayout() {
  const toggleMenu = () => { layoutState.mobileMenuActive = !layoutState.mobileMenuActive }
  const hideMobileMenu = () => { layoutState.mobileMenuActive = false }
  return { layoutConfig, layoutState, isDarkTheme, storageFailed, toggleMenu, hideMobileMenu }
}
