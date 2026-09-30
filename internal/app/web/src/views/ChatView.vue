<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import UButton from '@nuxt/ui/components/Button.vue'
import UChatPrompt from '@nuxt/ui/components/ChatPrompt.vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import UPopover from '@nuxt/ui/components/Popover.vue'
import AttachButton from '@/components/AttachButton.vue'
import ChatTimeline from '@/components/ChatTimeline.vue'
import ComposerAttachments from '@/components/ComposerAttachments.vue'
import { isNarrow } from '@/layout/composables/layout'
import { useClock } from '@/lib/clock'
import { icon } from '@/lib/icons'
import {
  activityLines, keepLastKnown, authorLabel, authorName, chatName, chatSessionList, clock, legacyPeerOld, others, preview,
  when, whoColor, type ActivityLine,
} from '@/lib/chat'
import { activityDot, agentDot, agentTime, countsText, duration, groupAgents, groupStateText, olderPeerNote, olderPeerRow, worstState } from '@/lib/agents'
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

// The chat has no title bar: the project's row in the sidebar names it, and its
// "⋯" menu holds the chat's controls (ProjectMenu.vue). Its thin header holds
// only the agents toolbar: their status and the project's pause.
const inProject = computed(() => !!info.value && !info.value.legacy && pid.value !== 'legacy')
const localChat = computed(() => inProject.value && projects.byID(pid.value)?.scope === 'local')
const projectStopped = computed(() => !!projects.byID(pid.value)?.autonomy?.stopped)
const globallyStopped = computed(() => !!app.status?.stop_all)
const pauseBusy = ref(false)
const pauseError = ref('')
const pauseLabel = computed(() => projectStopped.value
  ? t(globallyStopped.value ? 'inbox.project_pause.remove' : 'inbox.project_pause.resume')
  : t('inbox.project_pause.pause'))
async function toggleProjectPause() {
  if (!inProject.value || pauseBusy.value) return
  pauseBusy.value = true
  pauseError.value = ''
  try { await projects.stopAutonomy(pid.value, !projectStopped.value) }
  catch (error) { pauseError.value = (error as Error).message }
  finally { pauseBusy.value = false }
}
// A project has one chat: it goes by the project's name.
const title = computed(() => {
  const project = projects.byID(pid.value)
  return chatName(info.value, self.value, project?.legacy ? '' : project?.display)
})

// --- live activity, one line per running or queued job ---

// The elapsed timers advance locally, once a second; they never ask the app for anything.
const clockNow = useClock()
const now = computed(() => clockNow.value.getTime())

// lastKnown: each connected member's last running line, kept (plain, not
// reactive) so its row stays as idle after the job is gone.
const lastKnown = new Map<string, ActivityLine>()
const chatSessions = computed(() => chatSessionList(info.value, app.sessions, app.settings))
const pinnedSession = computed(() => app.sessions?.find((session) => session.project === pid.value && session.pinned))
const activity = computed(() => keepLastKnown(activityLines(info.value, inbox.messages, self.value,
  chatSessions.value, app.settings, now.value), info.value, lastKnown))
const agentsOpen = ref(false)
// A running line's time is how long ago its agent was last heard of («12 с
// назад»); the tooltip adds when it took the request. A waiting or queued
// line's time is how long it has waited.
function since(row: ActivityLine) {
  const at = Date.parse(row.heard || row.since)
  if (Number.isNaN(at)) return ''
  const span = duration(now.value - at)
  return row.heard ? fmt("inbox.activity.ago", { t: span }) : span
}
function sinceTitle(row: ActivityLine) {
  if (!row.heard) return ''
  const start = clock(row.since, { offset: true })
  return fmt("inbox.activity.heard_title", { heard: clock(row.heard, { offset: true }) }) + (start ? '\n' + fmt("inbox.activity.since_title", { start }) : '')
}

// --- composer ---

// A legacy chat is writable too: the node continues it in a real chat with the
// peer, or with a plain message when the peer's version has no chats.
const writable = computed(() => !!info.value && !info.value.closed && !info.value.removed)
const replying = computed(() => (inbox.replyTo ? fmt("inbox.replying", { text: authorLabel(inbox.replyTo, self.value) + ': ' + preview(inbox.replyTo.body, 60) }) : ''))

