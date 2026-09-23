// Single place for theming: PrimeVue Aura with an emerald primary and a calm
// zinc surface. Change the look here, not in components.
import { definePreset } from '@primeuix/themes'
import Aura from '@primeuix/themes/aura'

const emerald = {
  50: '#ecfdf5', 100: '#d1fae5', 200: '#a7f3d0', 300: '#6ee7b7', 400: '#34d399', 500: '#10b981',
  600: '#059669', 700: '#047857', 800: '#065f46', 900: '#064e3b', 950: '#022c22',
}

const zinc = {
  0: '#ffffff', 50: '#fafafa', 100: '#f4f4f5', 200: '#e4e4e7', 300: '#d4d4d8', 400: '#a1a1aa', 500: '#71717a',
  600: '#52525b', 700: '#3f3f46', 800: '#27272a', 900: '#18181b', 950: '#09090b',
}

export const preset = definePreset(Aura, {
  semantic: {
    primary: emerald,
    colorScheme: {
      light: {
        surface: zinc,
        primary: { color: '{primary.600}', contrastColor: '#ffffff', hoverColor: '{primary.700}', activeColor: '{primary.800}' },
        highlight: { background: '{primary.50}', focusBackground: '{primary.100}', color: '{primary.700}', focusColor: '{primary.800}' },
      },
      dark: {
        surface: zinc,
        primary: { color: '{primary.400}', contrastColor: '{surface.950}', hoverColor: '{primary.300}', activeColor: '{primary.200}' },
        highlight: {
          background: 'color-mix(in srgb, {primary.400}, transparent 84%)',
          focusBackground: 'color-mix(in srgb, {primary.400}, transparent 76%)',
          color: 'rgba(255,255,255,.87)',
          focusColor: 'rgba(255,255,255,.87)',
        },
      },
    },
  },
})
