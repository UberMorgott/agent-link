<script setup lang="ts">
import UButton from '@nuxt/ui/components/Button.vue'
import { icon } from '@/lib/icons'
import { t } from '@/lib/runtime'
import { useProjectsStore } from '@/stores/projects'

// No project and no legacy network yet: make one or join one (D10).
const projects = useProjectsStore()
</script>

<template>
  <section
    data-view="welcome"
    class="flex h-full items-center justify-center overflow-y-auto px-4"
  >
    <div class="flex max-w-md flex-col items-center gap-3 text-center">
      <h1>{{ t("welcome.h1") }}</h1>
      <p class="hint">
        {{ t("welcome.hint") }}
      </p>
      <div class="mt-2 flex flex-wrap justify-center gap-2">
        <UButton
          id="welcome_create"
          :label="t('projects.new')"
          :icon="icon('plus')"
          @click="projects.openDialog('create', '')"
        />
        <UButton
          id="welcome_join"
          :label="t('projects.join')"
          :icon="icon('join')"
          color="neutral"
          variant="outline"
          @click="projects.openDialog('join', '')"
        />
      </div>
    </div>
  </section>
</template>
