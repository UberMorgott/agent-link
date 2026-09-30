<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import { when } from '@/lib/chat'
import { icon } from '@/lib/icons'
import { chatLabel, groupLocalChats, liveState, useGraceClock } from '@/lib/localChats'
import { t } from '@/lib/runtime'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'

// «Мои нейросети»: the local Claude Code <-> Codex chats, a read-only viewer.
// The agents talk among themselves; a person opens a chat to read it and
// changes nothing here. A chat shows while it is in use (lib/localChats), as
// in the sidebar. Network project chats never appear here.
const projects = useProjectsStore()
const inbox = useInboxStore()
const pending = ref(false)
const loadError = ref('')
let request = 0

const projectIDs = computed(() => (projects.list || []).filter((p) => p.scope === 'local').map((p) => p.id))
watch(projectIDs, async (ids) => {
  const current = ++request
  const missing = ids.filter((id) => !Object.hasOwn(projects.seats, id))
  pending.value = missing.length > 0
  loadError.value = ''
  const results = await Promise.allSettled(missing.map((id) => projects.refreshSeats(id)))
  if (request !== current) return
  loadError.value = results.find((result) => result.status === 'rejected')?.reason?.message || ''
  pending.value = false
}, { immediate: true })

onMounted(() => {
  if (!projects.list) void projects.refreshList().catch((error: Error) => { loadError.value = error.message })
})

const now = useGraceClock(() => projects.list || [])
const groups = computed(() => groupLocalChats(projects.list || [], now.value, (p) => inbox.unreadCount(p.id) > 0)
  .map((g) => ({
    key: g.key,
    name: g.name,
    rows: g.items.map((p) => ({
      p,
      label: !p.local_chat ? t('local_chat.project_chat') : chatLabel(p),
      live: liveState(p),
      last: (p.local_chat || p.activity)?.last_active || '',
      unread: inbox.unreadCount(p.id),
      seats: projects.seats[p.id] || [],
    })),
  })))

function open(pid: string) {
  inbox.openActive(pid)
}
</script>

<template>
  <section
    data-view="agents"
    class="h-full overflow-y-auto px-4 py-6"
  >
    <div class="chat-column flex flex-col gap-4">
      <div class="flex flex-col gap-1">
        <h1>{{ t('nav.agents') }}</h1>
        <p class="hint">
          {{ t('agents.hint') }}
        </p>
      </div>
      <p
        v-if="loadError"
        role="alert"
        class="text-sm text-error"
      >
        {{ loadError }}
      </p>
      <p
        v-if="!groups.length && !pending && projects.list"
        id="agents_empty"
        class="hint"
      >
        {{ t('agents.empty') }}
      </p>
      <div
        v-if="groups.length"
        id="agents_projects"
        class="flex flex-col gap-4"
      >
        <section
          v-for="g in groups"
          :key="'group:' + g.key"
          class="flex flex-col gap-2"
          :data-group="g.key"
        >
          <h2 class="text-xs font-medium text-muted">
            {{ g.name }}
          </h2>
          <ul class="flex flex-col gap-2">
            <li
              v-for="row in g.rows"
              :key="row.p.id"
              :data-project="row.p.id"
            >
              <button
                type="button"
                class="flex w-full items-start gap-3 rounded-lg border border-default px-4 py-3 text-left hover:bg-elevated focus-visible:outline-2 focus-visible:outline-primary"
                @click="open(row.p.id)"
              >
                <UIcon
                  :name="icon('agent')"
                  class="mt-0.5 size-5 flex-none text-muted"
                />
                <span class="flex min-w-0 flex-1 flex-col gap-2">
                  <span class="flex items-center gap-2">
                    <span
                      v-if="row.live"
                      class="chat-live"
                      :class="row.live"
                      :data-live="row.live"
                      :title="t(row.live === 'waiting' ? 'local_chat.waiting' : 'local_chat.live')"
                      role="img"
                      :aria-label="t(row.live === 'waiting' ? 'local_chat.waiting' : 'local_chat.live')"
                    />
                    <span class="truncate font-semibold text-highlighted">{{ row.label }}</span>
                    <span
                      v-if="row.unread"
                      class="project-unread"
                      :title="t('inbox.unread')"
                    >{{ row.unread }}</span>
                    <span
                      v-if="row.last"
                      class="ml-auto flex-none text-xs text-muted"
                    >{{ when(row.last) }}</span>
                  </span>
                  <span
                    v-if="row.seats.length"
                    class="flex flex-wrap gap-1.5"
                  >
                    <span
                      v-for="seat in row.seats"
                      :key="seat.id"
                      class="rounded-full bg-elevated px-2 py-0.5 text-xs text-muted"
                      :title="t('project.agents.status.' + seat.status)"
                    >{{ seat.label || t('settings.handler.' + seat.provider) }} · {{ t('project.agents.status.' + seat.status) }}</span>
                  </span>
                </span>
                <UIcon
                  :name="icon('fold')"
                  class="mt-1 size-4 flex-none text-muted"
                />
              </button>
            </li>
          </ul>
        </section>
      </div>
    </div>
  </section>
</template>
