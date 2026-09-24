import { createRouter, createWebHistory, type Router, type RouterHistory } from 'vue-router'
import { t } from '@/lib/runtime'
import { useProjectsStore } from '@/stores/projects'
import ChatView from '@/views/ChatView.vue'
import DashboardView from '@/views/DashboardView.vue'
import ParticipantsView from '@/views/ParticipantsView.vue'
import ProjectView from '@/views/ProjectView.vue'
import SettingsView from '@/views/SettingsView.vue'
import WelcomeView from '@/views/WelcomeView.vue'

// landing is where /ui/inbox goes: the last used project, else the first one,
// else the welcome screen (docs/plans/projects-v1.md D10).
async function landing() {
  const projects = useProjectsStore()
  if (!projects.list) {
    await projects.refreshList().catch(() => {})
    await projects.listSettled()
  }
  const pid = projects.landing()
  return pid ? { name: 'project', params: { project: pid } } : { name: 'welcome' }
}

// The app serves the shell at /ui/dashboard, /ui/inbox, /ui/welcome,
// /ui/p/{pid}, /ui/p/{pid}/c/{chat}, /ui/participants and /ui/settings
// (internal/app/web.go); a reload of any of them lands here again.
export function createAppRouter(history: RouterHistory = createWebHistory('/ui/')): Router {
  const router = createRouter({
    history,
    routes: [
      { path: '/dashboard', name: 'dashboard', component: DashboardView },
      { path: '/inbox', name: 'inbox', component: WelcomeView, beforeEnter: landing },
      { path: '/welcome', name: 'welcome', component: WelcomeView },
      { path: '/p/:project', name: 'project', component: ProjectView },
      { path: '/p/:project/c/:chat', name: 'chat', component: ChatView },
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
