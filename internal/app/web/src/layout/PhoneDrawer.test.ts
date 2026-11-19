import { describe, expect, it } from 'vitest'
import { SITE } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useLayout } from './composables/layout'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)

describe('the phone drawer', () => {
  it('closes on Escape and gives the focus back to the menu button', async () => {
    fakeBackend()
    await mountApp('/p/' + SITE)
    await settle()
    const { layoutState } = useLayout()
    const toggle = $<HTMLButtonElement>('#menu_toggle')!
    expect(toggle.getAttribute('aria-controls')).toBe('sidebar')
    layoutState.mobileMenuActive = true
    await settle()
    expect(toggle.getAttribute('aria-expanded')).toBe('true')
    $('#sidebar')!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await settle()
    expect(layoutState.mobileMenuActive).toBe(false)
    expect(toggle.getAttribute('aria-expanded')).toBe('false')
    expect(document.activeElement).toBe(toggle)
  })
})
