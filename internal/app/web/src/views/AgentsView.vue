<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import { icon } from '@/lib/icons'
import { t } from '@/lib/runtime'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'

// The local chats are independent projects. Network project chats stay in the
// other sidebar section and never appear here.
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

const rows = computed(() => (projects.list || [])
  .filter((p) => p.scope === 'local')
  .map((p) => ({ project: p, seats: projects.seats[p.id] || [] })))

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
        v-if="!rows.length && !pending && projects.list"
        class="hint"
      >
        {{ t('agents.empty') }}
      </p>
      <ul
        v-if="rows.length"
        id="agents_projects"
        class="flex flex-col gap-2"
      >
        <li
          v-for="row in rows"
          :key="row.project.id"
          :data-project="row.project.id"
        >
          <button
            type="button"
            class="flex w-full items-start gap-3 rounded-lg border border-default px-4 py-3 text-left hover:bg-elevated focus-visible:outline-2 focus-visible:outline-primary"
            @click="open(row.project.id)"
          >
            <UIcon
              :name="icon('agent')"
              class="mt-0.5 size-5 flex-none text-muted"
            />
            <span class="flex min-w-0 flex-1 flex-col gap-2">
              <span class="truncate font-semibold text-highlighted">{{ row.project.display || t('projects.connecting') }}</span>
              <span class="flex flex-wrap gap-1.5">
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
    </div>
  </section>
</template>
