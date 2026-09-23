<script setup lang="ts">
import { computed } from 'vue'
import Button from 'primevue/button'
import AppIcon from './AppIcon.vue'
import { authorLabel, chatName, clock, isUnread, preview, workingLines } from '@/lib/chat'
import { navigate } from '@/lib/nav'
import { t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'
import type { ChatInfo } from '@/types'

const app = useAppStore()
const inbox = useInboxStore()

const archive = computed(() => app.showArchive)
const source = computed(() => (archive.value ? app.chatArchive : app.chats))

interface Row {
  chat: ChatInfo
  name: string
  selected: boolean
  working: string[]
  fresh: boolean
  time: string
  last: string
}

const rows = computed<Row[]>(() => (source.value || []).map((chat) => {
  const self = app.self
  const working = workingLines(chat, self)
  const lm = chat.last_message
  return {
    chat,
    name: chatName(chat, self),
    selected: chat.id === inbox.selectedChat && !inbox.newChatOpen,
    working,
    fresh: isUnread(chat, inbox.selectedChat, inbox.reads),
    time: chat.last_at ? clock(chat.last_at) : '',
    last: working.length
      ? working[0] + (working.length > 1 ? ' +' + (working.length - 1) : '')
      : lm ? authorLabel(lm, self) + ': ' + preview(lm.body, 90) : chat.title || '',
  }
}))

function toggleArchive() {
  app.showArchive = !app.showArchive
  if (app.showArchive) void app.refreshSlice('chats')
}
</script>

<template>
  <section
    class="flex flex-col"
    aria-labelledby="chat_list_title"
  >
    <div class="flex items-center justify-between px-4 pb-1">
      <h2
        id="chat_list_title"
        class="text-xs font-semibold tracking-wide text-[var(--app-muted)] uppercase"
      >
        {{ t(archive ? "inbox.archive.title" : "inbox.list.label") }}
      </h2>
      <Button
        id="new_chat"
        :label="t('inbox.new')"
        text
        size="small"
        @click="inbox.showNewChat([])"
      >
        <template #icon>
          <AppIcon name="plus" />
        </template>
      </Button>
    </div>
    <ul
      id="conversation_list"
      class="min-h-0 flex-1 overflow-y-auto px-2"
    >
      <li
        v-for="row in rows"
        :key="row.chat.id"
      >
        <button
          type="button"
          class="chat-row"
          :class="{ active: row.selected, live: row.working.length, fresh: row.fresh }"
          :data-chat="row.chat.id"
          :aria-pressed="row.selected ? 'true' : 'false'"
          @click="navigate('inbox', { chat: row.chat.id })"
        >
          <span class="row-top">
            <strong class="row-who">{{ row.name }}</strong>
            <span
              v-if="row.chat.legacy"
              class="chat-badge legacy"
            >{{ t("inbox.badge.legacy") }}</span>
            <span class="row-time">{{ row.time }}</span>
          </span>
          <span class="row-foot">
            <span :class="row.working.length ? 'row-live' : 'conversation-preview'">{{ row.last }}</span>
            <span
              v-if="row.fresh"
              class="conversation-unread"
            >{{ t("inbox.unread") }}</span>
          </span>
        </button>
      </li>
      <li
        v-if="!rows.length && source"
        class="empty px-3 py-2 text-sm text-[var(--app-muted)]"
      >
        {{ t(archive ? "inbox.archive.empty" : "inbox.list.empty") }}
      </li>
    </ul>
    <div class="flex flex-col gap-1 px-2 pt-2">
      <Button
        id="archive_toggle"
        :label="t(archive ? 'inbox.archive.hide' : 'inbox.archive.show')"
        text
        size="small"
        severity="secondary"
        class="!justify-start"
        :aria-pressed="archive ? 'true' : 'false'"
        @click="toggleArchive"
      >
        <template #icon>
          <AppIcon name="archive" />
        </template>
      </Button>
      <span
        class="link-status truncate px-3 text-xs"
        :class="app.link.cls"
      >{{ app.link.text }}</span>
    </div>
  </section>
</template>
