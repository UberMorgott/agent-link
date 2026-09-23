import { createApp } from 'vue'
import { createPinia } from 'pinia'
import PrimeVue from 'primevue/config'
import App from './App.vue'
import { createAppRouter } from './router'
import { preset } from './theme/preset'
import { applyUiState } from './layout/composables/layout'
import { claimDashboardWindow } from './lib/dashboardWindow'
import { setNavigator, type Query } from './lib/nav'
import { runtime } from './lib/runtime'
import { useAppStore } from './stores/app'

import './assets/styles.css'

const app = createApp(App)
const router = createAppRouter()
app.use(createPinia())
app.use(router)
app.use(PrimeVue, {
  theme: {
    preset,
    options: {
      darkModeSelector: '.app-dark',
      cssLayer: { name: 'primevue', order: 'theme, base, primevue' },
    },
  },
  // The app's Content-Security-Policy admits only styles carrying this page's nonce.
  csp: { nonce: runtime.nonce },
})

setNavigator({
  go: (route: string, query?: Query) => router.push({ name: route, query: query || {} }),
  current: () => String(router.currentRoute.value.name || ''),
})
applyUiState()
claimDashboardWindow((route) => void router.push({ name: route }))
void useAppStore().connectEvents()

app.mount('#app')
