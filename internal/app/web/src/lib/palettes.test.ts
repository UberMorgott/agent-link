import { describe, expect, it } from 'vitest'
import { MIN_CONTRAST, SHADES, contrast, pageColor, primaryColors, primaryPalette, primaryShade, surfacePalette, surfaces } from './palettes'

describe('the appearance palettes', () => {
  it('offers full 50–950 scales', () => {
    for (const p of [...primaryColors, ...surfaces]) {
      expect(Object.keys(p.palette).map(Number), p.name).toEqual([...SHADES])
      for (const shade of SHADES) expect(p.palette[shade], p.name + shade).toMatch(/^#[0-9a-f]{6}$/)
    }
  })

  it('measures contrast like WCAG', () => {
    expect(contrast('#000000', '#ffffff')).toBeCloseTo(21, 5)
    expect(contrast('#777777', '#777777')).toBeCloseTo(1, 5)
  })

  // Every accent on every background, in both modes: accent text on the page
  // and the label of an accent button (the same colour pair in Nuxt UI).
  for (const dark of [false, true]) {
    for (const s of surfaces) {
      it(`keeps text readable on ${s.name} (${dark ? 'dark' : 'light'})`, () => {
        const page = pageColor(s.name, dark)
        const bg = surfacePalette(s.name)
        // --ui-text and --ui-text-muted on --ui-bg.
        expect(contrast(dark ? bg[200] : bg[700], page)).toBeGreaterThanOrEqual(MIN_CONTRAST)
        expect(contrast(dark ? bg[400] : bg[500], page)).toBeGreaterThanOrEqual(MIN_CONTRAST)
        for (const c of primaryColors) {
          const accent = primaryPalette(c.name, s.name)[primaryShade(c.name, s.name, dark)]
          expect(contrast(accent, page), c.name).toBeGreaterThanOrEqual(MIN_CONTRAST)
        }
      })
    }
  }

  it('keeps Nuxt UI shades where they already read, noir at the ends', () => {
    expect(primaryShade('indigo', 'slate', true)).toBe(400)
    expect(primaryShade('blue', 'slate', false)).toBe(600)
    expect(primaryShade('noir', 'zinc', false)).toBe(950)
    expect(primaryShade('noir', 'zinc', true)).toBe(50)
    expect(primaryPalette('noir', 'stone')).toBe(surfacePalette('stone'))
    expect(primaryPalette('unknown', 'unknown')).toBe(primaryPalette('emerald', 'neutral'))
  })
})
