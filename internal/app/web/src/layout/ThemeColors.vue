<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UIcon from '@nuxt/ui/components/Icon.vue'
import { useLayout } from '@/layout/composables/layout'
import { ASPECTS, aspectContrast, lowContrast, resolveColour, type Aspect } from '@/lib/aspects'
import { icon } from '@/lib/icons'
import { fmt, t } from '@/lib/runtime'

// The palette inside «Тема» (layout/ThemeSettings.vue): a colour per aspect of
// the app, for the theme on screen. Each pick goes into its CSS token
// (layout/composables/layout.ts), so it recolours that aspect everywhere; a
// pick whose text would read below 4.5:1 gets a warning under its swatch.
const { layoutConfig, isDarkTheme } = useLayout()
const open = ref(false)
// What the page draws each unpicked aspect with, for the picker to start from.
const drawn = ref<Record<string, string>>({})

const picks = computed(() => layoutConfig.colors[isDarkTheme.value ? 'dark' : 'light'])
const count = computed(() => Object.keys(picks.value).length)

function refresh() {
  const out: Record<string, string> = {}
  for (const a of ASPECTS) out[a.name] = resolveColour(a.shown)
  drawn.value = out
}
onMounted(refresh)
watch(layoutConfig, () => { void nextTick(refresh) })

function value(a: Aspect): string {
  return picks.value[a.name] || drawn.value[a.name] || '#808080'
}

function warning(a: Aspect): string {
  const colour = picks.value[a.name]
  if (!colour || !lowContrast(a, colour, layoutConfig.surface, isDarkTheme.value)) return ''
  return fmt('theme.colors.low_contrast', { ratio: aspectContrast(a, colour, layoutConfig.surface, isDarkTheme.value).toFixed(1) })
}

function pick(a: Aspect, e: Event) {
  const colour = (e.target as HTMLInputElement).value.toLowerCase()
  if (/^#[0-9a-f]{6}$/.test(colour)) picks.value[a.name] = colour
}

function reset(a: Aspect) {
  delete picks.value[a.name]
}

function resetAll() {
  for (const name of Object.keys(picks.value)) delete picks.value[name]
}
</script>

<template>
  <div class="flex flex-col gap-2">
    <button
      id="theme_colors_toggle"
      type="button"
      class="colors-toggle"
      :aria-expanded="open ? 'true' : 'false'"
      aria-controls="theme_colors"
      @click="open = !open"
    >
      <span>{{ t('theme.colors') }}</span>
      <span class="colors-count">{{ count ? fmt('theme.colors.count', { count }) : '' }}</span>
      <UIcon
        :name="icon(open ? 'collapse' : 'expand')"
        class="size-4 text-muted"
        aria-hidden="true"
      />
    </button>
    <div
      v-if="open"
      id="theme_colors"
      class="flex flex-col gap-2"
      role="group"
      aria-labelledby="theme_colors_toggle"
    >
      <p class="text-xs text-muted">
        {{ t(isDarkTheme ? 'theme.colors.hint.dark' : 'theme.colors.hint.light') }}
      </p>
      <ul class="aspect-grid">
        <li
          v-for="a in ASPECTS"
          :key="a.name"
          class="aspect"
          :class="{ set: !!picks[a.name] }"
          :data-aspect="a.name"
        >
          <label
            class="aspect-pick"
            :title="t('theme.aspect.' + a.name + '.hint')"
          >
            <input
              type="color"
              class="aspect-input"
              :value="value(a)"
              :aria-label="t('theme.aspect.' + a.name)"
              :aria-describedby="warning(a) ? 'aspect_warn_' + a.name : undefined"
              @input="pick(a, $event)"
            >
            <span
              class="aspect-swatch"
              :class="{ members: !picks[a.name] && !a.shown }"
              :style="picks[a.name] || a.shown ? { background: picks[a.name] || a.shown } : undefined"
              aria-hidden="true"
            />
            <span class="aspect-name">{{ t('theme.aspect.' + a.name) }}</span>
          </label>
          <UButton
            v-if="picks[a.name]"
            class="aspect-reset"
            :icon="icon('reset')"
            :aria-label="fmt('theme.colors.reset_one', { name: t('theme.aspect.' + a.name) })"
            :title="t('theme.colors.reset')"
            color="neutral"
            variant="ghost"
            size="xs"
            @click="reset(a)"
          />
          <p
            v-if="warning(a)"
            :id="'aspect_warn_' + a.name"
            class="aspect-warn"
            role="status"
          >
            {{ warning(a) }}
          </p>
        </li>
      </ul>
      <UButton
        id="theme_colors_reset"
        :icon="icon('reset')"
        :label="t('theme.colors.reset_all')"
        :disabled="!count"
        color="neutral"
        variant="soft"
        size="xs"
        class="self-start"
        @click="resetAll"
      />
    </div>
  </div>
</template>
