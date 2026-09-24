<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import {
  authorLabel, authorName, clock, continues, genitiveName, isAgent, messageTick, others, preview, when, whoColor,
} from '@/lib/chat'
import { fmt, t } from '@/lib/runtime'
import UButton from '@nuxt/ui/components/Button.vue'
import UChatMessage from '@nuxt/ui/components/ChatMessage.vue'
import UChatMessages from '@nuxt/ui/components/ChatMessages.vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import { icon } from '@/lib/icons'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'
import type { ChatMessage } from '@/types'

const app = useAppStore()
const inbox = useInboxStore()
const list = ref<HTMLElement | null>(null)

interface Bubble {
  m: ChatMessage
  event: boolean
  out: boolean
  cont: boolean
  cls: string
  who: string
  icon: string
  author: string
  fyi: string
  quote: string
  note: string
  tick: { state: string; label: string } | null
  canReply: boolean
}

const bubbles = computed<Bubble[]>(() => {
  const self = app.self
  const info = inbox.chat
  const msgs = inbox.messages
  const byID = new Map(msgs.map((m) => [m.id, m]))
  return msgs.map((m, i) => {
    if (m.kind === 'chat_open' || m.kind === 'chat_close') {
      return {
        m, event: true, out: false, cont: false, cls: 'msg-event', who: '', icon: '', fyi: '', quote: '', note: '', tick: null, canReply: false,
        author: m.from === self
          ? t(m.kind === 'chat_open' ? "inbox.event.open_self" : "inbox.event.close_self")
          : fmt(m.kind === 'chat_open' ? "inbox.event.open" : "inbox.event.close", { name: authorName(m.from, self) }),
      }
    }
    const agent = isAgent(m)
    const out = m.direction === 'out'
    const cont = continues(msgs[i - 1], m)
    const parent = m.reply_to ? byID.get(m.reply_to) : undefined
    // Whom a group message asks; in a chat of two it is always the other side.
    const asks = (m.responders || []).filter((name) => name !== m.from)
    const tick = messageTick(m, info)
    return {
      m, event: false, out, cont,
      cls: 'msg ' + (out ? 'out' : 'in') + (agent ? ' agent' : ' human') + (cont ? ' cont' : ''),
      who: whoColor(m.from),
      icon: agent ? 'agent' : 'human',
      author: authorLabel(m, self),
      // A person's own message the local agent has not seen yet: it gets it as
      // information, not as a request.
      fyi: m.own_human && m.unread ? t("inbox.author.fyi") : '',
      quote: parent ? fmt("inbox.reply_to", { name: genitiveName(parent.from, self), text: preview(parent.body, 70) }) : '',
      note: asks.length && others(info, self).length > 1 ? fmt("inbox.asks", { names: asks.map((n) => genitiveName(n, self)).join(', ') }) : '',
      tick,
      // Own messages get no reply button: a reference to oneself asks nobody.
      canReply: !!info && !info.legacy && !info.closed && !out,
    }
  })
})

function node(id: string): HTMLElement | null {
  return list.value?.querySelector<HTMLElement>('[data-message-id="' + CSS.escape(id) + '"]') ?? null
}

function reveal(id: string | undefined) {
  const target = id ? node(id) : null
  if (!target) return
  target.focus({ preventScroll: true })
  target.scrollIntoView({ block: 'center' })
}

// The view follows new messages only when it was already at the bottom;
// otherwise it stays where it was.
let snapshot = { nearBottom: true, top: 0, height: 0 }
let seenIntent = 0
const source = () => [inbox.messages, inbox.scrollIntent] as const
watch(source, () => {
  const el = list.value
  if (el) snapshot = { nearBottom: el.scrollHeight - el.clientHeight - el.scrollTop <= 80, top: el.scrollTop, height: el.scrollHeight }
}, { flush: 'pre' })
watch(source, () => {
  const el = list.value
  if (!el) return
  const intent = inbox.scrollIntent
  const kind = intent.n !== seenIntent ? intent.kind : 'auto'
  seenIntent = intent.n
  const anchor = inbox.selectedMessage
  if (anchor && node(anchor)) {
    reveal(anchor)
    inbox.selectedMessage = ''
  } else if (kind === 'prepended') el.scrollTop = snapshot.top + (el.scrollHeight - snapshot.height)
  else if (kind === 'reset' || snapshot.nearBottom) el.scrollTop = el.scrollHeight
  else el.scrollTop = snapshot.top
}, { flush: 'post' })
</script>

