import { describe, expect, it } from 'vitest'
import { ASPECTS, aspectContrast, cleanColors, lowContrast, mix } from './aspects'

const aspect = (name: string) => ASPECTS.find((a) => a.name === name)!

describe('the palette aspects', () => {
  it('names every aspect once, each with its own token', () => {
    const names = ASPECTS.map((a) => a.name)
    expect(names).toEqual(['accent', 'mine', 'other', 'agent', 'notice', 'link', 'online', 'working', 'waiting', 'paused', 'error', 'unread', 'focus'])
    expect(new Set(ASPECTS.map((a) => a.token)).size).toBe(names.length)
  })

  it('keeps known aspects with #rrggbb colours only', () => {
    expect(cleanColors({ link: '#ABCDEF', mine: 'blue', nope: '#000000', unread: 7 })).toEqual({ link: '#abcdef' })
    expect(cleanColors(null)).toEqual({})
    expect(cleanColors(['#000000'])).toEqual({})
  })

  it('mixes like color-mix in srgb', () => {
    expect(mix('#ff0000', '#ffffff', 0.5)).toBe('#ff8080')
    expect(mix('#000000', '#ffffff', 0)).toBe('#ffffff')
  })

  it('warns when text would read below 4.5:1', () => {
    // Pale yellow text on the white page: unreadable; dark blue: fine.
    expect(lowContrast(aspect('link'), '#ffee88', 'neutral', false)).toBe(true)
    expect(lowContrast(aspect('link'), '#1d4ed8', 'neutral', false)).toBe(false)
    // The same yellow reads on the dark page.
    expect(lowContrast(aspect('link'), '#ffee88', 'neutral', true)).toBe(false)
    // A bubble tint stays light, so the body text on it reads; black does not.
    expect(lowContrast(aspect('mine'), '#3b82f6', 'neutral', false)).toBe(false)
    expect(lowContrast(aspect('mine'), '#000000', 'neutral', true)).toBe(false)
    expect(aspectContrast(aspect('focus'), '#000000', 'neutral', false)).toBeLessThan(aspectContrast(aspect('focus'), '#ffffff', 'neutral', false))
  })
})
