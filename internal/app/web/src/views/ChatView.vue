<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import UButton from '@nuxt/ui/components/Button.vue'
import UChatPrompt from '@nuxt/ui/components/ChatPrompt.vue'
import UCheckbox from '@nuxt/ui/components/Checkbox.vue'
import UPopover from '@nuxt/ui/components/Popover.vue'
import ChatTimeline from '@/components/ChatTimeline.vue'
import { isNarrow } from '@/layout/composables/layout'
import { icon } from '@/lib/icons'
import {
  activityLines, authorLabel, authorName, chatName, chatSessionList, elapsed, legacyPeerOld, memberState, others, preview,
  when, whoColor,
} from '@/lib/chat'
import { openProject } from '@/lib/nav'
import { fmt, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'

const app = useAppStore()
const inbox = useInboxStore()
const projects = useProjectsStore()
const route = useRoute()
const newChatForm = ref<HTMLFormElement | null>(null)

const pid = computed(() => String(route.params.project || ''))

// Every visit of /ui/p/{pid}/c/{chat} opens what it names.
watch(() => route.fullPath, () => {
  if (route.name !== 'chat') return
  const message = route.query.message
  projects.open(pid.value)
  void inbox.selectChat(pid.value, String(route.params.chat || ''), typeof message === 'string' ? message : '')
}, { immediate: true })

const info = computed(() => (inbox.newChatOpen ? null : inbox.chat))
const self = computed(() => app.self)

// --- header ---

const title = computed(() => {
  if (inbox.newChatOpen) return t("inbox.new.title")
  return chatName(info.value, self.value)
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

// One checkbox per person: a name in the list is a ticked box.
function toggle(names: string[], name: string, on: boolean | 'indeterminate'): string[] {
  const rest = names.filter((n) => n !== name)
  return on === true ? [...rest, name] : rest
}

watch(() => inbox.focusComposer, () => nextTick(() => document.getElementById('body')?.focus()))
watch(() => inbox.focusNewChat, () => nextTick(() => {
  newChatForm.value?.querySelector<HTMLElement>('#new_chat_members [role="checkbox"]')?.focus()
}))

function back() {
  inbox.hideNewChat()
  openProject(pid.value)
}
</script>

<template>
  <section
    data-view="chat"
    class="h-full"
  >
    <div
      id="conversation_layout"
      class="conversation-layout h-full"
    >
      <section
        id="conversation_panel"
        class="conversation-panel flex h-full flex-col"
        aria-labelledby="conversation_title"
      >
        <header class="chat-head chat-column flex flex-none items-start gap-1 pb-2">
          <UButton
            v-if="isNarrow"
            id="chat_back"
            :icon="icon('back')"
            :aria-label="t('inbox.back')"
            color="neutral"
            variant="ghost"
            size="sm"
            @click="back"
          />
          <div class="chat-head-main min-w-0 flex-1">
            <h1
              id="conversation_title"
              class="truncate text-base font-semibold"
            >
              {{ title }}
            </h1>
            <p
              v-if="inbox.subtitleError"
              id="chat_subtitle"
              class="chat-subtitle text-xs text-error"
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
                class="chat-area text-muted"
              >{{ fmt("inbox.area", { area: info.area }) }}</span>
            </p>
          </div>
          <UPopover
            v-if="info"
            v-model:open="inbox.infoOpen"
            :content="{ align: 'end' }"
          >
            <UButton
              id="chat_info"
              :icon="icon('info')"
              :aria-label="t('inbox.info')"
              :title="t('inbox.info')"
              color="neutral"
              variant="ghost"
              size="sm"
            />
            <template #content>
              <div class="chat-info-panel flex w-72 max-w-[calc(100vw-2rem)] flex-col gap-2 p-4">
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
                      class="member-note text-xs text-muted"
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
                    class="empty text-muted"
                  >
                    {{ t("inbox.info.no_sessions") }}
                  </li>
                </ul>
              </div>
            </template>
          </UPopover>
          <UButton
            v-if="canClose"
            id="chat_close"
            :icon="icon('archive')"
            :aria-label="t('inbox.close')"
            :title="t('inbox.close')"
            color="neutral"
            variant="ghost"
            size="sm"
            :disabled="inbox.closing"
            @click="inbox.confirmClose"
          />
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
              <UCheckbox
                v-for="person in inbox.newChatPeople"
                :id="'new_chat_' + person.name"
                :key="person.name"
                :label="person.name"
                :model-value="inbox.newChatChosen.includes(person.name)"
                class="choice"
                :class="{ on: person.online }"
                :style="{ '--who': whoColor(person.name) }"
                :ui="{ label: 'text-[var(--who)]' }"
                @update:model-value="inbox.newChatChosen = toggle(inbox.newChatChosen, person.name, $event)"
              />
            </span>
          </div>
          <p
            v-if="!inbox.newChatPeople.length"
            id="new_chat_empty"
            class="hint"
          >
            {{ t("inbox.new.no_members") }}
          </p>

          <div class="new-chat-actions flex items-center justify-end gap-2">
            <p
              id="new_chat_result"
              role="status"
              class="mr-auto text-sm text-error"
            >
              {{ inbox.newChatResult }}
            </p>
            <UButton
              id="new_chat_dismiss"
              type="button"
              :label="t('inbox.new.dismiss')"
              color="neutral"
              variant="ghost"
              @click="inbox.hideNewChat"
            />
            <UButton
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
                  class="field-label text-muted"
                >{{ t("inbox.ask.label") }}</span>
                <span
                  id="ask_choices"
                  class="ask-choices flex flex-wrap gap-x-4 gap-y-1"
                >
                  <UCheckbox
                    v-for="name in askNames"
                    :id="'ask_' + name"
                    :key="name"
                    :label="name"
                    :model-value="asked.includes(name)"
                    size="sm"
                    class="choice"
                    :style="{ '--who': whoColor(name) }"
                    :ui="{ label: 'text-[var(--who)]' }"
                    @update:model-value="asked = toggle(asked, name, $event)"
                  />
                </span>
                <span
                  v-if="!asked.length"
                  id="ask_hint"
                  class="ask-hint text-xs text-[var(--app-off)]"
                >{{ t("inbox.ask.none") }}</span>
              </div>
              <!-- Enter starts a new line, Ctrl+Enter sends. -->
              <UChatPrompt
                id="body"
                v-model="inbox.composer"
                as="div"
                name="body"
                :aria-label="t('inbox.body.label')"
                :placeholder="t('inbox.body.placeholder')"
                :rows="1"
                :maxrows="10"
                :autofocus="false"
                :submit-on-enter="false"
                :ui="{ root: 'rounded-3xl px-4', base: 'text-[15px]' }"
                @update:model-value="inbox.saveDraft(inbox.selectedChat)"
                @submit="inbox.submitMessage()"
              >
                <template
                  v-if="inbox.replyTo"
                  #header
                >
                  <p
                    id="replying"
                    class="replying flex w-full items-center gap-2 pt-1 text-xs text-muted"
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
                </template>
                <template #footer>
                  <span />
                  <UButton
                    id="send_button"
                    type="submit"
                    :icon="icon('send')"
                    :aria-label="t('inbox.send')"
                    :disabled="inbox.sending"
                    class="rounded-full"
                  />
                </template>
              </UChatPrompt>
              <p
                id="inbox_result"
                class="composer-result px-1 text-xs text-error"
                role="status"
              >
                {{ inbox.sendResult }}
              </p>
            </form>
            <p
              v-if="note"
              id="chat_note"
              class="chat-note flex flex-wrap items-center gap-2 rounded-xl bg-elevated px-4 py-3 text-sm"
            >
              <span id="chat_note_text">{{ note.text }}</span>
              <UButton
                v-if="note.invite.length"
                id="chat_note_action"
                :label="fmt('inbox.new_with', { names: note.invite.join(', ') })"
                size="sm"
                variant="link"
                @click="inbox.showNewChat(pid, note.invite)"
              />
            </p>
          </div>
        </template>
      </section>
    </div>
  </section>
</template>
