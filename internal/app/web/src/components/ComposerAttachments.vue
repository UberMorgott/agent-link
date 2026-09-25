<script setup lang="ts">
import UButton from '@nuxt/ui/components/Button.vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import { fileURL, isImage, sizeText } from '@/lib/attachments'
import { icon } from '@/lib/icons'
import { t } from '@/lib/runtime'
import { useAttachmentsStore } from '@/stores/attachments'

// The composer's files as chips: a thumbnail for an image once uploaded, the
// name and size, and a button that takes the file off the message.
const files = useAttachmentsStore()
</script>

<template>
  <ul
    v-if="files.items.length"
    id="composer_files"
    class="composer-files flex flex-wrap gap-2 px-1"
  >
    <li
      v-for="p in files.items"
      :key="p.key"
      class="composer-file flex max-w-60 items-center gap-2 rounded-xl bg-elevated py-1 pr-1 pl-2 text-xs"
      :class="{ uploading: !p.attachment }"
      :data-file="p.name"
    >
      <img
        v-if="p.attachment && isImage(p.attachment)"
        class="composer-thumb size-8 rounded object-cover"
        :src="fileURL(p.project, p.attachment)"
        :alt="p.name"
      >
      <UIcon
        v-else
        :name="icon('file')"
        class="size-4 flex-none"
      />
      <span class="flex min-w-0 flex-col">
        <span class="truncate">{{ p.name }}</span>
        <span class="text-muted">{{ p.attachment ? sizeText(p.attachment.size) : t("inbox.attach.uploading") }}</span>
      </span>
      <UButton
        :icon="icon('close')"
        :aria-label="t('inbox.attach.remove') + ': ' + p.name"
        :title="t('inbox.attach.remove')"
        class="composer-file-remove"
        color="neutral"
        variant="ghost"
        size="xs"
        @click="files.remove(p.key)"
      />
    </li>
  </ul>
</template>
