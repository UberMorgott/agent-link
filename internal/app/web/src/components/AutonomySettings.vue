<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UInput from '@nuxt/ui/components/Input.vue'
import USwitch from '@nuxt/ui/components/Switch.vue'
import { api } from '@/lib/api'
import { fmt, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { useProjectsStore } from '@/stores/projects'
import type { AutonomyMode, AutonomyRequest, ProjectView, Status } from '@/types'

// «Автономия агентов»: the emergency stop of every project's agents and, per
// project, how far its agents work by themselves (mode, hop limit, budgets of
// full mode) with what of the budgets is used and «Продолжить» after a pause.
// Every change saves itself through its own request, not the settings form.
const app = useAppStore()
const projects = useProjectsStore()

const MODES: AutonomyMode[] = ['off', 'asked', 'full']

const list = computed(() => (projects.list || []).filter((p) => !p.legacy && p.autonomy))
const stopAll = computed(() => !!app.status?.stop_all)
const stopBusy = ref(false)
const stopText = ref('')
// Per project: the text under its card (saved, an error) and a request running.
const notes = reactive<Record<string, string>>({})
const busy = reactive<Record<string, boolean>>({})

async function setStop(on: boolean) {
  stopBusy.value = true
  stopText.value = ''
  try {
    app.status = await api<Status>('POST', 'autonomy/stop', { on })
  } catch (e) {
    stopText.value = (e as Error).message
  } finally {
    stopBusy.value = false
  }
}

async function save(p: ProjectView, body: AutonomyRequest) {
  busy[p.id] = true
  notes[p.id] = ''
  try {
    await projects.bind(p.id, body)
    notes[p.id] = t("autonomy.saved")
  } catch (e) {
    notes[p.id] = (e as Error).message
  } finally {
    busy[p.id] = false
  }
}

// number reads a limit field: '' is the default (fallback), anything else a
// whole number.
function number(ev: Event, fallback: number): number {
  const raw = (ev.target as HTMLInputElement).value.trim()
  if (raw === '') return fallback
  const n = Number(raw)
  return Number.isInteger(n) ? n : NaN
}

function setMode(p: ProjectView, ev: Event) {
  void save(p, { autonomy: (ev.target as HTMLSelectElement).value as AutonomyMode })
}

function setLimit(p: ProjectView, field: 'max_auto_depth' | 'turns_per_hour' | 'max_run_minutes', ev: Event) {
  // An empty hop limit follows the mode again (-1); an empty budget is its default (0).
  const value = number(ev, field === 'max_auto_depth' ? -1 : 0)
  if (Number.isNaN(value)) {
    notes[p.id] = t("error.autonomy")
    return
  }
  void save(p, { [field]: value })
}

async function resume(p: ProjectView) {
  busy[p.id] = true
  try {
    await projects.resumeAutonomy(p.id)
    notes[p.id] = ''
  } catch (e) {
    notes[p.id] = (e as Error).message
  } finally {
    busy[p.id] = false
  }
}

const depthShown = (p: ProjectView) => (p.autonomy!.max_auto_depth_default ? '' : String(p.autonomy!.max_auto_depth))
const depthDefault = (p: ProjectView) => fmt("autonomy.depth.default", {
  value: p.autonomy!.max_auto_depth === 0 ? t("autonomy.depth.unlimited") : String(p.autonomy!.max_auto_depth),
})
</script>

<template>
  <section
    id="autonomy"
    class="settings-card flex flex-col gap-2"
    data-settings-card="autonomy"
    data-own-request
    aria-labelledby="settings_autonomy_title"
  >
    <h2 id="settings_autonomy_title">
      {{ t("autonomy.title") }}
    </h2>
    <p class="hint">
      {{ t("autonomy.intro") }}
    </p>
    <USwitch
      id="stop_all"
      :model-value="stopAll"
      :label="t('autonomy.stop.label')"
      :disabled="stopBusy"
      color="error"
      class="check"
      @update:model-value="(v: boolean) => setStop(v)"
    />
    <p class="hint">
      {{ t("autonomy.stop.hint") }}
    </p>
    <p
      v-if="stopAll"
      id="stop_all_on"
      class="hint warn"
      role="status"
    >
      {{ t("autonomy.stop.on") }}
    </p>
    <p
      v-if="stopText"
      class="hint warn"
      role="status"
    >
      {{ stopText }}
    </p>
    <p
      v-if="!list.length"
      id="autonomy_none"
      class="hint"
    >
      {{ t("autonomy.none") }}
    </p>
    <ul class="flex flex-col gap-6">
      <li
        v-for="p in list"
        :key="p.id"
        class="autonomy-project flex flex-col gap-2 border-l-2 border-default pl-4"
        :data-autonomy="p.id"
      >
        <h3>{{ fmt("autonomy.project", { name: p.display || p.id }) }}</h3>
        <p
          v-if="p.autonomy!.paused"
          class="autonomy-paused hint warn flex flex-wrap items-center gap-2"
          role="status"
        >
          <span>{{ fmt("autonomy.paused", { reason: t("autonomy.reason." + (p.autonomy!.pause_reason || 'turns')) }) }}</span>
          <UButton
            class="autonomy-resume"
            type="button"
            size="sm"
            :label="t('autonomy.resume')"
            :disabled="busy[p.id]"
            @click="resume(p)"
          />
        </p>
        <label class="field"><span>{{ t("autonomy.mode.label") }}</span>
          <select
            class="native-select autonomy-mode"
            :value="p.autonomy!.mode"
            :disabled="busy[p.id]"
            @change="setMode(p, $event)"
          >
            <option
              v-for="m in MODES"
              :key="m"
              :value="m"
            >{{ t("autonomy.mode." + m) }}</option>
          </select>
        </label>
        <ul class="flex flex-col gap-1">
          <li
            v-for="m in MODES"
            :key="m"
            class="hint"
            :class="{ 'autonomy-current': m === p.autonomy!.mode }"
          >
            {{ t("autonomy.mode." + m + ".hint") }}
          </li>
        </ul>
        <label class="field"><span>{{ t("autonomy.depth.label") }}</span><UInput
          class="autonomy-depth"
          type="number"
          min="0"
          max="250"
          step="1"
          :model-value="depthShown(p)"
          :placeholder="String(p.autonomy!.max_auto_depth)"
          :disabled="busy[p.id]"
          @change="setLimit(p, 'max_auto_depth', $event)"
        /></label>
        <p class="hint">
          {{ depthDefault(p) }}. {{ t("autonomy.depth.hint") }}
        </p>
        <label class="field"><span>{{ t("autonomy.turns.label") }}</span><UInput
          class="autonomy-turns"
          type="number"
          min="1"
          max="600"
          step="1"
          :model-value="String(p.autonomy!.turns_per_hour)"
          :disabled="busy[p.id]"
          @change="setLimit(p, 'turns_per_hour', $event)"
        /></label>
        <p class="hint">
          {{ t("autonomy.turns.hint") }}
        </p>
        <label class="field"><span>{{ t("autonomy.run.label") }}</span><UInput
          class="autonomy-run"
          type="number"
          min="10"
          max="1440"
          step="1"
          :model-value="String(p.autonomy!.max_run_minutes)"
          :disabled="busy[p.id]"
          @change="setLimit(p, 'max_run_minutes', $event)"
        /></label>
        <p class="hint">
          {{ t("autonomy.run.hint") }}
        </p>
        <p
          v-if="p.autonomy!.mode === 'full'"
          class="autonomy-used hint"
        >
          {{ fmt("autonomy.used", {
            turns: String(p.autonomy!.turns_last_hour), limit: String(p.autonomy!.turns_per_hour),
            minutes: String(p.autonomy!.run_minutes), max: String(p.autonomy!.max_run_minutes),
          }) }}
        </p>
        <p
          v-if="notes[p.id]"
          class="autonomy-note text-sm"
          role="status"
        >
          {{ notes[p.id] }}
        </p>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.field { display: flex; flex-direction: column; gap: 0.35rem; font-size: 0.9rem; }
.check { font-size: 0.9rem; }
.autonomy-current { color: var(--ui-text-highlighted); }
.native-select {
  padding: 0.375rem 0.625rem; border-radius: calc(var(--ui-radius) * 1.5); font: inherit; font-size: 0.875rem;
  background: var(--ui-bg); color: var(--ui-text-highlighted); border: 0; box-shadow: inset 0 0 0 1px var(--ui-border-accented);
}
.native-select:focus-visible { outline: 2px solid var(--ui-primary); outline-offset: 0; }
</style>
