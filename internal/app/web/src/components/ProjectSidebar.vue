<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute } from 'vue-router'
import UButton from '@nuxt/ui/components/Button.vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import ChatRow from '@/components/ChatRow.vue'
import ProjectMenu from '@/components/ProjectMenu.vue'
import { useLayout } from '@/layout/composables/layout'
import { isUnread } from '@/lib/chat'
import { icon } from '@/lib/icons'
import { openProject } from '@/lib/nav'
import { fmt, t } from '@/lib/runtime'
import { chatKey, useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'
import type { ProjectView } from '@/types'

// The sidebar: the projects as a tree (each with its chats), then the app's
// own pages and the theme.
const projects = useProjectsStore()
const inbox = useInboxStore()
const route = useRoute()
const { layoutConfig, cycleTheme } = useLayout()

const current = computed(() => String(route.name || ''))
const themeIcon = computed(() => icon(({ system: 'system', light: 'sun', dark: 'moon' })[layoutConfig.theme]))
const themeLabel = computed(() => t("theme." + layoutConfig.theme))
// The app icon from public/, served next to the page.
const logo = `${import.meta.env.BASE_URL}icon.svg`

const links = computed(() => [
  { route: 'dashboard', icon: 'dashboard', label: "nav.dashboard" },
  // The participants page belongs to the legacy network only.
  ...(projects.hasLegacy ? [{ route: 'participants', icon: 'participants', label: "nav.participants" }] : []),
  { route: 'settings', icon: 'settings', label: "nav.settings" },
])

// Projects opened by hand besides the one on screen, which is always open.
const unfolded = ref<Record<string, boolean>>({})
function expanded(p: ProjectView) { return p.id === projects.current || !!unfolded.value[p.id] }
function fold(p: ProjectView) {
  unfolded.value = { ...unfolded.value, [p.id]: !expanded(p) }
}

function unread(pid: string): number {
  return (projects.chats[pid] || []).filter((c) => isUnread(c, inbox.openKey() === chatKey(pid, c.id), inbox.readOf(pid, c.id))).length
}

const tree = computed(() => (projects.list || []).map((p) => ({
  p,
  name: p.display || t("projects.connecting"),
  dot: p.state === 'error' ? 'off' : p.online > 0 ? 'on' : 'away',
  online: fmt("projects.online", { online: p.online, total: p.total }),
  unread: unread(p.id),
  open: expanded(p),
  active: projects.current === p.id && current.value === 'project',
  chats: projects.chats[p.id] || [],
  archive: projects.archiveOpen[p.id] ? projects.archives[p.id] || [] : null,
})))
</script>

<template>
  <div class="flex h-full flex-col">
    <div class="flex items-center gap-2 px-4 pt-4 pb-3">
      <img
        :src="logo"
        alt=""
        class="h-6 w-6"
      >
      <strong class="text-[0.95rem] tracking-tight text-highlighted">agentlink</strong>
    </div>
    <section
      class="flex min-h-0 flex-1 flex-col"
      aria-labelledby="projects_label"
    >
      <h2
        id="projects_label"
        class="px-4 pt-2 pb-1 text-xs font-medium text-muted"
      >
        {{ t("projects.label") }}
      </h2>
      <ul
        id="project_tree"
        class="min-h-0 flex-1 overflow-y-auto px-2 pb-2"
      >
        <li
          v-for="item in tree"
          :key="item.p.id"
          :data-project="item.p.id"
        >
          <div
            class="project-row"
            :class="{ active: item.active }"
          >
            <button
              type="button"
              class="project-fold"
              :aria-expanded="item.open ? 'true' : 'false'"
              :aria-label="fmt('projects.fold', { name: item.name })"
              @click="fold(item.p)"
            >
              <UIcon
                :name="icon(item.open ? 'expand' : 'fold')"
                class="size-3.5"
              />
            </button>
            <button
              type="button"
              class="project-open"
              :aria-current="item.active ? 'page' : undefined"
              @click="openProject(item.p.id)"
            >
              <span
                class="project-dot"
                :class="item.dot"
                :title="item.online"
              />
              <span class="project-name">{{ item.name }}</span>
              <span
                v-if="item.unread"
                class="project-unread"
                :title="t('inbox.unread')"
              >{{ item.unread }}</span>
            </button>
            <ProjectMenu
              :project="item.p"
              :name="item.name"
            />
          </div>
          <ul
            v-if="item.open"
            class="project-chats"
          >
            <li>
              <button
                type="button"
                class="project-action project-new-chat"
                @click="inbox.showNewChat(item.p.id)"
              >
                <UIcon
                  :name="icon('plus')"
                  class="size-3.5"
                />{{ t("inbox.new.title") }}
              </button>
            </li>
            <li
              v-for="c in item.chats"
              :key="c.id"
            >
              <ChatRow
                :project="item.p.id"
                :chat="c"
                compact
              />
            </li>
            <li>
              <button
                type="button"
                class="project-archive"
                :aria-pressed="item.archive ? 'true' : 'false'"
                @click="projects.toggleArchive(item.p.id)"
              >
                {{ t(item.archive ? "inbox.archive.hide" : "inbox.archive.show") }}
              </button>
            </li>
            <li
              v-for="c in item.archive || []"
              :key="'a' + c.id"
              class="archived"
            >
              <ChatRow
                :project="item.p.id"
                :chat="c"
                compact
              />
            </li>
          </ul>
        </li>
      </ul>
    </section>
    <span
      id="nav_label"
      class="sr-only"
    >{{ t("nav.label") }}</span>
    <nav
      aria-labelledby="nav_label"
      class="flex flex-col gap-0.5 border-t border-default px-2 pt-2"
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
  </div>
</template>
