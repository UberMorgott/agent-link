import { computed, reactive, ref, watch } from 'vue'

export type ThemeMode = 'light' | 'dark' | 'system'

export const UI_STORAGE_KEY = 'agentlink' + '.ui'

const systemDark = ref(false)
let systemQuery: MediaQueryList | undefined

function readTheme(): ThemeMode {
  try {
    const value = localStorage.getItem(UI_STORAGE_KEY)
    const stored: unknown = value ? JSON.parse(value) : null
    const theme = stored && typeof stored === 'object' ? (stored as { theme?: unknown }).theme : null
    return theme === 'light' || theme === 'dark' ? theme : 'system'
  } catch {
    return 'system'
  }
}

export const layoutConfig = reactive<{ theme: ThemeMode }>({ theme: 'system' })
export const layoutState = reactive({ mobileMenuActive: false })

const isDarkTheme = computed(() => layoutConfig.theme === 'dark' || (layoutConfig.theme === 'system' && systemDark.value))

function applyTheme() {
  document.documentElement.classList.toggle('app-dark', isDarkTheme.value)
}

watch(layoutConfig, (value) => {
  applyTheme()
  try { localStorage.setItem(UI_STORAGE_KEY, JSON.stringify(value)) } catch { /* storage unavailable */ }
}, { flush: 'sync' })

// applyUiState reads the saved theme and follows the system while it is "system".
export function applyUiState() {
  layoutConfig.theme = readTheme()
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

const THEME_ORDER: ThemeMode[] = ['system', 'light', 'dark']

export function useLayout() {
  // cycleTheme steps system → light → dark → system.
  const cycleTheme = () => {
    layoutConfig.theme = THEME_ORDER[(THEME_ORDER.indexOf(layoutConfig.theme) + 1) % THEME_ORDER.length]!
  }
  const toggleMenu = () => { layoutState.mobileMenuActive = !layoutState.mobileMenuActive }
  const hideMobileMenu = () => { layoutState.mobileMenuActive = false }
  return { layoutConfig, layoutState, isDarkTheme, cycleTheme, toggleMenu, hideMobileMenu }
}
