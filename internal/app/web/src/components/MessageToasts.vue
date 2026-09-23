<script setup lang="ts">
import UButton from '@nuxt/ui/components/Button.vue'
import { icon } from '@/lib/icons'
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
      class="message-toast pointer-events-auto flex items-start gap-2 rounded-lg bg-default p-3 shadow-lg ring ring-default"
    >
      <button
        type="button"
        class="message-toast-main flex min-w-0 flex-1 cursor-pointer flex-col items-start gap-0.5 text-left"
        @click="inbox.openToast(toast)"
      >
        <strong class="text-sm text-highlighted">{{ toast.from }}</strong>
        <span class="message-toast-preview line-clamp-2 text-sm text-muted">{{ toast.body }}</span>
      </button>
      <UButton
        class="message-toast-close"
        :icon="icon('close')"
        color="neutral"
        variant="link"
        size="sm"
        :aria-label="t('inbox.toast.close')"
        @click="inbox.dismissToast(toast.id)"
      />
    </div>
  </div>
</template>
