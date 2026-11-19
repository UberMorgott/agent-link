<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import UButton from '@nuxt/ui/components/Button.vue'
import JoinProjectModal from '@/components/JoinProjectModal.vue'
import NewProjectModal from '@/components/NewProjectModal.vue'
import ProjectDialogs from '@/components/ProjectDialogs.vue'
import ProjectSidebar from '@/components/ProjectSidebar.vue'
import { icon } from '@/lib/icons'
import { browser, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { useProjectsStore } from '@/stores/projects'
import { isNarrow, useLayout } from './composables/layout'

const app = useAppStore()
const route = useRoute()
const { layoutState, toggleMenu, hideMobileMenu } = useLayout()
const view = ref<HTMLElement | null>(null)
const sidebar = ref<HTMLElement | null>(null)

// The phone drawer: off screen it is out of the Tab order (inert); open, the
// page behind it is, the drawer takes the focus, and Escape closes it back
// onto the button that opened it (the menu button, or a chat's back button).
const drawerHidden = computed(() => isNarrow.value && !layoutState.mobileMenuActive)
const drawerOpen = computed(() => isNarrow.value && layoutState.mobileMenuActive)
let opener: HTMLElement | null = null
watch(() => layoutState.mobileMenuActive, (open) => {
  if (!open) return
  opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
  if (isNarrow.value) void nextTick(() => sidebar.value?.querySelector<HTMLElement>('button, a[href]')?.focus())
})
function closeDrawer() {
  if (!layoutState.mobileMenuActive) return
  hideMobileMenu()
  const back = opener?.isConnected && opener.offsetParent !== null ? opener : document.getElementById('menu_toggle')
  back?.focus()
}

const current = computed(() => String(route.name || ''))

// Any move closes the phone drawer; a new kind of page takes the focus to its
// view, as a page load would.
watch(() => route.fullPath, hideMobileMenu)
// A dialog opened from the phone drawer shows above the page, not under the drawer.
const projects = useProjectsStore()
watch(() => projects.dialog, (open) => { if (open) hideMobileMenu() })
watch(current, () => { void nextTick(() => view.value?.focus({ preventScroll: true })) })
</script>

<template>
  <div
    id="app-shell"
    class="flex h-full overflow-hidden"
  >
    <div
      v-if="layoutState.mobileMenuActive"
      class="fixed inset-0 z-30 bg-black/30 md:hidden"
      @click="hideMobileMenu"
    />
    <aside
      id="sidebar"
      ref="sidebar"
      aria-labelledby="nav_label"
      :inert="drawerHidden || undefined"
      class="fixed inset-y-0 left-0 z-40 flex w-72 flex-col border-r border-default bg-[var(--app-side)] transition-transform md:static md:z-auto md:w-64 md:translate-x-0"
      :class="layoutState.mobileMenuActive ? 'translate-x-0' : '-translate-x-full'"
      @keydown.esc="closeDrawer"
    >
      <ProjectSidebar />
    </aside>
    <section
      class="workspace flex min-w-0 flex-1 flex-col"
      :inert="drawerOpen || undefined"
    >
      <header
        id="topbar"
        class="flex h-12 flex-none items-center gap-2 px-4"
        :hidden="current === 'chat'"
      >
        <!-- A chat has its own header (its back button opens the drawer on a
             phone) and shows no connection line: no empty bar above it. -->
        <UButton
          id="menu_toggle"
          :icon="icon('menu')"
          color="neutral"
          variant="ghost"
          size="sm"
          class="md:hidden"
          :aria-label="t('nav.menu')"
          aria-controls="sidebar"
          :aria-expanded="layoutState.mobileMenuActive ? 'true' : 'false'"
          @click="toggleMenu"
        />
        <span
          id="status"
          class="status-pill ml-auto"
          :class="app.link.cls"
          :hidden="current === 'chat' || current === 'project'"
        >{{ app.link.text }}</span>
      </header>
      <main
        id="view"
        ref="view"
        tabindex="-1"
        :data-route="current"
        class="min-h-0 flex-1 overflow-hidden outline-none"
      >
        <RouterView />
      </main>
    </section>
    <div
      id="toast-region"
      role="status"
      aria-live="polite"
      class="pointer-events-none fixed right-4 bottom-4 z-50 flex w-80 max-w-[calc(100vw-2rem)] flex-col gap-2"
    >
      <div
        v-if="app.banner"
        id="connection-banner"
        class="pointer-events-auto flex flex-wrap items-center gap-2 rounded-lg bg-default px-4 py-3 text-sm shadow-lg ring ring-default"
      >
        <span class="flex-1">{{ app.banner }}</span>
        <UButton
          v-if="app.reloadRequired"
          :label="t('connection.reload')"
          size="sm"
          variant="soft"
          @click="browser.reload()"
        />
      </div>
    </div>
    <ProjectDialogs />
    <NewProjectModal />
    <JoinProjectModal />
  </div>
</template>
