<script setup lang="ts">
import { computed, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import UButton from '@nuxt/ui/components/Button.vue'
import ChatRow from '@/components/ChatRow.vue'
import { icon } from '@/lib/icons'
import { navigate } from '@/lib/nav'
import { fmt, t } from '@/lib/runtime'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'

// A project's own page. A project has one chat: the page goes on to it, and
// without one yet it offers to start it. The network from before projects
// lists its chats (one per member).
const projects = useProjectsStore()
const inbox = useInboxStore()
const route = useRoute()
const router = useRouter()

const pid = computed(() => String(route.params.project || ''))
const view = computed(() => projects.byID(pid.value))
const name = computed(() => view.value?.display || t("projects.connecting"))
const legacy = computed(() => !!view.value?.legacy)
const chats = computed(() => projects.chats[pid.value] || [])
const active = computed(() => (legacy.value ? null : inbox.activeChat(pid.value)))

// What keeps the project from working fully, in one line.
const state = computed(() => {
  const p = view.value
  if (!p) return ''
  if (p.problem) return t("project.problem." + p.problem)
  if (p.state === 'connecting') return t("project.state.connecting")
  if (p.state === 'needs_folder') return t("project.state.needs_folder")
  return ''
})

watch(pid, (id) => {
  projects.open(id)
  void inbox.selectChat(id, '')
  const peer = route.query.peer
  if (typeof peer === 'string' && peer) void inbox.openPeer(id, peer)
}, { immediate: true })

// The project's one chat, once known, replaces this page.
watch(active, (c) => {
  if (c && route.name === 'project') void router.replace({ name: 'chat', params: { project: pid.value, chat: c.id } })
}, { immediate: true })

// A project that is not (or no longer) here sends the page on.
watch(() => [projects.list, pid.value] as const, ([list, id]) => {
  if (list && !list.some((p) => p.id === id)) navigate('inbox')
}, { immediate: true })
</script>

<template>
  <section
    data-view="project"
    class="h-full overflow-y-auto"
  >
    <div
      v-if="legacy && chats.length"
      class="chat-column flex flex-col gap-4 py-6"
    >
      <h1
        id="project_title"
        class="truncate"
      >
        {{ name }}
      </h1>
      <ul
        id="project_chats"
        class="flex flex-col gap-0.5"
      >
        <li
          v-for="c in chats"
          :key="c.id"
        >
          <ChatRow
            :project="pid"
            :chat="c"
          />
        </li>
      </ul>
    </div>
    <div
      v-else-if="!active"
      id="project_empty"
      class="flex h-full flex-col items-center justify-center gap-4 px-4 text-center"
    >
      <h1 class="text-xl">
        {{ view?.state === 'connecting' ? name : fmt("project.empty", { name }) }}
      </h1>
      <p
        v-if="state"
        id="project_state"
        class="hint max-w-md"
        :class="{ warn: view?.state === 'error' }"
      >
        {{ state }}
        <UButton
          v-if="view?.state === 'needs_folder'"
          class="project-folder-link"
          :label="t('project.menu.folder')"
          variant="link"
          size="sm"
          @click="projects.openDialog('folder', pid)"
        />
      </p>
      <div class="flex flex-wrap justify-center gap-2">
        <UButton
          v-if="!legacy"
          id="project_start_chat"
          :label="t('project.start_chat')"
          :icon="icon('plus')"
          :loading="inbox.starting"
          @click="inbox.startChat(pid)"
        />
        <UButton
          id="project_invite"
          :label="t('project.invite')"
          :icon="icon('invite')"
          color="neutral"
          variant="outline"
          @click="projects.openDialog('invite', pid)"
        />
      </div>
      <p
        v-if="inbox.startResult"
        id="project_start_result"
        class="text-sm text-error"
        role="status"
      >
        {{ inbox.startResult }}
      </p>
    </div>
  </section>
</template>
