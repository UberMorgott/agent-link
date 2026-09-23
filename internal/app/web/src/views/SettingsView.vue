<script setup lang="ts">
import { computed, nextTick, reactive, ref, watch } from 'vue'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import ToggleSwitch from 'primevue/toggleswitch'
import AppIcon from '@/components/AppIcon.vue'
import { api } from '@/lib/api'
import { browser, fmt, t } from '@/lib/runtime'
import {
  effectiveAPI, newCode, projectsBody, projectsKey, settingsBody, type ProjectRow, type SettingsFields,
} from '@/lib/settingsForm'
import { useAppStore } from '@/stores/app'
import type { AppSettings, HookStatus, SaveResult, UpdateStatus } from '@/types'

const app = useAppStore()

const form = reactive<SettingsFields>({
  node: '', code: '', handler: 'none', agent_path: '', work_dir: '', listen: '', api: '', areas: '',
  discovery: true, max_jobs: '', autostart: false, auto_answer: false,
})
const rows = ref<ProjectRow[]>([])
const result = ref('')
const busy = ref(false)
const agentShown = ref('')
const workDirKey = ref("settings.work_dir.current")
const workDirHooks = ref('')
const hooksCodex = ref(false)
const advancedOpen = ref(false)
const picking = ref(false)
const pickingAgent = ref(false)
const findingAgent = ref(false)
const codeInput = ref<{ $el: HTMLInputElement } | null>(null)
const projectList = ref<HTMLElement | null>(null)
let rowSeq = 0

// Every change saves itself: toggles, pickers and row removal at once, text
// fields on "change" (blur or Enter), so a half-typed value never restarts the
// node. edits counts edits in the form; the form is repainted from the server
// only when every edit has been saved.
let edits = 0
let settled = 0
let saving = false
let saveAgain = false
let lastSent = ''

function newRow(area: string, dir: string): ProjectRow {
  return { key: ++rowSeq, area, dir, hooks: '' }
}

const body = () => settingsBody(form, projectsBody(rows.value), app.settings)

function showSettings(s: AppSettings | null) {
  // Unsaved edits win over a repaint; the next save brings them together.
  if (!s || saving || edits !== settled) return
  form.node = s.node || ''
  form.code = s.code || ''
  form.work_dir = s.work_dir || ''
  form.listen = s.listen || ''
  form.api = s.api || ''
  form.areas = (s.areas || []).join(', ')
  form.handler = s.handler || 'none'
  form.agent_path = s.agent_path || ''
  void showAgent()
  form.autostart = !!s.autostart
  form.auto_answer = !!s.auto_answer
  form.discovery = s.discovery !== false
  form.max_jobs = s.max_jobs ? String(s.max_jobs) : ''
  if (s.listen || s.api || s.discovery === false || s.max_jobs || (s.areas || []).length) advancedOpen.value = true
  workDirKey.value = "settings.work_dir.current"
  const projects = s.projects || {}
  // Rebuilding equal rows would only take the focus away from them.
  const current = projectsBody(rows.value)
  if (!current.valid || projectsKey(current.projects) !== projectsKey(projects)) {
    rows.value = Object.keys(projects).sort().map((area) => newRow(area, projects[area]!.dir || ''))
  }
  lastSent = JSON.stringify(body())
}

// saveSettings sends the form when it differs from what was last sent. One
// request runs at a time; a change during it saves again afterwards. note
// replaces «Сохранено.» on success.
async function saveSettings(note?: string): Promise<void> {
  if (saving) { saveAgain = true; return }
  const projects = projectsBody(rows.value)
  const request = settingsBody(form, projects, app.settings)
  const sent = JSON.stringify(request), started = edits
  if (projects.duplicate) result.value = t("error.projects_twice")
  if (sent === lastSent) {
    if (projects.valid) settled = started
    return
  }
  const previousAPI = effectiveAPI(app.settings)
  if (!projects.duplicate) result.value = t("settings.saving")
  saving = true
  busy.value = true
  try {
    const r = await api<SaveResult>('POST', 'settings', request)
    lastSent = sent
    const text = r.error ? r.error : (note || t("settings.saved"))
    if (!projects.duplicate) result.value = r.found ? text + ' ' + r.found : text
    saving = false
    if (projects.valid && edits === started) settled = started
    if (r.settings) app.settings = r.settings
    if (r.status) app.status = r.status
    if (r.dashboard) app.dashboard = r.dashboard
    if (r.settings && effectiveAPI(r.settings) !== previousAPI) browser.reload()
  } catch (e) {
    result.value = (e as Error).message
  } finally {
    saving = false
    busy.value = false
  }
  if (saveAgain) {
    saveAgain = false
    await saveSettings()
  }
}