<template>
  <!-- #messages is the scroll box; ChatMessages finds it as its scroll parent
       and offers the jump to the newest message when scrolled up. -->
  <div class="relative flex min-h-0 flex-1 flex-col">
    <div
      id="messages"
      ref="list"
      class="conversation-timeline min-h-0 flex-1 overflow-y-auto py-4"
    >
      <div class="chat-column">
        <p
          v-if="!inbox.messages.length"
          class="empty py-10 text-center text-sm text-muted"
        >
          {{ t("inbox.empty_conversation") }}
        </p>
        <div
          v-else-if="inbox.hasOlder"
          class="msg-older flex justify-center pb-4"
        >
          <UButton
            :label="t('inbox.older')"
            color="neutral"
            variant="ghost"
            size="sm"
            @click="inbox.loadOlder"
          />
        </div>
        <UChatMessages
          v-if="inbox.messages.length"
          compact
          :should-scroll-to-bottom="false"
          :ui="{ root: 'px-0' }"
        >
          <template
            v-for="b in bubbles"
            :key="b.m.id"
          >
            <div
              v-if="b.event"
              :class="b.cls"
              :data-message-id="b.m.id"
              tabindex="-1"
            >
              <strong class="msg-author">{{ b.author }}</strong>
              <time
                class="msg-time"
                :datetime="b.m.created_at"
              >{{ clock(b.m.created_at) }}</time>
            </div>
            <UChatMessage
              v-else
              :id="b.m.id"
              :role="b.out ? 'user' : 'assistant'"
              :parts="[]"
              :side="b.out ? 'right' : 'left'"
              :variant="b.out ? 'soft' : 'naked'"
              color="neutral"
              compact
              :class="b.cls"
              :ui="{ content: b.out ? 'px-3.5 py-2 rounded-2xl' : '', container: b.cont ? 'pb-1.5' : 'pb-3' }"
              :style="{ '--who': b.who }"
              :data-message-id="b.m.id"
              tabindex="-1"
            >
              <template
                v-if="!b.cont"
                #header
              >
                <div class="msg-head">
                  <UIcon
                    :name="icon(b.icon)"
                    class="msg-icon"
                  />
                  <strong class="msg-author">{{ b.author }}</strong>
                  <span
                    v-if="b.fyi"
                    class="msg-fyi"
                  >{{ b.fyi }}</span>
                </div>
              </template>
              <template #content>
                <button
                  v-if="b.quote"
                  type="button"
                  class="msg-quote"
                  @click="reveal(b.m.reply_to)"
                >
                  {{ b.quote }}
                </button>
                <pre class="msg-body">{{ b.m.body || "" }}</pre>
                <div class="msg-foot">
                  <span
                    v-if="b.note"
                    class="msg-note"
                  >{{ b.note }}</span>
                  <time
                    class="msg-time"
                    :datetime="b.m.created_at"
                    :title="when(b.m.created_at)"
                  >{{ clock(b.m.created_at) }}</time>
                  <span
                    v-if="b.tick"
                    class="msg-ticks"
                    :class="b.tick.state"
                    role="img"
                    :title="b.tick.label"
                    :aria-label="b.tick.label.replace(/\n/g, '; ')"
                  >
                    <UIcon
                      :name="icon(b.tick.state)"
                      class="msg-tick-icon"
                    />
                  </span>
                  <button
                    v-if="b.canReply"
                    type="button"
                    class="msg-reply"
                    @click="inbox.setReply(b.m)"
                  >
                    {{ t("inbox.reply") }}
                  </button>
                </div>
              </template>
            </UChatMessage>
          </template>
        </UChatMessages>
      </div>
    </div>
  </div>
</template>
