<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UInput from '@nuxt/ui/components/Input.vue'
import UModal from '@nuxt/ui/components/Modal.vue'
import { pickFolder } from '@/lib/folders'
import { openProject } from '@/lib/nav'
import { t } from '@/lib/runtime'
import { useProjectsStore } from '@/stores/projects'

// A new project needs only a name; the folder can come later (D4).
const projects = useProjectsStore()

const open = computed({
  get: () => projects.dialog === 'create',
  set: (value: boolean) => { if (!value) projects.closeDialog() },
})
const name = ref('')
const dir = ref('')
const alias = ref('')
const result = ref('')
const busy = ref(false)

watch(open, (value) => {
  if (!value) return
  name.value = dir.value = alias.value = result.value = ''
})

async function pick() {
  const r = await pickFolder(dir.value)
  if (r.path) dir.value = r.path
  else result.value = r.message || ''
}

async function create() {
  if (busy.value) return
  busy.value = true
  result.value = ''
  try {
    const body: { name: string; dir?: string; alias?: string } = { name: name.value.trim() }
    if (dir.value.trim()) body.dir = dir.value.trim()
    if (alias.value.trim()) body.alias = alias.value.trim()
    const view = await projects.create(body)
    projects.closeDialog()
    openProject(view.id)
  } catch (error) {
    result.value = (error as Error).message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    :title="t('project.create.title')"
    :description="t('project.create.hint')"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form
        id="create_form"
        class="flex flex-col gap-2"
        @submit.prevent="create"
      >
        <label
          for="create_name"
          class="text-sm font-medium"
        >{{ t("project.name.label") }}</label>
        <UInput
          id="create_name"
          v-model="name"
          autocomplete="off"
          maxlength="80"
          autofocus
        />
        <label
          for="create_dir"
          class="mt-2 text-sm font-medium"
        >{{ t("project.create.folder") }}</label>
        <span class="flex gap-2">
          <UInput
            id="create_dir"
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
        <label
          for="create_alias"
          class="mt-2 text-sm font-medium"
        >{{ t("project.alias.label") }}</label>
        <UInput
          id="create_alias"
          v-model="alias"
          autocomplete="off"
          maxlength="64"
        />
        <p class="hint">
          {{ t("project.alias.hint") }}
        </p>
        <p
          class="dialog-result text-sm text-error"
          role="status"
        >
          {{ result }}
        </p>
      </form>
    </template>
    <template #footer>
      <UButton
        :label="t('project.cancel')"
        color="neutral"
        variant="ghost"
        @click="projects.closeDialog"
      />
      <UButton
        id="create_submit"
        type="submit"
        form="create_form"
        :label="t('project.create.submit')"
        :disabled="busy || !name.trim()"
      />
    </template>
  </UModal>
</template>
