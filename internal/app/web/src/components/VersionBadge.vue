<script setup lang="ts">
import { computed, ref } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UModal from '@nuxt/ui/components/Modal.vue'
import UProgress from '@nuxt/ui/components/Progress.vue'
import { api } from '@/lib/api'
import { fmt, runtime, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import type { Changelog, UpdateStatus } from '@/types'

// The build version next to the logo. It opens the update popup: a fresh
// check, the notes of every newer release (else of this one), «Обновить» and
// the download's progress. GitHub is asked by the app, never by the page.
const app = useAppStore()
const open = ref(false)
const checking = ref(false)
const changelog = ref<Changelog | null>(null)
const failure = ref('')

const upd = computed<UpdateStatus>(() => app.update || {})
const current = computed(() => upd.value.current || runtime.version || 'dev')
const versioned = computed(() => /^\d/.test(current.value))
const label = computed(() => (versioned.value ? 'v' + current.value : current.value))
const installing = computed(() => !!upd.value.installing)
const percent = computed(() => {
  const size = upd.value.size || 0
  return size > 0 ? Math.min(100, Math.floor(((upd.value.downloaded || 0) * 100) / size)) : null
})
const mb = (n: number) => (n / (1 << 20)).toFixed(1)
const progressText = computed(() => {
  const done = upd.value.downloaded || 0
  if (percent.value === null) return fmt('update.progress.bytes', { done: mb(done) })
  return fmt('update.progress', { done: mb(done), total: mb(upd.value.size || 0), percent: percent.value })
})

async function show() {
  open.value = true
  failure.value = ''
  changelog.value = null
  // A running install or restart shows its own state as it goes.
  const busy = !!upd.value.busy
  checking.value = !busy
  const check = busy
    ? Promise.resolve()
    : api<UpdateStatus>('POST', 'update/check')
      .then((s) => { app.update = s })
      .catch((e: Error) => { failure.value = e.message })
  const notes = api<Changelog>('GET', 'update/changelog')
    .then((c) => { changelog.value = c })
    .catch((e: Error) => { changelog.value = { current: current.value, newer: false, releases: [], text: e.message, failed: true } })
  await Promise.all([check, notes])
  checking.value = false
}

async function install() {
  failure.value = ''
  try {
    app.update = await api<UpdateStatus>('POST', 'update/apply')
  } catch (e) {
    failure.value = (e as Error).message
  }
}
</script>

<template>
  <button
    id="version_badge"
    type="button"
    class="relative rounded px-1.5 py-0.5 font-mono text-xs text-muted hover:bg-elevated hover:text-highlighted"
    :title="t('update.badge')"
    :aria-label="t('update.badge') + ': ' + label"
    @click="show"
  >
    {{ label }}
    <span
      v-if="upd.available"
      id="version_badge_dot"
      class="absolute -top-0.5 -right-0.5 h-2 w-2 rounded-full bg-primary"
    />
  </button>
  <UModal
    v-model:open="open"
    :title="fmt('update.dialog.title', { version: current })"
    :description="checking ? t('update.checking') : upd.failed ? '' : (upd.text || '')"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <div
        id="update_dialog"
        class="flex flex-col gap-4 text-sm"
      >
        <div
          v-if="checking"
          id="update_checking"
        >
          <UProgress
            :model-value="null"
            size="sm"
          />
        </div>
        <div
          v-else-if="installing"
          id="update_progress"
          class="flex flex-col gap-1"
        >
          <UProgress
            :model-value="percent"
            size="sm"
          />
          <span class="text-muted">{{ progressText }}</span>
        </div>
        <div
          v-else-if="upd.restarting"
          id="update_restarting"
        >
          <UProgress
            :model-value="null"
            size="sm"
          />
        </div>
        <p
          v-if="failure || (upd.failed && !checking)"
          id="update_error"
          class="text-error"
        >
          {{ failure || upd.text }}
        </p>
        <section
          v-if="changelog && !checking"
          id="update_changelog"
          class="flex flex-col gap-3"
        >
          <p
            v-if="changelog.failed"
            id="update_changelog_error"
            class="text-error"
          >
            {{ changelog.text }}
          </p>
          <template v-else-if="changelog.releases.length">
            <h3 class="font-semibold text-highlighted">
              {{ changelog.newer ? t('update.changelog.newer') : versioned ? t('update.changelog.this') : t('update.changelog.latest') }}
            </h3>
            <article
              v-for="r in changelog.releases"
              :key="r.version"
              class="release-note flex flex-col gap-1"
              :data-version="r.version"
            >
              <h4 class="font-medium">
                v{{ r.version }}<span
                  v-if="r.published"
                  class="ml-2 text-xs text-muted"
                >{{ r.published.slice(0, 10) }}</span>
              </h4>
              <pre class="max-h-64 overflow-y-auto font-sans text-xs whitespace-pre-wrap text-muted">{{ r.body || t('update.changelog.empty') }}</pre>
            </article>
          </template>
        </section>
      </div>
    </template>
    <template
      v-if="upd.available && !checking"
      #footer
    >
      <UButton
        id="update_install"
        :label="t('update.install')"
        @click="install"
      />
    </template>
  </UModal>
</template>