// editedAndSave saves a value that a script put into the form: such a value
// fires neither "input" nor "change".
function editedAndSave(note?: string) {
  edits++
  return saveSettings(note)
}

// The auto-update switch sits in the form but saves through its own request.
const ownRequest = (ev: Event) => (ev.target as HTMLElement | null)?.id === 'update_auto'
function onInput(ev: Event) { if (!ownRequest(ev)) edits++ }
function onChange(ev: Event) { if (!ownRequest(ev)) void saveSettings() }

// --- «Проекты»: an area mapped to a project folder ---

async function addProject() {
  const row = newRow('', '')
  rows.value = [...rows.value, row]
  await nextTick()
  projectList.value?.querySelector<HTMLInputElement>('[data-row="' + row.key + '"] input')?.focus()
}

function removeProject(row: ProjectRow) {
  rows.value = rows.value.filter((r) => r !== row)
  void editedAndSave()
}

// The tray process opens the native Windows folder dialog: a page cannot see
// absolute paths on disk. pickFolder returns the chosen folder.
async function pickFolder(start: string): Promise<string | null> {
  result.value = t("settings.work_dir.picking")
  try {
    const r = await api<{ path?: string; message?: string }>('POST', 'pick-folder', { start: start.trim() })
    if (r.path) {
      result.value = ''
      return r.path
    }
    result.value = r.message || ''
  } catch (e) {
    result.value = (e as Error).message
  }
  return null
}

async function pickWorkDir() {
  picking.value = true
  const path = await pickFolder(form.work_dir).finally(() => { picking.value = false })
  if (path === null) return
  form.work_dir = path
  workDirKey.value = "settings.work_dir.chosen"
  await editedAndSave()
}

async function pickProjectDir(row: ProjectRow) {
  const path = await pickFolder(row.dir)
  if (path === null) return
  row.dir = path
  await editedAndSave()
}

// --- the agent program ---

// showAgent tells which agent program the chosen handler would run.
async function showAgent() {
  const handler = form.handler
  if (handler === 'none') return
  try {
    const r = await api<{ text?: string }>('POST', 'agent', { handler, agent_path: form.agent_path })
    if (form.handler === handler) agentShown.value = r.text || ''
  } catch (e) { agentShown.value = (e as Error).message }
}

// A program chosen for one agent is not the other agent's program. The form's
// own "change" listener then saves the new handler.
function handlerChanged() {
  form.agent_path = ''
  void showAgent()
}

// «Найти заново» looks through every known install location and saves the
// program it finds, like a picked program.
async function findAgent() {
  findingAgent.value = true
  result.value = t("settings.agent.finding")
  const handler = form.handler
  try {
    const r = await api<{ path?: string; source?: string; text?: string }>('POST', 'find-agent', { handler })
    if (form.handler !== handler) return
    agentShown.value = r.text || ''
    result.value = ''
    if (r.source !== 'missing') {
      form.agent_path = r.path || ''
      await editedAndSave()
    }
  } catch (e) {
    result.value = (e as Error).message
  } finally {
    findingAgent.value = false
  }
}

// The tray process opens the native Windows file dialog for the program.
async function pickAgent() {
  pickingAgent.value = true
  result.value = t("settings.agent.picking")
  try {
    const r = await api<{ path?: string; message?: string }>('POST', 'pick-agent', { start: form.agent_path })
    if (r.path) {
      form.agent_path = r.path
      await showAgent()
      await editedAndSave(fmt("settings.agent.chosen", { path: r.path }))
    } else {
      result.value = r.message || ''
    }
  } catch (e) {
    result.value = (e as Error).message
  } finally {
    pickingAgent.value = false
  }
}

// --- the pairing code ---

function generate() {
  form.code = newCode()
  return editedAndSave(t("settings.code.generated"))
}

async function copyCode() {
  try {
    await navigator.clipboard.writeText(form.code.toUpperCase())
    result.value = t("settings.code.copied")
  } catch {
    codeInput.value?.$el.select()
    result.value = t("settings.code.copy_manual")
  }
}

// --- read-only lines ---

// The working folder repeated in full under the field.
const workDirShown = computed(() => {
  const path = form.work_dir.trim()
  return path ? fmt(workDirKey.value, { path }) : t("settings.work_dir.empty")
})

