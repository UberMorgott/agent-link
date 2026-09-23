<script setup lang="ts">
import { computed } from 'vue'
import { fmt, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'

const app = useAppStore()

const data = computed(() => app.dashboard)
const status = computed(() => data.value?.status || {})
const online = computed(() => ({ online: status.value.online || 0, total: status.value.total || 0 }))
const cards = computed(() => [
  { label: t("dashboard.online"), value: fmt("link.on_many", online.value) },
  { label: t("dashboard.messages"), value: String(data.value?.total_messages || 0) },
  { label: t("dashboard.active"), value: String(data.value?.active_requests || 0) },
  { label: t("dashboard.handler"), value: t("settings.handler." + (status.value.handler || 'none')) },
])
</script>

<template>
  <section
    data-view="dashboard"
    class="h-full overflow-y-auto"
  >
    <div class="mx-auto flex max-w-3xl flex-col gap-8 px-4 py-10">
      <header class="view-header flex flex-col gap-1">
        <h1>{{ t("dashboard.h1") }}</h1>
        <p
          id="dashboard_summary"
          class="hint"
        >
          <template v-if="data">
            {{ fmt("dashboard.summary", { ...online, messages: data.total_messages || 0 }) }}
          </template>
        </p>
      </header>
      <div
        id="dashboard_cards"
        class="dashboard-cards grid grid-cols-2 gap-x-6 gap-y-5 sm:grid-cols-4"
      >
        <article
          v-for="card in data ? cards : []"
          :key="card.label"
          class="flex flex-col gap-1"
        >
          <h2 class="text-xs font-medium text-[var(--app-muted)]">
            {{ card.label }}
          </h2>
          <p class="text-lg font-semibold">
            {{ card.value }}
          </p>
        </article>
      </div>
      <section
        aria-labelledby="dashboard_recent_title"
        class="flex flex-col gap-2"
      >
        <h2 id="dashboard_recent_title">
          {{ t("dashboard.recent") }}
        </h2>
        <ul
          id="dashboard_recent"
          class="dashboard-recent flex flex-col divide-y divide-[var(--app-line)]"
        >
          <template v-if="data">
            <li
              v-for="(conversation, i) in data.recent || []"
              :key="i"
              class="truncate py-2 text-sm"
            >
              <strong>{{ conversation.peer || "—" }}</strong>: <span class="text-[var(--app-muted)]">{{ conversation.preview || "" }}</span>
            </li>
            <li
              v-if="!(data.recent || []).length"
              class="py-2 text-sm text-[var(--app-muted)]"
            >
              {{ t("dashboard.recent.empty") }}
            </li>
          </template>
        </ul>
      </section>
    </div>
  </section>
</template>
