<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UModal from '@nuxt/ui/components/Modal.vue'
import UProgress from '@nuxt/ui/components/Progress.vue'
import { api } from '@/lib/api'
import { renderMarkdown } from '@/lib/markdown'
import { fmt, runtime, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import type { Changelog, UpdateStatus } from '@/types'

// The build version next to the logo. It opens the update popup: a fresh
// check and the notes of every newer release (else of this one). A newer
// release installs at once, with the download's progress; the restarted app
// reloads the page (reloadOnNewVersion), which opens the popup again on the
// new version's notes. GitHub is asked by the app, never by the page.

// REOPEN_KEY marks this tab as waiting for an update: the page loaded after
// the restart opens the popup again.
const REOPEN_KEY = 'agentlink' + '.update.reopen' // split: not a string key

function setReopen(on: boolean) {
  try {
    if (on) sessionStorage.setItem(REOPEN_KEY, '1')
    else sessionStorage.removeItem(REOPEN_KEY)
  } catch { /* storage may be unavailable */ }
}

function takeReopen(): boolean {
  try {
    const on = sessionStorage.getItem(REOPEN_KEY) !== null
    sessionStorage.removeItem(REOPEN_KEY)
    return on
  } catch { return false }
}

const app = useAppStore()
const open = ref(false)
const checking = ref(false)
const changelog = ref<Changelog | null>(null)
const failure = ref('')
const applying = ref(false)

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
  if (busy) {
    if (upd.value.installing || upd.value.restarting) setReopen(true)
  } else if (upd.value.available && !failure.value) {
    await install()
  }
}

// install runs the download (its progress comes with the "update" events)
// and the restart; the request answers once the new version is in place.
async function install() {
  failure.value = ''
  applying.value = true
  setReopen(true)
  try {
    const s = await api<UpdateStatus>('POST', 'update/apply')
    app.update = s
    if (!s.restarting) setReopen(false)
  } catch (e) {
    setReopen(false)
    failure.value = (e as Error).message
  } finally {
    applying.value = false
  }
}

const failed = computed(() => !!failure.value || (!!upd.value.failed && !checking.value))
const canRetry = computed(() => failed.value && !checking.value && !applying.value && !upd.value.busy)

onMounted(() => {
  if (takeReopen()) void show()
})
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
          v-else-if="upd.restarting || applying"
          id="update_restarting"
        >
          <UProgress
            :model-value="null"
            size="sm"
          />
        </div>
        <p
          v-if="failed"
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
              <!-- eslint-disable vue/no-v-html -- renderMarkdown escapes raw HTML -->
              <div
                v-if="r.body"
                class="release-md max-h-64 overflow-y-auto text-xs text-muted"
                v-html="renderMarkdown(r.body)"
              />
              <!-- eslint-enable vue/no-v-html -->
              <p
                v-else
                class="text-xs text-muted"
              >
                {{ t('update.changelog.empty') }}
              </p>
            </article>
          </template>
        </section>
      </div>
    </template>
    <template
      v-if="canRetry"
      #footer
    >
      <UButton
        id="update_retry"
        :label="t('update.retry')"
        @click="show"
      />
    </template>
  </UModal>
</template>
