<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import UInput from '@nuxt/ui/components/Input.vue'
import UPopover from '@nuxt/ui/components/Popover.vue'
import ThemeSettings from '@/layout/ThemeSettings.vue'
import { api } from '@/lib/api'
import { CHAT_COLORS, whoColor, whoName } from '@/lib/chat'
import { fmt, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import type { Status } from '@/types'

// The member's account button at the foot of the sidebar: its nickname in its
// chat color. It opens one menu: the name and the connection, the profile (the
// nickname and the color the other members see on its messages and its agents'
// messages, edited in place) and the appearance of this browser. A profile
// save applies at once in every project (POST profile); the name itself stays
// the member's identity.
const app = useAppStore()
const open = ref(false)
const nickname = ref('')
const color = ref('')
const busy = ref(false)
const result = ref('')
const pane = ref<HTMLElement | null>(null)

const shown = computed(() => app.status?.nickname || app.self)
const mine = computed(() => whoColor(app.self, app.status?.chat_color || ''))
const saved = computed(() => whoName(app.self, app.status?.chat_color || ''))

// Every opening starts from what is saved.
watch(open, (now) => {
  if (!now) return
  nickname.value = shown.value
  color.value = saved.value
  result.value = ''
})

// Arrow keys, Home and End move between the menu's controls.
function nav(e: KeyboardEvent) {
  const keys = ['ArrowDown', 'ArrowUp', 'Home', 'End']
  if (!keys.includes(e.key) || (e.target as HTMLElement).tagName === 'INPUT') return
  const items = Array.from(pane.value?.querySelectorAll<HTMLElement>('button:not([disabled])') || [])
  if (!items.length) return
  e.preventDefault()
  const at = items.indexOf(document.activeElement as HTMLElement)
  const next = e.key === 'Home' ? 0
    : e.key === 'End' ? items.length - 1
      : e.key === 'ArrowDown' ? (at + 1) % items.length
        : (at - 1 + items.length) % items.length
  items[next]!.focus()
}

// save sends the nickname and the color once either differs from what is
// saved: on Enter or leaving the field, and on a swatch click.
async function save() {
  const nick = nickname.value.trim()
  if (!nick) { result.value = t("profile.nickname.empty"); return }
  if (busy.value || (nick === shown.value && color.value === saved.value)) return
  busy.value = true
  result.value = ''
  try {
    // The color derived from the name is kept as "no own color".
    const own = color.value === whoName(app.self) && !app.status?.chat_color ? '' : color.value
    app.status = await api<Status>('POST', 'profile', { nickname: nick, color: own })
    nickname.value = shown.value
  } catch (e) {
    result.value = (e as Error).message
  } finally {
    busy.value = false
  }
}

function pick(c: string) {
  color.value = c
  void save()
}
</script>

<template>
  <UPopover
    v-model:open="open"
    :content="{ side: 'top', align: 'start', sideOffset: 8, collisionPadding: 8 }"
    :ui="{ content: 'z-50' }"
  >
    <button
      id="user_chip"
      type="button"
      class="user-chip"
      :style="{ '--who': mine }"
      :title="t('account.title')"
      :aria-label="fmt('account.chip', { name: shown })"
      aria-haspopup="dialog"
      :aria-expanded="open ? 'true' : 'false'"
    >
      <span
        class="user-chip-dot"
        aria-hidden="true"
      />
      <span class="user-chip-name">{{ shown }}</span>
    </button>
    <template #content>
      <div
        id="account_menu"
        ref="pane"
        class="account-pop"
        :aria-label="t('account.title')"
        @keydown="nav"
      >
        <div
          id="account_head"
          class="account-head"
          :style="{ '--who': mine }"
        >
          <span
            class="user-chip-dot"
            aria-hidden="true"
          />
          <div class="min-w-0">
            <div class="account-name">
              {{ shown }}
            </div>
            <div
              v-if="app.link.text"
              id="account_status"
              class="account-status"
              :class="app.link.cls"
            >
              {{ app.link.text }}
            </div>
          </div>
        </div>
        <div
          id="profile_form"
          class="account-section flex flex-col gap-2"
        >
          <label
            for="profile_nickname"
            class="text-sm font-semibold text-muted"
          >{{ t('profile.nickname') }}</label>
          <UInput
            id="profile_nickname"
            v-model="nickname"
            maxlength="32"
            autocomplete="off"
            :title="fmt('profile.nickname.hint', { name: app.self })"
            @keydown.enter.prevent="save"
            @blur="save"
          />
          <span
            id="profile_color_label"
            class="text-sm font-semibold text-muted"
          >{{ t('profile.color') }}</span>
          <div
            id="profile_color"
            class="flex flex-wrap justify-between gap-1.5"
            role="group"
            aria-labelledby="profile_color_label"
            :title="t('profile.color.hint')"
          >
            <button
              v-for="c in CHAT_COLORS"
              :key="c"
              type="button"
              class="config-swatch"
              :class="{ active: color === c }"
              :data-chat-color="c"
              :title="t('profile.color.' + c)"
              :aria-label="t('profile.color.' + c)"
              :aria-pressed="color === c ? 'true' : 'false'"
              :disabled="busy"
              :style="{ backgroundColor: 'var(--who-' + c + ')' }"
              @click="pick(c)"
            />
          </div>
          <p
            v-if="result"
            id="profile_result"
            class="text-xs text-error"
            role="status"
          >
            {{ result }}
          </p>
        </div>
        <div class="account-section">
          <ThemeSettings />
        </div>
      </div>
    </template>
  </UPopover>
</template>
