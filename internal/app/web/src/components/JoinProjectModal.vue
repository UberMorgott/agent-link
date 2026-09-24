<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UInput from '@nuxt/ui/components/Input.vue'
import UModal from '@nuxt/ui/components/Modal.vue'
import { ApiError } from '@/lib/api'
import { pickFolder } from '@/lib/folders'
import { openProject } from '@/lib/nav'
import { fmt, t } from '@/lib/runtime'
import { useProjectsStore } from '@/stores/projects'

// Joining: the invite (and, off the local network, a member's address), then
// the wait for the project's shared name, then this member's folder (D7).
// Joining is done once the invite is accepted: «Отмена» after that leaves the
// project, and only when this join created it.
const projects = useProjectsStore()

const step = computed(() => projects.joinStep)
const joined = computed(() => projects.byID(projects.joinProject))
const open = computed({
  get: () => projects.dialog === 'join',
  set: (value: boolean) => { if (!value && step.value === 'invite') projects.closeDialog() },
})
const invite = ref('')
const addr = ref('')
const dir = ref('')
const alias = ref('')
const result = ref('')
const busy = ref(false)
// workDir: the legacy network's working folder, asked for after a 400
// work_dir (its code joined while an agent answers).
const workDir = ref('')
const needsWorkDir = ref(false)

watch(open, (value) => {
  if (!value) return
  projects.joinReset()
  invite.value = addr.value = dir.value = alias.value = result.value = workDir.value = ''
  needsWorkDir.value = false
})

async function run(action: () => Promise<unknown>) {
  if (busy.value) return
  busy.value = true
  result.value = ''
  try {
    await action()
  } catch (error) {
    result.value = (error as Error).message
  } finally {
    busy.value = false
  }
}

// finish closes the dialog on the joined project's page.
function finish(pid: string) {
  projects.joinReset()
  projects.closeDialog()
  openProject(pid)
}

function join() {
  return run(async () => {
    let r
    try {
      r = await projects.join(invite.value, addr.value, needsWorkDir.value ? workDir.value : '')
    } catch (error) {
      if (error instanceof ApiError && error.code === 'work_dir') needsWorkDir.value = true
      throw error
    }
    // A project that was here already, or the legacy network, just opens.
    if (!r.created || r.project.legacy) finish(r.project.id)
  })
}

async function pick() {
  const r = await pickFolder(dir.value)
  if (r.path) dir.value = r.path
  else result.value = r.message || ''
}

async function pickWorkDir() {
  const r = await pickFolder(workDir.value)
  if (r.path) workDir.value = r.path
  else result.value = r.message || ''
}

function done() {
  return run(async () => {
    const pid = projects.joinProject
    const body: { dir?: string; alias?: string } = {}
    if (dir.value.trim()) body.dir = dir.value.trim()
    if (alias.value.trim()) body.alias = alias.value.trim()
    if (body.dir || body.alias) await projects.bind(pid, body)
    finish(pid)
  })
}

// wait closes the dialog and leaves the project connecting in the sidebar.
function wait() { finish(projects.joinProject) }

function cancel() {
  if (step.value === 'invite') { projects.closeDialog(); return }
  return run(async () => {
    await projects.joinCancel()
    projects.closeDialog()
  })
}
</script>

<template>
  <UModal
    v-model:open="open"
    :title="t('project.join.title')"
    :description="step === 'invite' ? t('project.join.hint') : t('project.join.cancel_hint')"
    :dismissible="step === 'invite'"
    :close="step === 'invite'"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form
        v-if="step === 'invite'"
        id="join_form"
        class="flex flex-col gap-2"
        @submit.prevent="join"
      >
        <label
          for="join_invite"
          class="text-sm font-medium"
        >{{ t("project.join.invite") }}</label>
        <UInput
          id="join_invite"
          v-model="invite"
          autocomplete="off"
          spellcheck="false"
          autofocus
          :ui="{ base: 'font-mono' }"
        />
        <label
          for="join_addr"
          class="mt-2 text-sm font-medium"
        >{{ t("project.join.addr") }}</label>
        <UInput
          id="join_addr"
          v-model="addr"
          autocomplete="off"
          :placeholder="t('participants.add.placeholder')"
        />
        <p class="hint">
          {{ t("project.join.addr_hint") }}
        </p>
        <template v-if="needsWorkDir">
          <label
            for="join_work_dir"
            class="mt-2 text-sm font-medium"
          >{{ t("project.join.work_dir") }}</label>
          <span class="flex gap-2">
            <UInput
              id="join_work_dir"
              v-model="workDir"
              class="flex-1"
              autocomplete="off"
              spellcheck="false"
            />
            <UButton
              type="button"
              :label="t('settings.work_dir.pick')"
              color="neutral"
              variant="outline"
              @click="pickWorkDir"
            />
          </span>
          <p class="hint">
            {{ t("project.join.work_dir_hint") }}
          </p>
        </template>
      </form>
      <p
        v-else-if="step === 'connecting'"
        id="join_connecting"
        class="flex items-center gap-2 text-sm"
      >
        <span
          class="act-spin"
          aria-hidden="true"
        />{{ t("project.join.connecting") }}
      </p>
      <form
        v-else
        id="join_folder"
        class="flex flex-col gap-2"
        @submit.prevent="done"
      >
        <p
          id="join_joined"
          class="text-sm"
        >
          {{ fmt("project.join.joined", { name: joined?.name || '' }) }}
        </p>
        <label
          for="join_dir"
          class="mt-2 text-sm font-medium"
        >{{ t("project.create.folder") }}</label>
        <span class="flex gap-2">
          <UInput
            id="join_dir"
            v-model="dir"
            class="flex-1"
            autocomplete="off"
            spellcheck="false"
          />
          <UButton
            type="button"
            :label="t('settings.work_dir.pick')"
            color="neutral"
            variant="outline"
            @click="pick"
          />
        </span>
        <p class="hint warn">
          {{ t("project.folder.warn") }}
        </p>
        <label
          for="join_alias"
          class="mt-2 text-sm font-medium"
        >{{ t("project.alias.label") }}</label>
        <UInput
          id="join_alias"
          v-model="alias"
          autocomplete="off"
          maxlength="64"
          :placeholder="joined?.name"
        />
      </form>
      <p
        class="dialog-result mt-2 text-sm text-error"
        role="status"
      >
        {{ result }}
      </p>
    </template>
    <template #footer>
      <UButton
        id="join_cancel"
        :label="t('project.cancel')"
        color="neutral"
        variant="ghost"
        :disabled="busy"
        @click="cancel"
      />
      <UButton
        v-if="step === 'invite'"
        id="join_submit"
        type="submit"
        form="join_form"
        :label="t('projects.join')"
        :disabled="busy || !invite.trim()"
      />
      <UButton
        v-else-if="step === 'connecting'"
        id="join_wait"
        :label="t('project.join.wait')"
        color="neutral"
        variant="outline"
        @click="wait"
      />
      <UButton
        v-else
        id="join_done"
        type="submit"
        form="join_folder"
        :label="t('project.join.done')"
        :disabled="busy"
      />
    </template>
  </UModal>
</template>
