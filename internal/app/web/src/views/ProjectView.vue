<script setup lang="ts">
import { computed, watch } from 'vue'
import { useRoute } from 'vue-router'
import ChatRow from '@/components/ChatRow.vue'
import { navigate } from '@/lib/nav'
import { t } from '@/lib/runtime'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'

// A project's own page: its state and its chats.
const projects = useProjectsStore()
const inbox = useInboxStore()
const route = useRoute()

const pid = computed(() => String(route.params.project || ''))
const view = computed(() => projects.byID(pid.value))
const name = computed(() => view.value?.display || t("projects.connecting"))
const chats = computed(() => projects.chats[pid.value] || [])

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
  if (typeof peer === 'string' && peer) inbox.openPeer(id, peer)
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
    <div class="chat-column flex flex-col gap-4 py-6">
      <header class="flex flex-col gap-1">
        <h1
          id="project_title"
          class="truncate"
        >
          {{ name }}
        </h1>
        <p
          v-if="state"
          id="project_state"
          class="hint"
          :class="{ warn: view?.state === 'error' }"
        >
          {{ state }}
        </p>
      </header>
      <ul
        v-if="chats.length"
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
  </section>
</template>
