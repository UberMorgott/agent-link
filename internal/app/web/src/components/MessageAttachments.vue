<script setup lang="ts">
import { computed } from 'vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import { fileURL, isImage, sizeText } from '@/lib/attachments'
import { icon } from '@/lib/icons'
import { t } from '@/lib/runtime'
import type { Attachment } from '@/types'

// A message's files: images as thumbnails that open full size, other files as
// chips that download; a file that never arrived says so.
const props = defineProps<{ project: string; items: Attachment[] }>()

const images = computed(() => props.items.filter((a) => !a.failed && isImage(a)))
const others = computed(() => props.items.filter((a) => a.failed || !isImage(a)))
</script>

<template>
  <div class="msg-files flex flex-col gap-1.5">
    <div
      v-if="images.length"
      class="msg-images flex flex-wrap gap-1.5"
    >
      <a
        v-for="a in images"
        :key="a.id"
        class="msg-image block"
        :href="fileURL(project, a)"
        target="_blank"
        rel="noopener"
        :title="a.name"
      >
        <img
          class="max-h-60 max-w-full rounded-lg object-contain"
          :src="fileURL(project, a)"
          :alt="a.name"
          loading="lazy"
        >
      </a>
    </div>
    <ul
      v-if="others.length"
      class="msg-file-list flex flex-wrap gap-1.5"
    >
      <li
        v-for="a in others"
        :key="a.id"
      >
        <span
          v-if="a.failed"
          class="msg-file failed flex items-center gap-1.5 rounded-lg bg-elevated px-2 py-1 text-xs text-muted"
          :title="t('inbox.attach.failed')"
        >
          <UIcon :name="icon('failed')" />
          <span class="truncate">{{ a.name }}</span>
          <span>· {{ t("inbox.attach.failed") }}</span>
        </span>
        <a
          v-else
          class="msg-file flex items-center gap-1.5 rounded-lg bg-elevated px-2 py-1 text-xs hover:underline"
          :href="fileURL(project, a, true)"
          :download="a.name"
          :title="t('inbox.attach.download') + ': ' + a.name"
        >
          <UIcon :name="icon('file')" />
          <span class="truncate">{{ a.name }}</span>
          <span class="text-muted">{{ sizeText(a.size) }}</span>
          <UIcon :name="icon('download')" />
        </a>
      </li>
    </ul>
  </div>
</template>
