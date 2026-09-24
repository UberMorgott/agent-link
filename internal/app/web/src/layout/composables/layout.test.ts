import { describe, expect, it, vi } from 'vitest'
import { UI_STORAGE_KEY, applyUiState, readUiState, useLayout } from './layout'

const css = (name: string) => document.documentElement.style.getPropertyValue(name)
const stored = (value: string) => ({ getItem: (key: string) => (key === UI_STORAGE_KEY ? value : null) })

describe('the saved appearance', () => {
  it('reads known values and drops the rest', () => {
    expect(readUiState(stored('{"theme":"dark","primary":"rose","surface":"zinc"}'))).toEqual({ theme: 'dark', primary: 'rose', surface: 'zinc' })
    expect(readUiState(stored('{"theme":"sepia","primary":"lime","surface":"beige"}'))).toEqual({ theme: 'system', primary: 'emerald', surface: 'neutral' })
    // A store from before the palettes had only the theme.
    expect(readUiState(stored('{"theme":"light"}'))).toEqual({ theme: 'light', primary: 'emerald', surface: 'neutral' })
    expect(readUiState(stored('not json'))).toEqual({ theme: 'system', primary: 'emerald', surface: 'neutral' })
    expect(readUiState(stored('[1]'))).toEqual({ theme: 'system', primary: 'emerald', surface: 'neutral' })
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
    expect(JSON.parse(localStorage.getItem(UI_STORAGE_KEY)!)).toEqual({ theme: 'dark', primary: 'rose', surface: 'zinc' })

    // Noir follows the background scale.
    layoutConfig.primary = 'noir'
    expect(css('--ui-color-primary-500')).toBe('#71717a')
    expect(css('--ui-primary')).toBe('var(--ui-color-primary-50)')
    expect(storageFailed.value).toBe(false)
  })

  it('says so when the browser keeps nothing', () => {
    applyUiState()
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('quota') })
    const { layoutConfig, storageFailed } = useLayout()
    layoutConfig.primary = layoutConfig.primary === 'teal' ? 'sky' : 'teal'
    expect(storageFailed.value).toBe(true)
  })
})
