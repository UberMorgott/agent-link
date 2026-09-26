<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UInput from '@nuxt/ui/components/Input.vue'
import UPopover from '@nuxt/ui/components/Popover.vue'
import { api } from '@/lib/api'
import { CHAT_COLORS, whoColor, whoName } from '@/lib/chat'
import { fmt, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import type { Status } from '@/types'

// The member's own chip at the foot of the sidebar: its nickname in its chat
// color. It opens the profile: the nickname and the color the other members
// see on its messages and its agents' messages. A save applies at once in
// every project (POST profile); the name itself stays the member's identity.
const app = useAppStore()
const open = ref(false)
const nickname = ref('')
const color = ref('')
const busy = ref(false)
const result = ref('')

const shown = computed(() => app.status?.nickname || app.self)
const mine = computed(() => whoColor(app.self, app.status?.chat_color || ''))

// Every opening starts from what is saved.
watch(open, (now) => {
  if (!now) return
  nickname.value = shown.value
  color.value = whoName(app.self, app.status?.chat_color || '')
  result.value = ''
})

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
    :content="{ side: 'top', align: 'start' }"
  >
    <button
      id="user_chip"
      type="button"
      class="user-chip"
      :style="{ '--who': mine }"
      :title="t('profile.title')"
      :aria-label="fmt('profile.chip', { name: shown })"
    >
      <span
        class="user-chip-dot"
        aria-hidden="true"
      />
      <span class="user-chip-name">{{ shown }}</span>
    </button>
    <template #content>
      <form
        id="profile_form"
        class="flex w-80 max-w-[calc(100vw-2rem)] flex-col gap-3 p-4"
        @submit.prevent="save"
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
    </template>
  </UPopover>
</template>
