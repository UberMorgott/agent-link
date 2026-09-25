<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import UButton from '@nuxt/ui/components/Button.vue'
import UChatPrompt from '@nuxt/ui/components/ChatPrompt.vue'
import UCheckbox from '@nuxt/ui/components/Checkbox.vue'
import AttachButton from '@/components/AttachButton.vue'
import ChatTimeline from '@/components/ChatTimeline.vue'
import ComposerAttachments from '@/components/ComposerAttachments.vue'
import { isNarrow } from '@/layout/composables/layout'
import { icon } from '@/lib/icons'
import {
  activityLines, keepLastKnown, authorLabel, authorName, chatName, chatSessionList, clock, elapsed, legacyPeerOld, others, preview,
  when, whoColor, type ActivityLine,
} from '@/lib/chat'
import { openProject } from '@/lib/nav'
import { fmt, t } from '@/lib/runtime'
import { pastedFiles } from '@/lib/attachments'
import { useAppStore } from '@/stores/app'
import { useAttachmentsStore } from '@/stores/attachments'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'

const app = useAppStore()
const inbox = useInboxStore()
const projects = useProjectsStore()
const route = useRoute()

const pid = computed(() => String(route.params.project || ''))

// Every visit of /ui/p/{pid}/c/{chat} opens what it names.
watch(() => route.fullPath, () => {
  if (route.name !== 'chat') return
  const message = route.query.message
  projects.open(pid.value)
  void inbox.selectChat(pid.value, String(route.params.chat || ''), typeof message === 'string' ? message : '')
}, { immediate: true })

const info = computed(() => inbox.chat)
const self = computed(() => app.self)

// The chat has no header: the project's row in the sidebar names it, and its
// "⋯" menu holds the chat's controls (ProjectMenu.vue).
const inProject = computed(() => !!info.value && !info.value.legacy && pid.value !== 'legacy')
// A project has one chat: it goes by the project's name.
const title = computed(() => {
  const project = projects.byID(pid.value)
  return chatName(info.value, self.value, project?.legacy ? '' : project?.display)
})

// --- live activity, one line per running or queued job ---

const now = ref(Date.now())
let ticker: ReturnType<typeof setInterval> | undefined
// The elapsed timers advance locally, once a second; they never ask the app for anything.
onMounted(() => { ticker = setInterval(() => { now.value = Date.now() }, 1000) })
onBeforeUnmount(() => clearInterval(ticker))

// lastKnown: each connected member's last running line, kept (plain, not
// reactive) so its row stays as idle after the job is gone.
const lastKnown = new Map<string, ActivityLine>()
const activity = computed(() => keepLastKnown(activityLines(info.value, inbox.messages, self.value,
  chatSessionList(info.value, app.sessions, app.settings), app.settings, now.value), info.value, lastKnown))
// A running line's time is how long ago its agent was last heard of («0:12
// назад»); the tooltip adds when it took the request. A waiting or queued
// line's time is how long it has waited.
function since(row: ActivityLine) {
  const at = Date.parse(row.heard || row.since)
  if (Number.isNaN(at)) return ''
  const span = elapsed(now.value - at)
  return '· ' + (row.heard ? fmt("inbox.activity.ago", { t: span }) : span)
}
function sinceTitle(row: ActivityLine) {
  if (!row.heard) return ''
  const start = clock(row.since)
  return fmt("inbox.activity.heard_title", { heard: clock(row.heard) }) + (start ? '\n' + fmt("inbox.activity.since_title", { start }) : '')
}

// --- composer ---

// A legacy chat is writable too: the node continues it in a real chat with the
// peer, or with a plain message when the peer's version has no chats.
const writable = computed(() => !!info.value && !info.value.closed && !info.value.removed)
const askNames = computed(() => others(info.value, self.value))
const asked = computed<string[]>({
  get: () => (info.value ? inbox.askFor(info.value) : []),
  set: (names) => { if (info.value) inbox.setAsk(info.value, names) },
})
const replying = computed(() => (inbox.replyTo ? fmt("inbox.replying", { text: authorLabel(inbox.replyTo, self.value) + ': ' + preview(inbox.replyTo.body, 60) }) : ''))

// This member's local agents (seats) the message asks: none by default.
watch(() => [pid.value, inProject.value] as const, ([p, on]) => {
  if (on && p && !Object.hasOwn(projects.seats, p)) void projects.refreshSeats(p).catch(() => {})
}, { immediate: true })
const seatList = computed(() => (inProject.value ? projects.seats[pid.value] || [] : []))
const seatAsked = computed<string[]>({
  get: () => (info.value ? inbox.seatAskFor(info.value) : []),
  set: (ids) => { if (info.value) inbox.setSeatAsk(info.value, ids) },
})

const note = computed(() => {
  const i = info.value
  if (!i || (writable.value && !i.legacy)) return null
  if (i.removed && !i.closed) return { text: t("inbox.removed_note"), invite: [] as string[] }
  if (i.legacy) {
    let text = fmt(legacyPeerOld(i) ? "inbox.legacy_note_old" : "inbox.legacy_note", { name: i.peer || '' })
    if (i.archived && i.closed_by) {
      text = fmt("inbox.legacy_closed_note", { name: authorName(i.closed_by, self.value), when: i.closed_at ? when(i.closed_at) : '' }) + ' ' + text
    }
    return { text, invite: [] as string[] }
  }
  const closed = { name: authorName(i.closed_by || '', self.value), when: i.closed_at ? when(i.closed_at) : '' }
  // A project's archived history: the conversation goes on in its one chat.
  if (inProject.value) return { text: fmt("inbox.archived_note", closed), invite: [] as string[], current: true }
  return { text: fmt("inbox.closed_note", closed), invite: others(i, self.value) }
})

