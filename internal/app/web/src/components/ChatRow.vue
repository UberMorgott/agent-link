<script setup lang="ts">
import { computed } from 'vue'
import UBadge from '@nuxt/ui/components/Badge.vue'
import { authorLabel, chatName, clock, isUnread, preview, workingLines } from '@/lib/chat'
import { openChat } from '@/lib/nav'
import { t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { useInboxStore, chatKey } from '@/stores/inbox'
import type { ChatInfo } from '@/types'

// One chat in a list: its name, the newest line or what its agents do now,
// the time and the unread mark.
const props = defineProps<{ project: string; chat: ChatInfo; compact?: boolean }>()

const app = useAppStore()
const inbox = useInboxStore()

const row = computed(() => {
  const self = app.self
  const chat = props.chat
  const working = workingLines(chat, self)
  const lm = chat.last_message
  const selected = inbox.openKey() === chatKey(props.project, chat.id) && !inbox.newChatOpen
  return {
    name: chatName(chat, self),
    selected,
    working,
    fresh: isUnread(chat, selected, inbox.readOf(props.project, chat.id)),
    time: chat.last_at ? clock(chat.last_at) : '',
    last: working.length
      ? working[0] + (working.length > 1 ? ' +' + (working.length - 1) : '')
      : lm ? authorLabel(lm, self) + ': ' + preview(lm.body, 90) : '',
  }
})
</script>

<template>
  <button
    type="button"
    class="chat-row"
    :class="{ active: row.selected, live: row.working.length, fresh: row.fresh, compact }"
    :data-chat="chat.id"
    :aria-current="row.selected ? 'page' : undefined"
    @click="openChat(project, chat.id)"
  >
    <span class="row-top">
      <strong class="row-who">{{ row.name }}</strong>
      <UBadge
        v-if="chat.legacy"
        class="chat-badge legacy"
        :label="t('inbox.badge.legacy')"
        color="neutral"
        variant="soft"
        size="sm"
      />
      <span
        v-if="!compact"
        class="row-time"
      >{{ row.time }}</span>
      <span
        v-if="row.fresh"
        class="conversation-unread"
        :title="t('inbox.unread')"
      ><span class="sr-only">{{ t("inbox.unread") }}</span></span>
    </span>
    <span
      v-if="!compact || row.working.length"
      class="row-foot"
    >
      <span :class="row.working.length ? 'row-live' : 'conversation-preview'">{{ row.last }}</span>
    </span>
  </button>
</template>