// This machine's advertised address stays with the advanced network settings
// instead of being presented as a participant.
const myAddr = computed(() => {
  const st = app.status
  if (!st?.configured) return ''
  const addr = (st.listen || '').replace(/:7420$/, '')
  let text = addr ? fmt("settings.my_addr", { addr }) : ''
  if (!st.zerotier) text += ' ' + t("settings.my_addr.none")
  return text
})

// --- hooks: the chosen agent's hook in the working and project folders ---

function hookText(client: string | undefined, state: string | undefined) {
  if (!client || !state) return ''
  if (state === 'ok') return fmt("settings.hooks.ok", { agent: client === 'codex' ? 'Codex' : 'Claude' })
  return t("settings.hooks." + state)
}

// showHooks reads what the app installed at the last save or start.
async function showHooks() {
  let h: HookStatus
  try { h = await api<HookStatus>('GET', 'hooks') } catch { return }
  const projects = h.projects || {}
  workDirHooks.value = hookText(h.client, h.work_dir)
  for (const r of rows.value) r.hooks = hookText(h.client, projects[r.area.trim()])
  hooksCodex.value = h.client === 'codex' && [h.work_dir, ...Object.values(projects)].includes('ok')
}

// --- updates: own buttons and switch, saved by their own requests ---

const upd = computed<UpdateStatus>(() => app.update || {})
const updText = ref<string | null>(null)
const updWorking = ref(false)
const updAuto = ref(false)
watch(() => app.update, (u) => {
  updText.value = null
  updWorking.value = false
  updAuto.value = !!u?.auto
}, { immediate: true })

async function updateAction(path: string, request?: unknown, busyText?: string) {
  if (busyText) updText.value = busyText
  updWorking.value = true
  try {
    app.update = await api<UpdateStatus>('POST', path, request)
  } catch (e) {
    updText.value = (e as Error).message
    updWorking.value = false
  }
}

watch(() => app.settings, (s) => { showSettings(s); void showHooks() }, { immediate: true, flush: 'sync' })
</script>