// One checkbox per person: a name in the list is a ticked box.
function toggle(names: string[], name: string, on: boolean | 'indeterminate'): string[] {
  const rest = names.filter((n) => n !== name)
  return on === true ? [...rest, name] : rest
}

watch(() => inbox.focusComposer, () => nextTick(() => document.getElementById('body')?.focus()))

// Files go onto the message by paste (a screenshot), drop or the paperclip;
// another chat starts without them.
const files = useAttachmentsStore()
const dragging = ref(false)
watch(() => inbox.openKey(), () => files.clear())
function onPaste(event: ClipboardEvent) {
  const list = pastedFiles(event.clipboardData)
  if (!list.length) return // text pastes as usual
  event.preventDefault()
  files.add(pid.value, list)
}
function onDrop(event: DragEvent) {
  dragging.value = false
  const list = Array.from(event.dataTransfer?.files || [])
  if (list.length) files.add(pid.value, list)
}


function back() {
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
        <!-- For screen readers only: the project's row names the chat on screen. -->
        <h1
          id="conversation_title"
          class="sr-only"
        >
          {{ title }}
        </h1>
        <div
          v-if="isNarrow"
          class="chat-column flex flex-none items-center pb-2"
        >
          <UButton
            id="chat_back"
            :icon="icon('back')"
            :aria-label="t('inbox.back')"
            color="neutral"
            variant="ghost"
            size="sm"
            @click="back"
          />
        </div>
        <p
          v-if="inbox.subtitleError"
          id="chat_error"
          class="chat-column flex-none pb-2 text-xs text-error"
          role="status"
        >
          {{ inbox.subtitleError }}
        </p>

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
              class="act-node"
              :class="row.cls"
              :style="{ '--who': whoColor(row.name) }"
            >
              <div
                v-for="line in [row, ...(row.children || [])]"
                :key="line.key"
                class="act-row"
                :class="[line.cls, { 'act-sub': line !== row }]"
              >
                <span
                  class="act-spin"
                  aria-hidden="true"
                />
                <strong class="act-who">{{ line.who }}</strong>
                <span
                  class="act-text"
                  :title="line.text"
                >{{ line.text }}</span>
                <span
                  class="act-time"
                  :title="sinceTitle(line)"
                >{{ since(line) }}</span>
              </div>
            </li>
          </ul>
          <form
            v-if="writable"
            id="send"
            class="composer flex flex-col gap-2"
            :aria-busy="inbox.sending ? 'true' : undefined"
            :class="{ dragging }"
            @submit.prevent="inbox.submitMessage"
            @paste="onPaste"
            @dragover.prevent="dragging = true"
            @dragleave="dragging = false"
            @drop.prevent="onDrop"
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
            <div
              v-if="seatList.length"
              id="seat_row"
              class="ask-row flex flex-wrap items-center gap-x-4 gap-y-1 px-1 text-sm"
              role="group"
              aria-labelledby="seat_label"
            >
              <span
                id="seat_label"
                class="field-label text-muted"
              >{{ t("inbox.seats.label") }}</span>
              <span class="ask-choices flex flex-wrap gap-x-4 gap-y-1">
                <UCheckbox
                  v-for="s in seatList"
                  :id="'seat_' + s.id"
                  :key="s.id"
                  :label="s.label"
                  :title="t('project.agents.status.' + s.status)"
                  :model-value="seatAsked.includes(s.id)"
                  size="sm"
                  class="choice"
                  @update:model-value="seatAsked = toggle(seatAsked, s.id, $event)"
                />
              </span>
            </div>
            <ComposerAttachments />
            <p
              v-if="dragging"
              id="drop_hint"
              class="drop-hint px-1 text-xs text-muted"
            >
              {{ t("inbox.attach.drop") }}
            </p>
            <!-- Enter sends, Shift+Enter starts a new line; an IME composition
                 and a blank message never send (UChatPrompt guards both). -->
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
              :ui="{ root: 'rounded-3xl px-4', base: 'text-[15px]' }"
              @update:model-value="inbox.saveDraft(inbox.openKey())"
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
                <span
                  id="composer_hint"
                  class="composer-hint text-xs text-[var(--app-off)]"
                >{{ t("inbox.body.hint") }}</span>
                <AttachButton :project="pid" />
                <UButton
                  id="send_button"
                  type="submit"
                  :icon="icon('send')"
                  :aria-label="t('inbox.send')"
                  :disabled="inbox.sending || files.uploading"
                  class="rounded-full"
                />
              </template>
            </UChatPrompt>
            <p
              id="inbox_result"
              class="composer-result px-1 text-xs text-error"
              role="status"
            >
              {{ inbox.sendResult || files.error }}
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
            <UButton
              v-if="note.current"
              id="chat_note_current"
              :label="t('inbox.archived_note.open')"
              size="sm"
              variant="link"
              @click="inbox.openActive(pid)"
            />
          </p>
        </div>
      </section>
    </div>
  </section>
</template>
