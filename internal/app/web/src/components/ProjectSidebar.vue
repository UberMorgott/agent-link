<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import UIcon from '@nuxt/ui/components/Icon.vue'
import ProjectMenu from '@/components/ProjectMenu.vue'
import UserChip from '@/components/UserChip.vue'
import VersionBadge from '@/components/VersionBadge.vue'
import AppConfigurator from '@/layout/AppConfigurator.vue'
import { isUnread, projectDot } from '@/lib/chat'
import { icon } from '@/lib/icons'
import { openProject } from '@/lib/nav'
import { t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { chatKey, useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'

// The sidebar: one row per project (its one chat opens on a click), then the
// member's own chip with the appearance and the settings.
const app = useAppStore()
const projects = useProjectsStore()
const inbox = useInboxStore()
const route = useRoute()

const current = computed(() => String(route.name || ''))
// The app icon from public/, served next to the page.
const logo = `${import.meta.env.BASE_URL}icon.svg`

function unread(pid: string): number {
  return (projects.chats[pid] || []).filter((c) => isUnread(c, inbox.openKey() === chatKey(pid, c.id), inbox.readOf(pid, c.id))).length
}

const tree = computed(() => (projects.list || []).map((p) => {
  const dot = projectDot(p, app.self)
  return {
    p,
    name: p.display || t("projects.connecting"),
    dot: dot.cls,
    dotLabel: dot.label,
    unread: unread(p.id),
    active: projects.current === p.id && (current.value === 'project' || current.value === 'chat'),
  }
}))

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
      <AppConfigurator />
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
