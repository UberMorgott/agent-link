import { fileURLToPath, URL } from 'node:url'

import ui from '@nuxt/ui/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

// The Go app serves dist/ under /ui/ (internal/app/web.go): index.html for the
// application routes with the per-run token filled in, open.html for the
// public launcher, assets/ as they are.
export default defineConfig({
  base: '/ui/',
  plugins: [
    vue(),
    // Nuxt UI brings Tailwind CSS with it. The page runs offline: every icon
    // it shows is bundled from @iconify-json/lucide (the default theme icons
    // plus the ones the scan finds in src/), so the Iconify API is never asked.
    // Colour mode is the app's own (src/layout/composables/layout.ts): the
    // built-in one injects a <style> without the CSP nonce on every switch.
    ui({
      ui: { colors: { primary: 'emerald', neutral: 'neutral' } },
      colorMode: false,
      dts: false,
      // Only the components the app uses get their theme classes into the CSS.
      experimental: { componentDetection: true },
      icon: {
        clientBundle: {
          scan: { globInclude: ['src/**/*.{vue,ts}'] },
        },
      },
    }),
  ],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // One app chunk on purpose: the binary embeds it and serves it locally.
    chunkSizeWarningLimit: 700,
    rolldownOptions: {
      input: {
        index: fileURLToPath(new URL('./index.html', import.meta.url)),
        open: fileURLToPath(new URL('./open.html', import.meta.url)),
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['src/test/setup.ts'],
  },
})