<template>
  <section
    data-view="settings"
    class="h-full overflow-y-auto"
  >
    <div class="mx-auto flex max-w-3xl flex-col gap-8 px-4 py-10">
      <header class="view-header">
        <h1>{{ t("settings.h1") }}</h1>
      </header>
      <form
        id="form"
        novalidate
        class="flex flex-col gap-10"
        :aria-busy="busy ? 'true' : undefined"
        @input="onInput"
        @change="onChange"
        @submit.prevent="saveSettings()"
      >
        <section
          class="settings-card flex flex-col gap-2"
          data-settings-card="identity"
          aria-labelledby="settings_identity_title"
        >
          <h2 id="settings_identity_title">
            {{ t("settings.identity.title") }}
          </h2>
          <label class="field"><span>{{ t("settings.node.label") }}</span><InputText
            v-model="form.node"
            name="node"
            autocomplete="off"
          /></label>
          <p class="hint">
            {{ t("settings.node.hint") }}
          </p>
          <label class="field"><span>{{ t("settings.code.label") }}</span>
            <span class="flex flex-wrap gap-2">
              <InputText
                id="code"
                ref="codeInput"
                v-model="form.code"
                name="code"
                maxlength="16"
                autocomplete="off"
                spellcheck="false"
                class="code flex-1 font-mono uppercase"
              />
              <Button
                id="generate"
                type="button"
                :label="t('settings.code.generate')"
                severity="secondary"
                outlined
                @click="generate"
              />
              <Button
                id="copy"
                type="button"
                :label="t('settings.code.copy')"
                severity="secondary"
                outlined
                @click="copyCode"
              />
            </span>
          </label>
          <p class="hint">
            {{ t("settings.code.hint") }}
          </p>
        </section>

        <section
          class="settings-card flex flex-col gap-2"
          data-settings-card="handler"
          aria-labelledby="settings_handler_title"
        >
          <h2 id="settings_handler_title">
            {{ t("settings.handler.title") }}
          </h2>
          <label class="field"><span>{{ t("settings.handler.label") }}</span>
            <select
              v-model="form.handler"
              name="handler"
              class="native-select"
              @change="handlerChanged"
            >
              <option value="none">{{ t("settings.handler.none") }}</option>
              <option value="claude">{{ t("settings.handler.claude") }}</option>
              <option value="codex">{{ t("settings.handler.codex") }}</option>
            </select>
          </label>
          <p class="hint">
            {{ t("settings.handler.hint") }}
          </p>
          <p class="handler-note text-sm">
            {{ t("settings.handler.context") }}
          </p>
          <div
            v-if="form.handler !== 'none'"
            id="agent_row"
            class="flex flex-col gap-1.5"
          >
            <span class="text-sm">{{ t("settings.agent.label") }}</span>
            <span class="flex flex-wrap items-center gap-2">
              <span
                id="agent_shown"
                class="min-w-0 flex-1 text-sm break-all"
              >{{ agentShown }}</span>
              <Button
                id="find_agent"
                type="button"
                :label="t('settings.agent.find')"
                severity="secondary"
                outlined
                size="small"
                :disabled="findingAgent"
                @click="findAgent"
              />
              <Button
                id="pick_agent"
                type="button"
                :label="t('settings.agent.pick')"
                severity="secondary"
                outlined
                size="small"
                :disabled="pickingAgent"
                @click="pickAgent"
              />
            </span>
            <p class="hint">
              {{ t("settings.agent.hint") }}
            </p>
          </div>
          <label class="check"><ToggleSwitch
            v-model="form.auto_answer"
            name="auto_answer"
            input-id="auto_answer"
          /> <span>{{ t("settings.auto_answer.label") }}</span></label>
          <p class="hint">
            {{ t("settings.auto_answer.hint") }}
          </p>
        </section>

        <section
          class="settings-card flex flex-col gap-2"
          data-settings-card="application"
          aria-labelledby="settings_application_title"
        >
          <h2 id="settings_application_title">
            {{ t("settings.application.title") }}
          </h2>
          <label class="field"><span>{{ t("settings.work_dir.label") }}</span>
            <span class="flex gap-2">
              <InputText
                id="work_dir"
                v-model="form.work_dir"
                name="work_dir"
                autocomplete="off"
                spellcheck="false"
                class="flex-1"
                @input="workDirKey = 'settings.work_dir.current'"
              />
              <Button
                id="pick"
                type="button"
                :label="t('settings.work_dir.pick')"
                severity="secondary"
                outlined
                :disabled="picking"
                @click="pickWorkDir"
              />
            </span>
          </label>
          <p
            id="work_dir_shown"
            class="path text-sm break-all"
          >
            {{ workDirShown }}
          </p>
          <p
            v-if="workDirHooks"
            id="work_dir_hooks"
            class="hint"
          >
            {{ workDirHooks }}
          </p>
          <p
            v-if="hooksCodex"
            id="hooks_codex"
            class="hint"
          >
            {{ t("settings.hooks.codex") }}
          </p>
          <p class="hint">
            {{ t("settings.work_dir.hint") }}
          </p>
          <label class="check"><ToggleSwitch
            v-model="form.autostart"
            name="autostart"
            input-id="autostart"
          /> <span>{{ t("settings.autostart.label") }}</span></label>
        </section>

        <section
          class="settings-card settings-projects flex flex-col gap-2"
          data-settings-card="projects"
          aria-labelledby="settings_projects_title"
        >
          <h2 id="settings_projects_title">
            {{ t("settings.projects.title") }}
          </h2>
          <p class="hint">
            {{ t("settings.projects.hint") }}
          </p>
          <p class="hint warn">
            {{ t("settings.projects.warn") }}
          </p>
          <p
            v-if="!rows.length"
            id="projects_empty"
            class="hint"
          >
            {{ t("settings.projects.empty") }}
          </p>
          <ul
            id="projects"
            ref="projectList"
            class="project-list flex flex-col gap-4"
          >
            <li
              v-for="row in rows"
              :key="row.key"
              class="project-card flex flex-col gap-2 border-l-2 border-[var(--app-line)] pl-4"
              :data-row="row.key"
            >
              <label class="field"><span>{{ t("settings.projects.area") }}</span><InputText
                v-model="row.area"
                autocomplete="off"
                spellcheck="false"
              /></label>
              <label class="field"><span>{{ t("settings.projects.dir") }}</span>
                <span class="flex gap-2">
                  <InputText
                    v-model="row.dir"
                    autocomplete="off"
                    spellcheck="false"
                    class="flex-1"
                  />
                  <Button
                    type="button"
                    :label="t('settings.work_dir.pick')"
                    severity="secondary"
                    outlined
                    @click="pickProjectDir(row)"
                  />
                </span>
              </label>
              <p class="hint">
                {{ row.hooks }}
              </p>
              <Button
                type="button"
                class="project-remove self-start"
                :label="t('settings.projects.remove')"
                severity="danger"
                text
                size="small"
                @click="removeProject(row)"
              />
            </li>
          </ul>
          <Button
            id="add_project"
            type="button"
            class="self-start"
            :label="t('settings.projects.add')"
            severity="secondary"
            text
            @click="addProject"
          >
            <template #icon>
              <AppIcon name="plus" />
            </template>
          </Button>
        </section>

        <section
          id="updates"
          class="settings-card flex flex-col gap-2"
          data-settings-card="updates"
          aria-labelledby="settings_updates_title"
        >
          <h2 id="settings_updates_title">
            {{ t("update.legend") }}
          </h2>
          <p class="update-actions flex flex-wrap items-center gap-2">
            <span
              id="update_version"
              class="text-sm"
            >{{ fmt("update.version", { version: upd.current || "" }) }}</span>
            <Button
              id="update_check"
              type="button"
              :label="t('update.check')"
              severity="secondary"
              outlined
              size="small"
              :disabled="!upd.enabled || upd.busy || updWorking"
              @click="updateAction('update/check', undefined, t('update.checking'))"
            />
            <Button
              v-if="upd.available"
              id="update_apply"
              type="button"
              size="small"
              :label="fmt('update.apply', { version: upd.latest || '' })"
              :disabled="upd.busy || updWorking"
              @click="updateAction('update/apply', undefined, fmt('update.applying', { version: upd.latest || '' }))"
            />
          </p>
          <p
            id="update_text"
            class="hint"
            :class="{ failed: upd.failed && updText === null }"
            role="status"
          >
            {{ updText ?? upd.text ?? "" }}
          </p>
          <label class="check">
            <ToggleSwitch
              v-model="updAuto"
              input-id="update_auto"
              :disabled="!upd.enabled"
              @update:model-value="(v: boolean) => updateAction('update/auto', { auto: v })"
            />
            <span>{{ t("update.auto") }}</span>
          </label>
          <p class="hint">
            {{ t("update.auto.hint") }}
          </p>
        </section>

        <section
          class="settings-card settings-advanced"
          data-settings-card="advanced"
        >
          <details
            id="advanced"
            :open="advancedOpen"
            @toggle="advancedOpen = ($event.target as HTMLDetailsElement).open"
          >
            <summary class="cursor-pointer font-semibold">
              {{ t("settings.advanced") }}
            </summary>
            <div class="advanced-fields mt-4 flex flex-col gap-2">
              <label class="field"><span>{{ t("settings.listen.label") }}</span><InputText
                v-model="form.listen"
                name="listen"
                autocomplete="off"
              /></label>
              <p class="hint">
                {{ t("settings.listen.hint") }}
              </p>
              <p
                v-if="myAddr"
                id="my_addr"
                class="hint"
              >
                {{ myAddr }}
              </p>
              <label class="field"><span>{{ t("settings.api.label") }}</span><InputText
                v-model="form.api"
                name="api"
                autocomplete="off"
                placeholder="127.0.0.1:7520"
              /></label>
              <p class="hint">
                {{ t("settings.api.hint") }}
              </p>
              <label class="field"><span>{{ t("settings.areas.label") }}</span><InputText
                v-model="form.areas"
                name="areas"
                autocomplete="off"
              /></label>
              <p class="hint">
                {{ t("settings.areas.hint") }}
              </p>
              <label class="check"><ToggleSwitch
                v-model="form.discovery"
                name="discovery"
                input-id="discovery"
              /> <span>{{ t("settings.discovery.label") }}</span></label>
              <p class="hint">
                {{ t("settings.discovery.hint") }}
              </p>
              <label class="field"><span>{{ t("settings.max_jobs.label") }}</span><InputText
                v-model="form.max_jobs"
                name="max_jobs"
                type="number"
                min="1"
                max="4"
                step="1"
                placeholder="2"
              /></label>
              <p class="hint">
                {{ t("settings.max_jobs.hint") }}
              </p>
            </div>
          </details>
        </section>
        <footer class="settings-actions sticky bottom-0 bg-[var(--app-bg)] py-2">
          <p
            id="settings_result"
            role="status"
            class="text-sm"
          >
            {{ result }}
          </p>
        </footer>
      </form>
    </div>
  </section>
</template>

<style scoped>
.field { display: flex; flex-direction: column; gap: 0.35rem; font-size: 0.9rem; }
.check { display: flex; align-items: center; gap: 0.6rem; font-size: 0.9rem; }
.native-select {
  padding: 0.5rem 0.75rem; border: 1px solid var(--p-inputtext-border-color, var(--app-line)); border-radius: var(--p-inputtext-border-radius, 6px);
  background: var(--p-inputtext-background, var(--app-bg)); color: var(--app-text); font: inherit;
}
</style>
