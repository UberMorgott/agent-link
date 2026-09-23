<script setup lang="ts">
import { t } from '@/lib/runtime'
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
    </div>
  </section>
</template>
