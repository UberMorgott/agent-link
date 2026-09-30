<script setup lang="ts">
import UButton from '@nuxt/ui/components/Button.vue'
import UModal from '@nuxt/ui/components/Modal.vue'
import { t } from '@/lib/runtime'

// A question before an action nobody can take back (lib/confirm.ts): the
// action's own name on the button, «Отмена» beside it. The overlay that opens
// it binds `open`; close answers the question.
defineProps<{ text: string; action: string }>()
const emit = defineEmits<{ close: [ok: boolean] }>()
</script>

<template>
  <UModal
    :title="action"
    :description="text"
    :ui="{ footer: 'justify-end' }"
  >
    <template #footer>
      <UButton
        id="confirm_cancel"
        :label="t('project.cancel')"
        color="neutral"
        variant="ghost"
        @click="emit('close', false)"
      />
      <UButton
        id="confirm_ok"
        :label="action"
        color="error"
        @click="emit('close', true)"
      />
    </template>
  </UModal>
</template>
