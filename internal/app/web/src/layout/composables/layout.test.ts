import { describe, expect, it, vi } from 'vitest'
import { UI_STORAGE_KEY, applyUiState, readUiState, useLayout } from './layout'

const css = (name: string) => document.documentElement.style.getPropertyValue(name)
const none = { light: {}, dark: {} }
const stored = (value: string) => ({ getItem: (key: string) => (key === UI_STORAGE_KEY ? value : null) })

describe('the saved appearance', () => {
  it('reads known values and drops the rest', () => {
    expect(readUiState(stored('{"theme":"dark","primary":"rose","surface":"zinc","font":"plex"}'))).toEqual({ theme: 'dark', primary: 'rose', surface: 'zinc', font: 'plex', colors: none })
    expect(readUiState(stored('{"theme":"sepia","primary":"lime","surface":"beige","font":"comic"}'))).toEqual({ theme: 'system', primary: 'emerald', surface: 'neutral', font: 'inter', colors: none })
    // A store from before the palettes had only the theme; one from before the fonts no font.
    expect(readUiState(stored('{"theme":"light"}'))).toEqual({ theme: 'light', primary: 'emerald', surface: 'neutral', font: 'inter', colors: none })
    expect(readUiState(stored('not json'))).toEqual({ theme: 'system', primary: 'emerald', surface: 'neutral', font: 'inter', colors: none })
    expect(readUiState(stored('[1]'))).toEqual({ theme: 'system', primary: 'emerald', surface: 'neutral', font: 'inter', colors: none })
  })

  it('paints the saved scales on <html> and keeps changes', () => {
    localStorage.setItem(UI_STORAGE_KEY, JSON.stringify({ theme: 'light', primary: 'blue', surface: 'slate' }))
    applyUiState()
    const root = document.documentElement
    expect(root.classList.contains('dark')).toBe(false)
    expect(css('--ui-color-primary-500')).toBe('#3b82f6')
    expect(css('--ui-color-neutral-900')).toBe('#0f172a')
    expect(css('--ui-primary')).toBe('var(--ui-color-primary-600)')

    const { layoutConfig, storageFailed } = useLayout()
    layoutConfig.theme = 'dark'
    layoutConfig.primary = 'rose'
    layoutConfig.surface = 'zinc'
    expect(root.classList.contains('dark')).toBe(true)
    expect(css('--ui-color-primary-400')).toBe('#fb7185')
    expect(css('--ui-color-neutral-900')).toBe('#18181b')
    expect(css('--ui-primary')).toBe('var(--ui-color-primary-400)')
    expect(JSON.parse(localStorage.getItem(UI_STORAGE_KEY)!)).toEqual({ theme: 'dark', primary: 'rose', surface: 'zinc', font: 'inter', colors: none })
    expect(css('--app-font')).toContain('Inter Variable')
    layoutConfig.font = 'system'
    expect(css('--app-font')).toContain('system-ui')
    layoutConfig.font = 'inter'

    // Noir follows the background scale.
    layoutConfig.primary = 'noir'
    expect(css('--ui-color-primary-500')).toBe('#71717a')
    expect(css('--ui-primary')).toBe('var(--ui-color-primary-50)')
    expect(storageFailed.value).toBe(false)
  })

  it('keeps the palette picks per theme, valid ones only', () => {
    const saved = { theme: 'light', colors: { light: { link: '#AA0000', mine: 'red', bogus: '#000000' }, dark: { unread: '#123456' } } }
    expect(readUiState(stored(JSON.stringify(saved))).colors).toEqual({ light: { link: '#aa0000' }, dark: { unread: '#123456' } })
    expect(readUiState(stored('{"colors":[1]}')).colors).toEqual(none)
  })

  it('puts a pick into its token for the theme on screen only', () => {
    localStorage.setItem(UI_STORAGE_KEY, JSON.stringify({ theme: 'light', colors: { light: { link: '#aa0000', error: '#bb0000', accent: '#00aa00' }, dark: { link: '#ffcc00' } } }))
    applyUiState()
    expect(css('--app-link')).toBe('#aa0000')
    expect(css('--app-status-error')).toBe('#bb0000')
    expect(css('--ui-error')).toBe('#bb0000')
    expect(css('--ui-primary')).toBe('#00aa00')
    const { layoutConfig } = useLayout()
    layoutConfig.theme = 'dark'
    expect(css('--app-link')).toBe('#ffcc00')
    expect(css('--app-status-error')).toBe('')
    expect(css('--ui-error')).toBe('')
    expect(css('--ui-primary')).toMatch(/^var\(--ui-color-primary-/)
    delete layoutConfig.colors.dark.link
    expect(css('--app-link')).toBe('')
    expect(JSON.parse(localStorage.getItem(UI_STORAGE_KEY)!).colors.dark).toEqual({})
    layoutConfig.colors.light = {}
  })

  it('says so when the browser keeps nothing', () => {
    applyUiState()
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('quota') })
    const { layoutConfig, storageFailed } = useLayout()
    layoutConfig.primary = layoutConfig.primary === 'teal' ? 'sky' : 'teal'
    expect(storageFailed.value).toBe(true)
  })
})
