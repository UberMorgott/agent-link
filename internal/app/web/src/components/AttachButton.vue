<script setup lang="ts">
import { ref } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import { icon } from '@/lib/icons'
import { t } from '@/lib/runtime'
import { useAttachmentsStore } from '@/stores/attachments'

// The composer's paperclip: opens the file picker, and the picked files go
// onto the message.
const props = defineProps<{ project: string }>()
const files = useAttachmentsStore()
const input = ref<HTMLInputElement | null>(null)

function picked(event: Event) {
  const el = event.target as HTMLInputElement
  files.add(props.project, Array.from(el.files || []))
  el.value = '' // the same file can be picked again
}
</script>

<template>
  <span class="attach-button contents">
    <input
      id="attach_input"
      ref="input"
      type="file"
      multiple
      class="hidden"
      aria-hidden="true"
      tabindex="-1"
      accept="image/png,image/jpeg,image/gif,image/webp,application/pdf,text/*,.md,.json,.csv,.log,.yaml,.yml,.toml"
      @change="picked"
    >
    <UButton
      id="attach_button"
      type="button"
      :icon="icon('attach')"
      :aria-label="t('inbox.attach')"
      :title="t('inbox.attach')"
      color="neutral"
      variant="ghost"
      class="rounded-full"
      @click="input?.click()"
    />
  </span>
</template>
