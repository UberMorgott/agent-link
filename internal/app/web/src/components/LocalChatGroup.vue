<script setup lang="ts">
import { computed } from 'vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import ProjectMenu from '@/components/ProjectMenu.vue'
import { icon } from '@/lib/icons'
import { fmt, t } from '@/lib/runtime'
import type { ProjectView } from '@/types'

// One project of the local chats section: a plain row while it has one chat,
// else a row that opens and closes the list of its chats (a disclosure:
// Enter/Space toggle it, ArrowRight opens, ArrowLeft closes).
export interface LocalChatRow {
  p: ProjectView
  name: string
  dot: string
  dotLabel: string
  unread: number
  active: boolean
  // live: 'waiting' (someone waits for a reply), 'live', or '' (not a
  // temporary chat, or no live flag).
  live: '' | 'live' | 'waiting'
}

const props = defineProps<{ groupKey: string; name: string; rows: LocalChatRow[]; expanded: boolean }>()
const emit = defineEmits<{ open: [pid: string]; toggle: [on: boolean] }>()

const single = computed(() => props.rows.length === 1)
// A group holding the chat on screen shows it, whatever was saved.
const open = computed(() => props.expanded || props.rows.some((r) => r.active))
const unread = computed(() => props.rows.reduce((n, r) => n + r.unread, 0))
const live = computed(() => (props.rows.some((r) => r.live === 'waiting') ? 'waiting' : props.rows.some((r) => r.live) ? 'live' : ''))
const listID = computed(() => 'local_group_' + (props.groupKey || 'none'))

function liveLabel(state: string): string {
  return state === 'waiting' ? t('local_chat.waiting') : t('local_chat.live')
}
</script>

<template>
  <li
    v-if="single"
    :data-project="rows[0]!.p.id"
  >
    <div
      class="project-row"
      :class="{ active: rows[0]!.active }"
    >
      <button
        type="button"
        class="project-open"
        :aria-current="rows[0]!.active ? 'page' : undefined"
        @click="emit('open', rows[0]!.p.id)"
      >
        <span
          v-if="rows[0]!.live"
          class="chat-live"
          :class="rows[0]!.live"
          :data-live="rows[0]!.live"
          :title="liveLabel(rows[0]!.live)"
          role="img"
          :aria-label="liveLabel(rows[0]!.live)"
        />
        <span
          v-else
          class="project-dot"
          :class="rows[0]!.dot"
          :data-dot="rows[0]!.dot"
          :title="rows[0]!.dotLabel"
          role="img"
          :aria-label="rows[0]!.dotLabel.replace(/\n/g, '; ')"
        />
        <span class="project-name">{{ rows[0]!.name }}</span>
        <span
          v-if="rows[0]!.unread"
          class="project-unread"
          :title="t('inbox.unread')"
        >{{ rows[0]!.unread }}</span>
      </button>
      <ProjectMenu
        :project="rows[0]!.p"
        :name="rows[0]!.name"
      />
    </div>
  </li>
  <li
    v-else
    class="local-group"
    :data-group="groupKey"
  >
    <div class="project-row">
      <button
        type="button"
        class="project-open local-group-head"
        :aria-expanded="open"
        :aria-controls="listID"
        :title="fmt('local_chat.count', { n: rows.length })"
        @click="emit('toggle', !open)"
        @keydown.right.prevent="emit('toggle', true)"
        @keydown.left.prevent="emit('toggle', false)"
      >
        <UIcon
          :name="icon(open ? 'expand' : 'fold')"
          class="size-3.5 flex-none text-muted"
        />
        <span class="project-name">{{ name }}</span>
        <span
          v-if="live"
          class="chat-live"
          :class="live"
          :title="liveLabel(live)"
          role="img"
          :aria-label="liveLabel(live)"
        />
        <span
          v-if="unread"
          class="project-unread"
          :title="t('inbox.unread')"
        >{{ unread }}</span>
        <span
          class="local-group-count"
          :aria-label="fmt('local_chat.count', { n: rows.length })"
        >{{ rows.length }}</span>
      </button>
    </div>
    <ul
      v-show="open"
      :id="listID"
      class="local-group-items"
      role="group"
      :aria-label="name"
    >
      <li
        v-for="r in rows"
        :key="r.p.id"
        :data-project="r.p.id"
      >
        <div
          class="project-row"
          :class="{ active: r.active }"
        >
          <button
            type="button"
            class="project-open"
            :aria-current="r.active ? 'page' : undefined"
            @click="emit('open', r.p.id)"
          >
            <span
              v-if="r.live"
              class="chat-live"
              :class="r.live"
              :data-live="r.live"
              :title="liveLabel(r.live)"
              role="img"
              :aria-label="liveLabel(r.live)"
            />
            <span
              v-else
              class="project-dot"
              :class="r.dot"
              :data-dot="r.dot"
              :title="r.dotLabel"
              role="img"
              :aria-label="r.dotLabel.replace(/\n/g, '; ')"
            />
            <span class="project-name">{{ r.name }}</span>
            <span
              v-if="r.unread"
              class="project-unread"
              :title="t('inbox.unread')"
            >{{ r.unread }}</span>
          </button>
          <ProjectMenu
            :project="r.p"
            :name="r.name"
          />
        </div>
      </li>
    </ul>
  </li>
</template>
