import { createApp } from 'vue'
import { createPinia } from 'pinia'
import { createHead } from '@unhead/vue/client'
import ui from '@nuxt/ui/vue-plugin'
import App from './App.vue'
import { createAppRouter } from './router'
import { applyUiState } from './layout/composables/layout'
import { claimDashboardWindow } from './lib/dashboardWindow'
import { setNavigator, type Params, type Query } from './lib/nav'
import { runtime } from './lib/runtime'
import { useAppStore } from './stores/app'

import './assets/styles.css'

const app = createApp(App)
const router = createAppRouter()
app.use(createPinia())
app.use(router)
// Nuxt UI writes its colour variables into a <style> through unhead. The app's
// Content-Security-Policy admits only styles carrying this page's nonce, so
// the head is created here, with the nonce on every tag it renders; Nuxt UI
// then reuses it instead of creating its own.
app.use(createHead({
  hooks: {
    'tags:resolve': ({ tags }) => {
      for (const tag of tags) if (tag.tag === 'style' || tag.tag === 'script') tag.props.nonce = runtime.nonce
    },
  },
}))
// Until the head's first render Nuxt UI also puts a stopgap copy of those
// variables into a <style> of its own; that one gets the nonce as it is made.
const createElement = document.createElement
document.createElement = function (this: Document, tag: string, options?: ElementCreationOptions) {
  const el = createElement.call(this, tag, options)
  if (el instanceof HTMLStyleElement) el.nonce = runtime.nonce
  return el
} as typeof document.createElement
try {
  app.use(ui)
} finally {
  document.createElement = createElement
}

setNavigator({
  go: (route: string, query?: Query, params?: Params) => router.push({ name: route, query: query || {}, params: params || {} }),
  current: () => String(router.currentRoute.value.name || ''),
})
applyUiState()
claimDashboardWindow((route) => void router.push({ name: route }))
void useAppStore().connectEvents()

app.mount('#app')
