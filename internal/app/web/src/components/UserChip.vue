<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import UInput from '@nuxt/ui/components/Input.vue'
import UPopover from '@nuxt/ui/components/Popover.vue'
import ThemeSettings from '@/layout/ThemeSettings.vue'
import { api } from '@/lib/api'
import { CHAT_COLORS, whoColor, whoName } from '@/lib/chat'
import { icon } from '@/lib/icons'
import { fmt, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import type { Status } from '@/types'

// The member's account button at the foot of the sidebar: its nickname in its
// chat color. It opens one menu: the name and the connection, the profile (the
// nickname and the color the other members see on its messages and its agents'
// messages) and the appearance of this browser. A profile save applies at once
// in every project (POST profile); the name itself stays the member's identity.
const app = useAppStore()
const open = ref(false)
const view = ref<'menu' | 'profile'>('menu')
const nickname = ref('')
const color = ref('')
const busy = ref(false)
const result = ref('')
const pane = ref<HTMLElement | null>(null)

const shown = computed(() => app.status?.nickname || app.self)
const mine = computed(() => whoColor(app.self, app.status?.chat_color || ''))

// Every opening starts at the menu.
watch(open, (now) => { if (now) view.value = 'menu' })

async function focusIn(sel: string) {
  await nextTick()
  pane.value?.querySelector<HTMLElement>(sel)?.focus()
}

// The profile starts from what is saved.
function editProfile() {
  nickname.value = shown.value
  color.value = whoName(app.self, app.status?.chat_color || '')
  result.value = ''
  view.value = 'profile'
  void focusIn('#profile_nickname')
}

function back() {
  view.value = 'menu'
  void focusIn('#account_profile')
}

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

async function save() {
  const nick = nickname.value.trim()
  if (!nick) { result.value = t("profile.nickname.empty"); return }
  busy.value = true
  result.value = ''
  try {
    // The color derived from the name is kept as "no own color".
    const own = color.value === whoName(app.self) && !app.status?.chat_color ? '' : color.value
    app.status = await api<Status>('POST', 'profile', { nickname: nick, color: own })
    open.value = false
  } catch (e) {
    result.value = (e as Error).message
  } finally {
    busy.value = false
  }
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
        <template v-if="view === 'menu'">
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
                id="account_status"
                class="account-status"
                :class="app.link.cls"
              >
                {{ app.link.text }}
              </div>
            </div>
          </div>
          <button
            id="account_profile"
            type="button"
            class="account-item"
            @click="editProfile"
          >
            <UIcon
              :name="icon('rename')"
              class="size-4 flex-none"
            />
            <span class="min-w-0 flex-1 truncate">{{ t('profile.title') }}</span>
            <UIcon
              :name="icon('fold')"
              class="size-4 flex-none text-muted"
            />
          </button>
          <div class="account-section">
            <ThemeSettings />
          </div>
        </template>
        <form
          v-else
          id="profile_form"
          class="flex flex-col gap-3 p-2"
          @submit.prevent="save"
        >
          <button
            id="account_back"
            type="button"
            class="account-item account-back"
            @click="back"
          >
            <UIcon
              :name="icon('back')"
              class="size-4 flex-none"
            />
            <span>{{ t('profile.title') }}</span>
          </button>
          <label
            for="profile_nickname"
            class="text-sm font-semibold text-muted"
          >{{ t('profile.nickname') }}</label>
          <UInput
            id="profile_nickname"
            v-model="nickname"
            maxlength="32"
            autocomplete="off"
          />
          <p class="hint">
            {{ fmt('profile.nickname.hint', { name: app.self }) }}
          </p>
          <span
            id="profile_color_label"
            class="text-sm font-semibold text-muted"
          >{{ t('profile.color') }}</span>
          <div
            id="profile_color"
            class="flex flex-wrap gap-1.5"
            role="group"
            aria-labelledby="profile_color_label"
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
              :style="{ backgroundColor: 'var(--who-' + c + ')' }"
              @click="color = c"
            />
          </div>
          <p class="hint">
            {{ t('profile.color.hint') }}
          </p>
          <p
            id="profile_result"
            class="text-xs text-error"
            role="status"
          >
            {{ result }}
          </p>
          <UButton
            id="profile_save"
            type="submit"
            :label="t('project.save')"
            :disabled="busy"
            class="self-end"
          />
        </form>
      </div>
    </template>
  </UPopover>
</template>
