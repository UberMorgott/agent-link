import { createRouter, createWebHistory, type Router, type RouterHistory } from 'vue-router'
import { t } from '@/lib/runtime'
import DashboardView from '@/views/DashboardView.vue'
import InboxView from '@/views/InboxView.vue'
import ParticipantsView from '@/views/ParticipantsView.vue'
import SettingsView from '@/views/SettingsView.vue'

// The app serves the shell at /ui/dashboard, /ui/inbox, /ui/participants and
// /ui/settings (internal/app/web.go); a reload of any of them lands here again.
export function createAppRouter(history: RouterHistory = createWebHistory('/ui/')): Router {
  const router = createRouter({
    history,
    routes: [
      { path: '/dashboard', name: 'dashboard', component: DashboardView },
      { path: '/inbox', name: 'inbox', component: InboxView },
      { path: '/participants', name: 'participants', component: ParticipantsView },
      { path: '/settings', name: 'settings', component: SettingsView },
      { path: '/:rest(.*)*', redirect: '/dashboard' },
    ],
  })
  router.afterEach((to) => {
    document.title = t("page.title." + String(to.name || 'dashboard'))
  })
  return router
}
