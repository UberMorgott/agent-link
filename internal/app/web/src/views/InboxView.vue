<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import Button from 'primevue/button'
import Checkbox from 'primevue/checkbox'
import InputText from 'primevue/inputtext'
import Textarea from 'primevue/textarea'
import AppIcon from '@/components/AppIcon.vue'
import ChatTimeline from '@/components/ChatTimeline.vue'
import ConversationList from '@/components/ConversationList.vue'
import { isNarrow } from '@/layout/composables/layout'
import {
  activityLines, authorLabel, authorName, chatName, chatSessionList, elapsed, legacyPeerOld, memberState, others, preview,
  when, whoColor,
} from '@/lib/chat'
import { navigate, type Query } from '@/lib/nav'
import { fmt, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'

const app = useAppStore()
const inbox = useInboxStore()
const route = useRoute()
const composer = ref<{ $el: HTMLTextAreaElement } | null>(null)
const newChatForm = ref<HTMLFormElement | null>(null)

// Every visit and every query change of /ui/inbox opens what it names.
watch(() => route.fullPath, () => {
  if (route.name === 'inbox') inbox.openInbox(route.query as Query)
}, { immediate: true })

const info = computed(() => (inbox.newChatOpen ? null : inbox.chat))
const self = computed(() => app.self)
const showList = computed(() => isNarrow.value && !inbox.selectedChat && !inbox.newChatOpen)

// --- header ---

const title = computed(() => {
  if (inbox.newChatOpen) return t("inbox.new.title")
  return info.value ? chatName(info.value, self.value) : t("inbox.select")
})
// The subtitle: who is reachable right now, and the chat's project area.
const presence = computed(() => (info.value?.members || []).filter((m) => !m.self).map((member) => {
  const state = memberState(member)
  return { name: member.name, state, text: member.name + ' — ' + t(state === 'old' ? "inbox.member.old_short" : state === 'on' ? "inbox.member.online" : "inbox.member.away") }
}))
const chips = computed(() => (info.value?.members || []).map((member) => {
  const state = memberState(member)
  const notes: string[] = []
  if (!member.self) notes.push(t(state === 'old' ? "inbox.member.old" : state === 'on' ? "inbox.member.online" : "inbox.member.away"))
  if (member.queued) notes.push(fmt("inbox.member.queued", { n: member.queued }))
  for (const job of member.held || []) notes.push(job.activity || t("inbox.hold.unknown"))
  return { name: member.self ? member.name + ' (' + t("inbox.you") + ')' : member.name, state, who: whoColor(member.name), note: notes.join(' · ') }
}))
const sessions = computed(() => chatSessionList(info.value, app.sessions, app.settings).map((s) =>
  [s.provider || '', s.folder || '', t(s.wake === 'rewake' ? "inbox.session.rewake" : "inbox.session.next_event")].filter(Boolean).join(' · ')))
// Closing is the only way into the archive (a legacy chat's peer archives it too).
const canClose = computed(() => !!info.value && !info.value.closed && !info.value.archived)

// --- live activity, one line per running or queued job ---

const now = ref(Date.now())
let ticker: ReturnType<typeof setInterval> | undefined
// The elapsed timers advance locally, once a second; they never ask the app for anything.
onMounted(() => { ticker = setInterval(() => { now.value = Date.now() }, 1000) })
onBeforeUnmount(() => clearInterval(ticker))

const activity = computed(() => activityLines(info.value, inbox.messages, self.value,
  chatSessionList(info.value, app.sessions, app.settings), app.settings))
function since(iso: string) {
  const at = Date.parse(iso)
  return Number.isNaN(at) ? '' : '· ' + elapsed(now.value - at)
}

// --- composer ---

// A legacy chat is writable too: the node continues it in a real chat with the
// peer, or with a plain message when the peer's version has no chats.
const writable = computed(() => !!info.value && !info.value.closed)
const askNames = computed(() => others(info.value, self.value))
const asked = computed<string[]>({
  get: () => (info.value ? inbox.askFor(info.value) : []),
  set: (names) => { if (info.value) inbox.setAsk(info.value, names) },
})
const replying = computed(() => (inbox.replyTo ? fmt("inbox.replying", { text: authorLabel(inbox.replyTo, self.value) + ': ' + preview(inbox.replyTo.body, 60) }) : ''))

const note = computed(() => {
  const i = info.value
  if (!i || (writable.value && !i.legacy)) return null
  if (i.legacy) {
    let text = fmt(legacyPeerOld(i) ? "inbox.legacy_note_old" : "inbox.legacy_note", { name: i.peer || '' })
    if (i.archived && i.closed_by) {
      text = fmt("inbox.legacy_closed_note", { name: authorName(i.closed_by, self.value), when: i.closed_at ? when(i.closed_at) : '' }) + ' ' + text
    }
    return { text, invite: [] as string[] }
  }
  return {
    text: fmt("inbox.closed_note", { name: authorName(i.closed_by || '', self.value), when: i.closed_at ? when(i.closed_at) : '' }),
    invite: others(i, self.value),
  }
})

function onComposerKey(event: KeyboardEvent) {
  if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
    event.preventDefault()
    void inbox.submitMessage()
  }
}