// This member's local agents (seats), shown in the agents popover.
watch(() => [pid.value, inProject.value] as const, ([p, on]) => {
  if (on && p && !Object.hasOwn(projects.seats, p)) void projects.refreshSeats(p).catch(() => {})
}, { immediate: true })
const seatList = computed(() => (inProject.value ? projects.seats[pid.value] || [] : []))
const agentMembers = computed(() => (projects.byID(pid.value)?.members || [])
  .filter((member) => (member.self || member.online) && (member.agent || !!member.agents?.length ||
    Object.values(member.agent_counts || {}).some((count) => count > 0)))
  .map((member) => ({
    name: member.display || member.name, key: member.name, counts: member.agent_counts, agents: member.agents, self: !!member.self,
    online: member.online, seen: member.seen, app: member.app,
  })))
const paused = computed(() => projectStopped.value || globallyStopped.value)

// The agents popover: one row per agent — whose it is, what it does now and
// since when (members' agent lists, this computer's first). A member without
// one (an older peer) shows its chat's live job lines, else its counts; this
// computer without one, its seats.
interface AgentRow {
  key: string; dot: string; name: string; state: string; time?: string; title?: string; sub?: boolean; kind: string; who?: string
  line?: string // an older peer's chat line: its kind (running, stale, presence…)
  note?: string; noteTitle?: string // an older peer's: update it to see its agents one by one
}
const SEAT_STATE: Record<string, string> = {
  running: 'thinking', active: 'thinking', idle: 'idle', closed: 'off', stopped: 'stopped', busy: 'idle', needs_human: 'needs_human', paused: 'paused',
}
const agentRows = computed<AgentRow[]>(() => {
  const rows: AgentRow[] = []
  const covered = new Set<string>()
  const who = (name: string) => whoColor(name, projects.colorOf(pid.value, name))
  for (const member of [...agentMembers.value].sort((a, b) => Number(b.self) - Number(a.self))) {
    if (!member.agents) continue
    covered.add(member.key)
    // One row per agent: its sessions under one name, by the most active.
    const agents = paused.value && member.self ? member.agents.map((a) => ({ ...a, state: 'paused' })) : member.agents
    for (const g of groupAgents(agents, member.name, member.self)) {
      const { time, title } = agentTime(g.agent, now.value)
      rows.push({
        key: 'agent\n' + member.key + '\n' + g.name, kind: 'agent', dot: agentDot(g.agent.state), name: g.name,
        state: groupStateText(g), time, title, who: member.self ? undefined : who(member.key),
      })
    }
  }
  const self = agentMembers.value.find((member) => member.self)
  if (!self?.agents) {
    for (const seat of seatList.value) {
      const state = paused.value ? 'paused' : SEAT_STATE[seat.status] || 'idle'
      rows.push({ key: 'seat\n' + seat.id, kind: 'seat', dot: agentDot(state), name: seat.label, state: t('inbox.agents.doing.' + state) })
    }
  }
  // A job line (its key starts with the member) is an agent at work; the
  // waiting and delivery lines (keys starting with a newline) are about messages.
  const jobs = activity.value.filter((row) => !row.key.startsWith('\n'))
  for (const row of activity.value) {
    if (covered.has(row.name)) continue
    const job = jobs.includes(row)
    for (const line of [row, ...(row.children || [])]) {
      rows.push({
        key: line.key, kind: 'activity', dot: activityDot(line.cls), line: line.cls, name: line.cls === 'presence' ? line.name : line.who, state: line.text,
        time: since(line) || '—', title: sinceTitle(line), sub: line !== row, who: who(line.name),
        note: job && line === row && row.name !== self?.key ? olderPeerNote(row.name) : undefined,
      })
    }
  }
  // A member already on a job line, or this computer by its seats, is not listed again.
  const listed = new Set(jobs.map((row) => row.name))
  for (const member of agentMembers.value) {
    if (covered.has(member.key) || (member.self && seatList.value.length) || listed.has(member.key)) continue
    if (member.self) {
      rows.push({ key: 'member\n' + member.key, kind: 'member', dot: paused.value ? 'paused' : 'idle', name: member.name, state: countsText(member.counts) })
      continue
    }
    rows.push({ key: 'member\n' + member.key, kind: 'member', name: member.name, who: who(member.key), ...olderPeerRow(member, now.value) })
  }
  return rows
})
// The button counts the agents at work; its dot shows the worst state (grey:
// an agent with no session open, yellow: one idle, green: all at work).
const agentTotal = computed(() => agentRows.value.filter((row) => !row.sub && row.dot === 'running').length)
const agentCount = computed(() => agentRows.value.filter((row) => !row.sub).length)
const agentsState = computed(() => paused.value ? 'paused' : worstState(agentRows.value.map((row) => row.dot)))
// A network project where no computer (this one included) has Claude Code or
// Codex open: messages to agents wait, so say so with the next step. A pause
// already explains itself.
const noAgents = computed(() => {
  const project = projects.byID(pid.value)
  return inProject.value && !localChat.value && project?.state === 'ready' && !!project.members?.length &&
    !agentMembers.value.length && !projectStopped.value && !globallyStopped.value
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
  // A closed chat of two continues with the other side (the network from before projects).
  const rest = others(i, self.value)
  return { text: fmt("inbox.closed_note", closed), invite: rest.length === 1 ? rest : [] }
})

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
        <!-- The chat's thin header: back (narrow screens) and the pinned thread.
             The agents status and pause live in the composer box. -->
        <header
          v-if="isNarrow || pinnedSession"
          id="chat_header"
          class="chat-column chat-topbar flex flex-none items-center gap-2"
        >
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
          <span
            v-if="pinnedSession"
            id="chat_pinned_session"
            class="chat-pinned"
            role="status"
          >{{ fmt('inbox.session_pin', { id: pinnedSession.session_id || '' }) }}</span>
        </header>
        <p
          v-if="inProject && paused"
          id="chat_pause_banner"
          class="chat-column chat-pause-banner flex-none"
          role="status"
        >
          <UIcon
            :name="icon('pause')"
            class="size-3.5 flex-none"
          />
          <span>{{ t(globallyStopped ? 'inbox.project_pause.global' : 'inbox.project_pause.banner') }}</span>
        </p>
        <p
          v-if="pauseError"
          id="chat_pause_error"
          class="chat-column flex-none text-xs text-error"
          role="status"
        >
          {{ pauseError }}
        </p>
        <p
          v-if="inbox.subtitleError"
          id="chat_error"
          class="chat-column flex-none pb-2 text-xs text-error"
          role="status"
        >
          {{ inbox.subtitleError }}
        </p>
        <p
          v-if="noAgents"
          id="chat_no_agents"
          class="chat-column flex-none pb-2 text-xs text-warning"
          role="status"
        >
          {{ t('inbox.no_agents') }}
        </p>

        <ChatTimeline />
        <div class="chat-column flex flex-none flex-col gap-2 pb-4">
          <!-- A local Claude/Codex chat is the agents' own: a person reads it,
               nothing more (no composer, no pause, no reply). -->
          <p
            v-if="localChat"
            id="chat_readonly"
            class="chat-note flex items-center gap-2 rounded-xl bg-elevated px-4 py-3 text-sm text-muted"
            role="note"
          >
            <UIcon
              :name="icon('agent')"
              class="size-4 flex-none"
            />
            <span>{{ t('local_chat.readonly') }}</span>
          </p>
          <form
            v-else-if="writable"
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
            <ComposerAttachments />
            <p
              v-if="dragging"
              id="drop_hint"
              class="drop-hint px-1 text-xs text-muted"
            >
              {{ t("inbox.attach.drop") }}
            </p>
            <p
              v-if="inbox.replyTo"
              id="replying"
              class="replying flex w-full items-center gap-2 px-3 text-xs text-muted"
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
            <!-- One row: the paperclip, the text (it grows up to ten lines),
                 the keys hint, the agents status and pause, and the send button. Enter sends, Shift+Enter
                 starts a new line; an IME composition and a blank message
                 never send (UChatPrompt guards both), and the button is off
                 while there is nothing to send. -->
            <UChatPrompt
              id="body"
              v-model="inbox.composer"
              as="div"
              name="body"
              class="composer-box flex-row items-end gap-1.5 rounded-3xl px-2 py-1.5"
              variant="naked"
              :aria-label="t('inbox.body.label')"
              :placeholder="t('inbox.body.placeholder')"
              :rows="1"
              :maxrows="10"
              :autofocus="false"
              :ui="{ header: 'flex-none pb-0.5', body: 'min-w-0 flex-1 self-center w-full', base: 'text-[15px] py-1.5', footer: 'flex-none gap-2 pb-0.5' }"
              @update:model-value="inbox.saveDraft(inbox.openKey())"
              @submit="inbox.submitMessage()"
            >
              <template #header>
                <AttachButton :project="pid" />
              </template>
              <template #footer>
                <span
                  id="composer_hint"
                  class="composer-hint hidden text-xs sm:inline"
                >{{ t("inbox.body.hint") }}</span>
                <div
                  v-if="inProject || agentRows.length"
                  id="chat_toolbar"
                  class="chat-toolbar"
                  role="toolbar"
                  :aria-label="t('inbox.agents.title')"
                >
                  <UPopover
                    v-model:open="agentsOpen"
                    :content="{ side: 'top', align: 'end', sideOffset: 8, collisionPadding: 8 }"
                  >
                    <button
                      id="chat_agents_toggle"
                      type="button"
                      class="panel-icon chat-tool"
                      :class="{ active: agentsOpen }"
                      :data-state="agentsState"
                      :title="t('inbox.agents.title')"
                      :aria-label="fmt('inbox.agents.button', { n: agentCount, w: agentTotal })"
                    >
                      <UIcon
                        :name="icon('agent')"
                        class="size-[1.1rem]"
                      />
                      <span
                        class="chat-tool-dot"
                        :class="agentsState"
                        aria-hidden="true"
                      />
                      <span
                        v-if="agentTotal"
                        class="chat-tool-count"
                        aria-hidden="true"
                      >{{ agentTotal }}</span>
                    </button>
                    <template #content>
                      <div
                        id="chat_agents_popover"
                        class="agents-pop"
                        role="region"
                        :aria-label="t('inbox.agents.title')"
                      >
                        <p class="agents-pop-title">
                          {{ t('inbox.agents.title') }}
                        </p>
                        <ul
                          v-if="agentRows.length"
                          id="chat_agents"
                          class="agents-list"
                        >
                          <li
                            v-for="row in agentRows"
                            :key="row.key"
                            class="agent-row"
                            :class="[row.dot, row.line, { sub: row.sub }]"
                            :data-kind="row.kind"
                            :style="row.who ? { '--who': row.who } : undefined"
                          >
                            <span
                              class="agent-dot"
                              aria-hidden="true"
                            />
                            <span class="agent-name">{{ row.name }}</span>
                            <span
                              class="agent-state"
                              :title="row.state"
                            >{{ row.state }}</span>
                            <span
                              class="agent-time"
                              :title="row.title || undefined"
                            >{{ row.time || '' }}</span>
                            <span
                              v-if="row.note"
                              class="agent-note"
                              :title="row.noteTitle || undefined"
                            >{{ row.note }}</span>
                          </li>
                        </ul>
                        <p
                          v-else
                          id="chat_agents_empty"
                          class="agents-empty"
                        >
                          {{ t('inbox.agents.empty') }}
                        </p>
                      </div>
                    </template>
                  </UPopover>
                  <button
                    v-if="inProject"
                    id="chat_agent_pause"
                    type="button"
                    class="panel-icon chat-tool"
                    :class="{ paused: projectStopped }"
                    :title="pauseLabel"
                    :aria-label="pauseLabel"
                    :aria-pressed="projectStopped ? 'true' : 'false'"
                    :disabled="pauseBusy"
                    @click="toggleProjectPause"
                  >
                    <UIcon
                      :name="icon(projectStopped ? 'play' : 'pause')"
                      class="size-[1.1rem]"
                    />
                  </button>
                </div>
                <UButton
                  id="send_button"
                  type="submit"
                  :icon="icon('send')"
                  :aria-label="t('inbox.send')"
                  :title="t('inbox.send')"
                  :disabled="!inbox.canSend()"
                  class="rounded-full disabled:opacity-35"
                />
              </template>
            </UChatPrompt>
            <p
              v-if="inbox.sendResult || files.error"
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
              @click="inbox.openPeer(pid, note.invite[0]!)"
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
