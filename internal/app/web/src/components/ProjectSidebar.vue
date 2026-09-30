<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import UIcon from '@nuxt/ui/components/Icon.vue'
import LocalChatGroup, { type LocalChatRow } from '@/components/LocalChatGroup.vue'
import ProjectMenu from '@/components/ProjectMenu.vue'
import UserChip from '@/components/UserChip.vue'
import VersionBadge from '@/components/VersionBadge.vue'
import { projectDot, projectTitle } from '@/lib/chat'
import { icon } from '@/lib/icons'
import { chatLabel, groupLocalChats, liveState, useGraceClock } from '@/lib/localChats'
import { openProject } from '@/lib/nav'
import { t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'
import type { ProjectView } from '@/types'

// The sidebar separates chats shared with other computers from local
// Claude/Codex chats. Each project still owns one chat.
const app = useAppStore()
const projects = useProjectsStore()
const inbox = useInboxStore()
const route = useRoute()

const current = computed(() => String(route.name || ''))
// The app icon from public/, served next to the page.
const logo = `${import.meta.env.BASE_URL}icon.svg`

// onScreen: the project (or its chat) is the page shown.
const onScreen = (pid: string) => projects.current === pid && (current.value === 'project' || current.value === 'chat')

function row(p: ProjectView) {
  const dot = projectDot(p, app.self)
  return {
    p,
    name: projectTitle(p),
    dot: dot.cls,
    dotLabel: dot.label,
    unread: inbox.unreadCount(p.id),
    active: onScreen(p.id),
  }
}
const tree = computed(() => (projects.list || []).filter((p) => p.scope !== 'local').map(row))

// Local chats by project, each only while its agents are at work and for a
// grace after (lib/localChats); the clock moves on when a grace ends.
const now = useGraceClock(() => projects.list || [])
function localRow(p: ProjectView, group: string): LocalChatRow {
  const lc = p.local_chat
  // A chat alone in its project's group keeps its project's name beside its
  // own; in an open group the project's own chat is named as such.
  const name = !lc ? (group ? projectTitle(p) : chatLabel(p)) : group ? group + ' · ' + chatLabel(p) : chatLabel(p)
  return { p, name, unread: inbox.unreadCount(p.id), active: onScreen(p.id), live: liveState(p) }
}
// A chat with unread messages or on screen stays until read or left.
const keepLocal = (p: ProjectView) => inbox.unreadCount(p.id) > 0 || onScreen(p.id)
const localTree = computed(() => groupLocalChats(projects.list || [], now.value, keepLocal).map((g) => ({
  ...g,
  rows: g.items.map((p) => localRow(p, g.items.length === 1 ? g.name : '')),
})))

// A project's row opens its one chat (the network from before projects: its page).
function openRow(pid: string, legacy: boolean) {
  if (legacy) { void openProject(pid); return }
  inbox.openActive(pid)
}
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
      <VersionBadge />
    </div>
    <div class="flex flex-col gap-0.5 px-2 pb-2">
      <button
        id="new_project"
        type="button"
        class="nav-link w-full"
        @click="projects.openDialog('create', '')"
      >
        <UIcon
          :name="icon('plus')"
          class="nav-icon size-[1.1rem] flex-none"
        /><span class="nav-text">{{ t("projects.new") }}</span>
      </button>
      <button
        id="join_project"
        type="button"
        class="nav-link w-full"
        @click="projects.openDialog('join', '')"
      >
        <UIcon
          :name="icon('join')"
          class="nav-icon size-[1.1rem] flex-none"
        /><span class="nav-text">{{ t("projects.join") }}</span>
      </button>
    </div>
    <div class="min-h-0 flex-1 overflow-y-auto">
      <section aria-labelledby="projects_label">
        <h2
          id="projects_label"
          class="px-4 pt-2 pb-1 text-xs font-medium text-muted"
        >
          {{ t("projects.network_chats") }}
        </h2>
        <ul
          id="project_tree"
          class="px-2 pb-2"
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
                class="project-open"
                :aria-current="item.active ? 'page' : undefined"
                @click="openRow(item.p.id, item.p.legacy)"
              >
                <span
                  class="project-dot"
                  :class="item.dot"
                  :data-dot="item.dot"
                  :title="item.dotLabel"
                  role="img"
                  :aria-label="item.dotLabel.replace(/\n/g, '; ')"
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
          </li>
        </ul>
      </section>
      <section aria-labelledby="local_chats_label">
        <h2
          id="local_chats_label"
          class="px-4 pt-3 pb-1 text-xs font-medium text-muted"
        >
          <RouterLink
            to="/agents"
            data-route="agents"
            class="inline-flex items-center gap-1 hover:underline"
            :class="{ active: current === 'agents' }"
            :aria-current="current === 'agents' ? 'page' : undefined"
          >
            <UIcon
              :name="icon('agent')"
              class="size-3.5"
            />{{ t('projects.local_chats') }}
          </RouterLink>
        </h2>
        <ul
          id="local_chat_tree"
          class="px-2 pb-2"
        >
          <LocalChatGroup
            v-for="g in localTree"
            :key="'group:' + g.key"
            :group-key="g.key"
            :name="g.name"
            :rows="g.rows"
            :expanded="!!projects.expanded[g.key]"
            @open="openRow($event, false)"
            @toggle="projects.setExpanded(g.key, $event)"
          />
        </ul>
      </section>
    </div>
    <span
      id="nav_label"
      class="sr-only"
    >{{ t("nav.label") }}</span>
    <nav
      id="user_panel"
      aria-labelledby="nav_label"
      class="flex items-center gap-1 border-t border-default px-2 py-2"
    >
      <UserChip />
      <RouterLink
        v-if="projects.hasLegacy"
        to="/participants"
        data-route="participants"
        class="panel-icon"
        :class="{ active: current === 'participants' }"
        :title="t('nav.participants')"
        :aria-label="t('nav.participants')"
      >
        <UIcon
          :name="icon('participants')"
          class="size-[1.1rem]"
        />
      </RouterLink>
      <RouterLink
        to="/settings"
        data-route="settings"
        class="panel-icon"
        :class="{ active: current === 'settings' }"
        :title="t('nav.settings')"
        :aria-label="t('nav.settings')"
      >
        <UIcon
          :name="icon('settings')"
          class="size-[1.1rem]"
        />
      </RouterLink>
    </nav>
  </div>
</template>
