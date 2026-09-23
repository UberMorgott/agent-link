<script setup lang="ts">
import { t } from '@/lib/runtime'
import { useInboxStore } from '@/stores/inbox'

const inbox = useInboxStore()
</script>

<template>
  <div
    id="message-toast-region"
    class="message-toast-region flex flex-col gap-2"
  >
    <div
      v-for="toast in inbox.toasts"
      :key="toast.id"
      class="message-toast pointer-events-auto flex items-start gap-2 rounded-xl border border-[var(--app-line)] bg-[var(--app-bg)] p-3 shadow-lg"
    >
      <button
        type="button"
        class="message-toast-main flex min-w-0 flex-1 cursor-pointer flex-col items-start gap-0.5 text-left"
        @click="inbox.openToast(toast)"
      >
        <strong class="text-sm">{{ toast.from }}</strong>
        <span class="message-toast-preview line-clamp-2 text-sm text-[var(--app-muted)]">{{ toast.body }}</span>
      </button>
      <button
        type="button"
        class="message-toast-close cursor-pointer px-1 text-lg leading-none text-[var(--app-muted)]"
        :aria-label="t('inbox.toast.close')"
        @click="inbox.dismissToast(toast.id)"
      >
        ×
      </button>
    </div>
  </div>
</template>
