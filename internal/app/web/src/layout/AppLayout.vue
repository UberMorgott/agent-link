<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import UButton from '@nuxt/ui/components/Button.vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import ConversationList from '@/components/ConversationList.vue'
import MessageToasts from '@/components/MessageToasts.vue'
import { icon } from '@/lib/icons'
import { browser, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { isNarrow, useLayout } from './composables/layout'

const app = useAppStore()
const route = useRoute()
const { layoutConfig, layoutState, cycleTheme, toggleMenu, hideMobileMenu } = useLayout()
const view = ref<HTMLElement | null>(null)
// The app icon from public/, served next to the page.
const logo = `${import.meta.env.BASE_URL}icon.svg`

const links = [
  { route: 'dashboard', icon: 'dashboard', label: "nav.dashboard" },
  { route: 'inbox', icon: 'inbox', label: "nav.inbox" },
  { route: 'participants', icon: 'participants', label: "nav.participants" },
  { route: 'settings', icon: 'settings', label: "nav.settings" },
]
const current = computed(() => String(route.name || ''))
const themeIcon = computed(() => icon(({ system: 'system', light: 'sun', dark: 'moon' })[layoutConfig.theme]))
const themeLabel = computed(() => t("theme." + layoutConfig.theme))

// A new route takes the focus to its view, as a page load would.
watch(current, () => {
  hideMobileMenu()
  void nextTick(() => view.value?.focus({ preventScroll: true }))
})
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
      aria-labelledby="nav_label"
      class="fixed inset-y-0 left-0 z-40 flex w-64 flex-col border-r border-default bg-[var(--app-side)] transition-transform md:static md:translate-x-0"
      :class="layoutState.mobileMenuActive ? 'translate-x-0' : '-translate-x-full'"
    >
      <div class="flex items-center gap-2 px-4 pt-4 pb-3">
        <img
          :src="logo"
          alt=""
          class="h-6 w-6"
        >
        <strong class="text-[0.95rem] tracking-tight text-highlighted">agentlink</strong>
      </div>
      <span
        id="nav_label"
        class="sr-only"
      >{{ t("nav.label") }}</span>
      <nav
        aria-labelledby="nav_label"
        class="flex flex-col gap-0.5 px-2"
      >
        <RouterLink
          v-for="link in links"
          :key="link.route"
          :to="'/' + link.route"
          :data-route="link.route"
          :title="t(link.label)"
          class="nav-link"
          :class="{ active: current === link.route }"
        >
          <UIcon
            :name="icon(link.icon)"
            class="nav-icon size-[1.1rem] flex-none"
          /><span class="nav-text">{{ t(link.label) }}</span>
        </RouterLink>
      </nav>
      <ConversationList
        v-if="current === 'inbox' && !isNarrow"
        class="mt-4 min-h-0 flex-1"
      />
      <div
        v-else
        class="flex-1"
      />
      <div class="flex items-center justify-between gap-2 px-3 py-3">
        <UButton
          id="theme_toggle"
          :icon="themeIcon"
          color="neutral"
          variant="ghost"
          size="sm"
          :aria-label="themeLabel"
          :title="themeLabel"
          @click="cycleTheme"
        />
      </div>
    </aside>
    <section class="workspace flex min-w-0 flex-1 flex-col">
      <header
        id="topbar"
        class="flex h-12 flex-none items-center gap-2 px-4"
      >
        <UButton
          :icon="icon('menu')"
          color="neutral"
          variant="ghost"
          size="sm"
          class="md:hidden"
          :aria-label="t('nav.menu')"
          @click="toggleMenu"
        />
        <span
          id="status"
          class="ml-auto truncate text-xs"
          :class="app.link.cls"
          :hidden="current === 'inbox'"
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
      <MessageToasts />
    </div>
  </div>
</template>
