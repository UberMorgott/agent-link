<script setup lang="ts">
import { computed, ref } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UPopover from '@nuxt/ui/components/Popover.vue'
import { FONTS, useLayout, type ThemeMode } from '@/layout/composables/layout'
import { icon } from '@/lib/icons'
import { primaryColors, surfaces } from '@/lib/palettes'
import { t } from '@/lib/runtime'

// The appearance panel under the sidebar: the theme, the accent colour, the
// background scale and the font, kept in this browser
// (layout/composables/layout.ts).
const { layoutConfig, storageFailed } = useLayout()
const open = ref(false)

const modes: { value: ThemeMode, icon: string }[] = [
  { value: 'light', icon: 'sun' },
  { value: 'dark', icon: 'moon' },
  { value: 'system', icon: 'system' },
]
const label = computed(() => t('theme.panel') + ' · ' + t('theme.' + layoutConfig.theme))
</script>

<template>
  <UPopover
    v-model:open="open"
    :content="{ side: 'top', align: 'start' }"
  >
    <UButton
      id="theme_config"
      :icon="icon('palette')"
      color="neutral"
      variant="ghost"
      size="sm"
      :aria-label="label"
      :title="label"
    />
    <template #content>
      <div
        id="theme_panel"
        class="config-panel flex max-h-[80vh] w-80 max-w-[calc(100vw-2rem)] flex-col gap-4 overflow-y-auto p-4"
      >
        <div class="flex flex-col gap-2">
          <span
            id="theme_mode_label"
            class="text-sm font-semibold text-muted"
          >{{ t('theme.mode') }}</span>
          <div
            id="theme_mode"
            class="grid grid-cols-3 gap-1 rounded-lg bg-elevated p-1"
            role="group"
            aria-labelledby="theme_mode_label"
          >
            <UButton
              v-for="m in modes"
              :key="m.value"
              :data-mode="m.value"
              :icon="icon(m.icon)"
              :label="t('theme.mode.' + m.value)"
              :aria-pressed="layoutConfig.theme === m.value ? 'true' : 'false'"
              :color="layoutConfig.theme === m.value ? 'primary' : 'neutral'"
              :variant="layoutConfig.theme === m.value ? 'solid' : 'ghost'"
              size="xs"
              class="justify-center"
              @click="layoutConfig.theme = m.value"
            />
          </div>
        </div>
        <div class="flex flex-col gap-2">
          <span
            id="theme_primary_label"
            class="text-sm font-semibold text-muted"
          >{{ t('theme.primary') }}</span>
          <div
            id="theme_primary"
            class="flex flex-wrap justify-between gap-1.5"
            role="group"
            aria-labelledby="theme_primary_label"
          >
            <button
              v-for="c in primaryColors"
              :key="c.name"
              type="button"
              class="config-swatch"
              :class="{ active: layoutConfig.primary === c.name }"
              :data-color="c.name"
              :title="t('theme.color.' + c.name)"
              :aria-label="t('theme.color.' + c.name)"
              :aria-pressed="layoutConfig.primary === c.name ? 'true' : 'false'"
              :style="{ backgroundColor: c.name === 'noir' ? 'var(--ui-text-highlighted)' : c.palette[500] }"
              @click="layoutConfig.primary = c.name"
            />
          </div>
        </div>
        <div class="flex flex-col gap-2">
          <span
            id="theme_surface_label"
            class="text-sm font-semibold text-muted"
          >{{ t('theme.surface') }}</span>
          <div
            id="theme_surface"
            class="flex flex-wrap gap-2"
            role="group"
            aria-labelledby="theme_surface_label"
          >
            <button
              v-for="s in surfaces"
              :key="s.name"
              type="button"
              class="config-swatch"
              :class="{ active: layoutConfig.surface === s.name }"
              :data-surface="s.name"
              :title="t('theme.surface.' + s.name)"
              :aria-label="t('theme.surface.' + s.name)"
              :aria-pressed="layoutConfig.surface === s.name ? 'true' : 'false'"
              :style="{ backgroundColor: s.palette[500] }"
              @click="layoutConfig.surface = s.name"
            />
          </div>
        </div>
        <div class="flex flex-col gap-2">
          <span
            id="theme_font_label"
            class="text-sm font-semibold text-muted"
          >{{ t('theme.font') }}</span>
          <div
            id="theme_font"
            class="flex flex-col gap-1"
            role="group"
            aria-labelledby="theme_font_label"
          >
            <button
              v-for="f in FONTS"
              :key="f.name"
              type="button"
              class="font-choice"
              :class="{ active: layoutConfig.font === f.name }"
              :data-font="f.name"
              :aria-pressed="layoutConfig.font === f.name ? 'true' : 'false'"
              :style="{ fontFamily: f.stack }"
              @click="layoutConfig.font = f.name"
            >
              <span class="font-choice-name">{{ t('theme.font.' + f.name) }}</span>
              <span class="font-choice-hint">{{ t('theme.font.' + f.name + '.hint') }}</span>
            </button>
          </div>
        </div>
        <p
          id="theme_saved"
          class="text-xs text-muted"
          role="status"
        >
          {{ t(storageFailed ? 'theme.unsaved' : 'theme.saved') }}
        </p>
      </div>
    </template>
  </UPopover>
</template>