watch(() => inbox.focusComposer, () => nextTick(() => composer.value?.$el.focus()))
watch(() => inbox.focusNewChat, () => nextTick(() => {
  newChatForm.value?.querySelector<HTMLInputElement>('#new_chat_members input, #new_chat_area')?.focus()
}))

function back() {
  if (inbox.newChatOpen) { inbox.hideNewChat(); if (!inbox.selectedChat) return }
  void inbox.selectChat('', '')
  navigate('inbox')
}
</script>

<template>
  <section
    data-view="inbox"
    class="h-full"
  >
    <h1 class="sr-only">
      {{ t("inbox.h1") }}
    </h1>
    <div
      id="conversation_layout"
      class="conversation-layout h-full"
      :class="{ 'has-chat': inbox.newChatOpen || inbox.selectedChat, 'no-chat': !inbox.newChatOpen && !inbox.selectedChat, 'new-open': inbox.newChatOpen }"
    >
      <ConversationList
        v-if="showList"
        class="h-full py-2"
      />
      <section
        v-else
        id="conversation_panel"
        class="conversation-panel flex h-full flex-col"
        aria-labelledby="conversation_title"
      >
        <header class="chat-head chat-column flex flex-none items-start gap-2 pb-2">
          <Button
            v-if="isNarrow"
            id="chat_back"
            :aria-label="t('inbox.back')"
            text
            rounded
            size="small"
            severity="secondary"
            @click="back"
          >
            <template #icon>
              <AppIcon name="back" />
            </template>
          </Button>
          <div class="chat-head-main min-w-0 flex-1">
            <h2
              id="conversation_title"
              class="truncate text-base font-semibold"
            >
              {{ title }}
            </h2>
            <p
              v-if="inbox.subtitleError"
              id="chat_subtitle"
              class="chat-subtitle text-xs text-[var(--app-danger)]"
            >
              {{ inbox.subtitleError }}
            </p>
            <p
              v-else-if="presence.length || info?.area"
              id="chat_subtitle"
              class="chat-subtitle flex flex-wrap gap-x-3 text-xs"
            >
              <span
                v-for="p in presence"
                :key="p.name"
                class="presence"
                :class="p.state"
              >{{ p.text }}</span>
              <span
                v-if="info?.area"
                class="chat-area text-[var(--app-muted)]"
              >{{ fmt("inbox.area", { area: info.area }) }}</span>
            </p>
          </div>
          <details
            v-if="info"
            id="chat_info"
            class="chat-info relative"
            :open="inbox.infoOpen"
            :title="t('inbox.info')"
            @toggle="inbox.infoOpen = ($event.target as HTMLDetailsElement).open"
          >
            <summary class="flex h-8 w-8 cursor-pointer list-none items-center justify-center rounded-full text-[var(--app-muted)] hover:bg-[var(--app-soft)]">
              <AppIcon name="info" /><span class="sr-only">{{ t("inbox.info") }}</span>
            </summary>
            <div class="chat-info-panel absolute right-0 z-20 mt-1 flex w-72 flex-col gap-2 rounded-xl border border-[var(--app-line)] bg-[var(--app-bg)] p-4 shadow-lg">
              <h3>{{ t("inbox.participants.label") }}</h3>
              <ul
                id="chat_members"
                class="chat-members flex flex-col gap-1.5"
                :aria-label="t('inbox.participants.label')"
              >
                <li
                  v-for="chip in chips"
                  :key="chip.name"
                  class="member-chip flex flex-col text-sm"
                  :class="chip.state"
                  :style="{ '--who': chip.who }"
                >
                  <strong class="text-[var(--who)]">{{ chip.name }}</strong>
                  <span
                    v-if="chip.note"
                    class="member-note text-xs text-[var(--app-muted)]"
                  >{{ chip.note }}</span>
                </li>
              </ul>
              <h3>{{ t("inbox.info.sessions") }}</h3>
              <ul
                id="chat_sessions"
                class="chat-sessions flex flex-col gap-1 text-sm"
              >
                <li
                  v-for="(s, i) in sessions"
                  :key="i"
                >
                  {{ s }}
                </li>
                <li
                  v-if="!sessions.length"
                  class="empty text-[var(--app-muted)]"
                >
                  {{ t("inbox.info.no_sessions") }}
                </li>
              </ul>
            </div>
          </details>
          <Button
            v-if="canClose"
            id="chat_close"
            :aria-label="t('inbox.close')"
            :title="t('inbox.close')"
            text
            rounded
            size="small"
            severity="secondary"
            :disabled="inbox.closing"
            @click="inbox.confirmClose"
          >
            <template #icon>
              <AppIcon name="archive" />
            </template>
          </Button>
        </header>

        <form
          v-if="inbox.newChatOpen"
          id="new_chat_form"
          ref="newChatForm"
          class="new-chat chat-column flex flex-col gap-4 py-6"
          :aria-busy="inbox.newChatBusy ? 'true' : undefined"
          @submit.prevent="inbox.createChat"
        >
          <p class="hint">
            {{ t("inbox.new.hint") }}
          </p>
          <div
            class="new-chat-group flex flex-col gap-2"
            role="group"
            aria-labelledby="new_chat_people"
          >
            <span
              id="new_chat_people"
              class="field-label text-sm font-medium"
            >{{ t("inbox.new.participants") }}</span>
            <span
              id="new_chat_members"
              class="ask-choices flex flex-wrap gap-x-4 gap-y-2"
            >
              <label
                v-for="person in inbox.newChatPeople"
                :key="person.name"
                class="choice"
                :class="{ on: person.online }"
                :style="{ '--who': whoColor(person.name) }"
              >
                <Checkbox
                  v-model="inbox.newChatChosen"
                  :value="person.name"
                  :input-id="'new_chat_' + person.name"
                />
                <span>{{ person.name }}</span>
              </label>
            </span>
          </div>
          <p
            v-if="!inbox.newChatPeople.length"
            id="new_chat_empty"
            class="hint"
          >
            {{ t("inbox.new.no_members") }}
          </p>
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{{ t("inbox.new.area") }}</span>
            <InputText
              id="new_chat_area"
              v-model="inbox.newChatArea"
              name="area"
              autocomplete="off"
              spellcheck="false"
              list="new_chat_areas"
            />
          </label>
          <datalist id="new_chat_areas">
            <option
              v-for="area in inbox.newChatAreas"
              :key="area"
              :value="area"
            />
          </datalist>
          <p class="hint">
            {{ t("inbox.new.area.hint") }}
          </p>
          <div class="new-chat-actions flex items-center justify-end gap-2">
            <p
              id="new_chat_result"
              role="status"
              class="mr-auto text-sm text-[var(--app-danger)]"
            >
              {{ inbox.newChatResult }}
            </p>
            <Button
              id="new_chat_dismiss"
              type="button"
              :label="t('inbox.new.dismiss')"
              text
              severity="secondary"
              @click="inbox.hideNewChat"
            />
            <Button
              id="new_chat_create"
              type="submit"
              :label="t('inbox.new.create')"
              :disabled="!inbox.newChatPeople.length || inbox.newChatBusy"
            />
          </div>
        </form>

        <template v-else>
          <ChatTimeline />
          <div class="chat-column flex flex-none flex-col gap-2 pb-4">
            <ul
              v-if="activity.length"
              id="chat_activity"
              class="chat-activity flex flex-col gap-1 px-1"
              :aria-label="t('inbox.activity.label')"
            >
              <li
                v-for="row in activity"
                :key="row.key"
                class="act-row"
                :class="row.cls"
                :style="{ '--who': whoColor(row.name) }"
              >
                <span
                  class="act-spin"
                  aria-hidden="true"
                />
                <strong class="act-who">{{ row.who }}</strong>
                <span
                  class="act-text"
                  :title="row.text"
                >{{ row.text }}</span>
                <span class="act-time">{{ since(row.since) }}</span>
              </li>
            </ul>
            <form
              v-if="writable"
              id="send"
              class="composer flex flex-col gap-2"
              :aria-busy="inbox.sending ? 'true' : undefined"
              @submit.prevent="inbox.submitMessage"
            >
              <div
                v-if="askNames.length >= 2"
                id="ask_row"
                class="ask-row flex flex-wrap items-center gap-x-4 gap-y-1 px-1 text-sm"
                role="group"
                aria-labelledby="ask_label"
              >
                <span
                  id="ask_label"
                  class="field-label text-[var(--app-muted)]"
                >{{ t("inbox.ask.label") }}</span>
                <span
                  id="ask_choices"
                  class="ask-choices flex flex-wrap gap-x-4 gap-y-1"
                >
                  <label
                    v-for="name in askNames"
                    :key="name"
                    class="choice"
                    :style="{ '--who': whoColor(name) }"
                  >
                    <Checkbox
                      v-model="asked"
                      :value="name"
                      :input-id="'ask_' + name"
                    />
                    <span>{{ name }}</span>
                  </label>
                </span>
                <span
                  v-if="!asked.length"
                  id="ask_hint"
                  class="ask-hint text-xs text-[var(--app-off)]"
                >{{ t("inbox.ask.none") }}</span>
              </div>
              <div class="composer-box rounded-3xl border border-[var(--app-line)] bg-[var(--app-bg)] px-4 py-2 shadow-sm focus-within:border-[var(--p-primary-color)]">
                <p
                  v-if="inbox.replyTo"
                  id="replying"
                  class="replying flex items-center gap-2 pt-1 text-xs text-[var(--app-muted)]"
                >
                  <span
                    id="replying_text"
                    class="min-w-0 flex-1 truncate"
                  >{{ replying }}</span>
                  <button
                    id="cancel_reply"
                    type="button"
                    class="cursor-pointer hover:underline"
                    @click="inbox.setReply(null)"
                  >
                    {{ t("inbox.cancel_reply") }}
                  </button>
                </p>
                <div class="composer-row flex items-end gap-2">
                  <label class="composer-body min-w-0 flex-1">
                    <span class="sr-only">{{ t("inbox.body.label") }}</span>
                    <Textarea
                      id="body"
                      ref="composer"
                      v-model="inbox.composer"
                      name="body"
                      rows="1"
                      auto-resize
                      class="max-h-60 w-full !border-0 !bg-transparent !px-0 !shadow-none"
                      :placeholder="t('inbox.body.placeholder')"
                      @update:model-value="inbox.saveDraft(inbox.selectedChat)"
                      @keydown="onComposerKey"
                    />
                  </label>
                  <Button
                    id="send_button"
                    type="submit"
                    rounded
                    size="small"
                    class="mb-1 flex-none"
                    :aria-label="t('inbox.send')"
                    :disabled="inbox.sending"
                  >
                    <template #icon>
                      <AppIcon name="send" />
                    </template>
                  </Button>
                </div>
              </div>
              <p
                id="inbox_result"
                class="composer-result px-1 text-xs text-[var(--app-danger)]"
                role="status"
              >
                {{ inbox.sendResult }}
              </p>
            </form>
            <p
              v-if="note"
              id="chat_note"
              class="chat-note flex flex-wrap items-center gap-2 rounded-xl bg-[var(--app-soft)] px-4 py-3 text-sm"
            >
              <span id="chat_note_text">{{ note.text }}</span>
              <Button
                v-if="note.invite.length"
                id="chat_note_action"
                :label="fmt('inbox.new_with', { names: note.invite.join(', ') })"
                size="small"
                text
                @click="inbox.showNewChat(note.invite)"
              />
            </p>
          </div>
        </template>
      </section>
    </div>
  </section>
</template>
