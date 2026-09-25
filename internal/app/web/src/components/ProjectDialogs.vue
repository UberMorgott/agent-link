<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import UButton from '@nuxt/ui/components/Button.vue'
import UInput from '@nuxt/ui/components/Input.vue'
import UModal from '@nuxt/ui/components/Modal.vue'
import { pickFolder } from '@/lib/folders'
import { icon } from '@/lib/icons'
import { navigate, openProject } from '@/lib/nav'
import { browser, fmt, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore, type ProjectDialog } from '@/stores/projects'

// The dialogs of a project's menu (ProjectMenu.vue): members, the invite, the
// shared name, this member's folder, deleting the project here (leaving).
const projects = useProjectsStore()
const inbox = useInboxStore()
const app = useAppStore()

const view = computed(() => projects.byID(projects.dialogProject))
const name = computed(() => view.value?.display || t("projects.connecting"))
const result = ref('')
const busy = ref(false)

function openFor(kind: ProjectDialog) {
  return computed({
    get: () => projects.dialog === kind && !!view.value,
    set: (open: boolean) => { if (!open) projects.closeDialog() },
  })
}
const membersOpen = openFor('members')
const inviteOpen = openFor('invite')
const nameOpen = openFor('name')
const folderOpen = openFor('folder')
const leaveOpen = openFor('leave')

// Every dialog starts clean, filled from the project as it is now.
const addr = ref('')
const newName = ref('')
const dir = ref('')
const alias = ref('')
const shown = ref(false)
watch(() => [projects.dialog, projects.dialogProject] as const, () => {
  result.value = ''
  busy.value = false
  shown.value = false
  addr.value = ''
  newName.value = view.value?.name || ''
  dir.value = view.value?.dir || ''
  alias.value = view.value?.alias || ''
})

async function run(action: () => Promise<unknown>) {
  if (busy.value) return
  busy.value = true
  result.value = ''
  try {
    await action()
  } catch (error) {
    result.value = (error as Error).message
  } finally {
    busy.value = false
  }
}

// --- members ---

// The project's active chat: its owner invites project members into it and
// removes them from it, here (the chat has no header of its own).
const active = computed(() => inbox.activeChat(projects.dialogProject))
const canManageChat = computed(() => {
  const c = active.value
  return !!c && c.mode === 'project' && !!app.self && c.owner === app.self && !c.closed && !c.removed
})

const members = computed(() => (view.value?.members || []).map((m) => {
  const details: string[] = []
  if (!m.self) details.push(t(m.online ? "participants.online" : "participants.lost"))
  if (!m.self && !m.online && m.seen) details.push(fmt("participants.seen", { when: new Date(m.seen).toLocaleString('ru-RU') }))
  if (m.app) details.push(fmt("participants.version", { version: m.app }))
  if ((m.addrs || []).length) details.push(fmt("participants.addresses", { addresses: m.addrs!.join(', ') }))
  const inChat = (active.value?.participants || []).includes(m.name)
  return {
    key: m.name, self: !!m.self, inChat, name: m.self ? m.name + ' (' + t("inbox.you") + ')' : m.name,
    online: m.self || m.online, details: details.join(' · '),
  }
}))

function chatMembers(name: string, add: boolean) {
  const c = active.value
  if (!c) return
  return run(async () => {
    if (add) await inbox.setMembers(projects.dialogProject, c.id, [name])
    else await inbox.confirmRemove(projects.dialogProject, c.id, name)
  })
}

// removeMember removes a member from the whole project, after a confirmation.
function removeMember(name: string) {
  if (!browser.confirm(fmt("project.members.remove_confirm", { name }))) return
  return run(async () => {
    await projects.removeMember(projects.dialogProject, name)
    result.value = fmt("project.members.removed", { name })
  })
}

function addMember() {
  const value = addr.value.trim()
  if (!value) { result.value = t("participants.add.empty"); return }
  return run(async () => {
    await projects.addMember(projects.dialogProject, value)
    addr.value = ''
    result.value = fmt("participants.add.added", { addr: value })
  })
}

// --- the invite: hidden until the eye asks for it; the dialog forgets it ---

const invite = computed(() => (projects.inviteFor === projects.dialogProject ? projects.invite : ''))
function toggleInvite() {
  if (shown.value) { shown.value = false; return }
  return run(async () => {
    await projects.revealInvite(projects.dialogProject)
    shown.value = true
  })
}

async function copyInvite() {
  await run(async () => {
    const text = await projects.revealInvite(projects.dialogProject)
    try {
      await navigator.clipboard.writeText(text)
      result.value = t("project.invite.copied")
    } catch {
      shown.value = true
      await nextTick()
      document.querySelector<HTMLInputElement>('#invite_value')?.select()
      result.value = t("project.invite.copy_manual")
    }
  })
}

// --- the shared name ---

function saveName() {
  return run(async () => {
    await projects.rename(projects.dialogProject, newName.value.trim())
    projects.closeDialog()
  })
}

// --- this member's folder and own name for the project ---

async function pick() {
  const r = await pickFolder(dir.value)
  if (r.path) dir.value = r.path
  else result.value = r.message || ''
}

function saveFolder() {
  return run(async () => {
    // The legacy network has no own name: its binding takes the working folder only.
    const body: { dir: string; alias?: string } = { dir: dir.value.trim() }
    if (!view.value?.legacy) body.alias = alias.value.trim()
    await projects.bind(projects.dialogProject, body)
    projects.closeDialog()
  })
}

// --- this member's local agents (seats): Claude Code and Codex ---

const agentsOpen = openFor('agents')
watch(() => [projects.dialog, projects.dialogProject] as const, ([kind, pid]) => {
  if (kind === 'agents' && pid) void run(() => projects.refreshSeats(pid))
})
const seatRows = computed(() => (projects.seats[projects.dialogProject] || []).map((s) => ({
  ...s,
  statusText: t('project.agents.status.' + s.status),
  pendingText: s.pending?.length ? fmt("project.agents.pending", { n: s.pending.length }) : '',
})))
function seat(action: 'add' | 'start' | 'stop' | 'remove', arg: string) {
  return run(() => projects.seatAction(projects.dialogProject, action, arg))
}
function removeSeat(id: string, label: string) {
  if (!browser.confirm(fmt("project.agents.remove_confirm", { name: label }))) return
  return seat('remove', id)
}

// --- leaving ---

function leave() {
  return run(async () => {
    const next = await projects.leave(projects.dialogProject)
    projects.closeDialog()
    if (next) openProject(next)
    else navigate('welcome')
  })
}
</script>

<template>
  <UModal
    v-model:open="membersOpen"
    :title="t('project.members.title')"
    :description="name"
  >
    <template #body>
      <div class="flex flex-col gap-4">
        <ul
          id="project_members"
          class="flex flex-col gap-2"
        >
          <li
            v-for="m in members"
            :key="m.name"
            class="flex items-start gap-2"
          >
            <span
              class="project-dot mt-2"
              :class="m.online ? 'on' : 'away'"
            />
            <span class="flex min-w-0 flex-1 flex-col">
              <strong class="text-sm text-highlighted">{{ m.name }}</strong>
              <span class="text-xs break-all text-muted">{{ m.details }}</span>
            </span>
            <template v-if="!m.self">
              <UButton
                v-if="canManageChat && !m.inChat"
                :id="'chat_invite_' + m.key"
                :label="t('inbox.members.add_button')"
                :title="t('inbox.members.add')"
                size="xs"
                variant="soft"
                :disabled="busy || inbox.membersBusy"
                @click="chatMembers(m.key, true)"
              />
              <UButton
                v-if="canManageChat && m.inChat"
                :id="'chat_remove_' + m.key"
                :label="t('inbox.members.remove')"
                size="xs"
                color="neutral"
                variant="ghost"
                :disabled="busy || inbox.membersBusy"
                @click="chatMembers(m.key, false)"
              />
              <UButton
                :id="'member_remove_' + m.key"
                :icon="icon('remove')"
                :aria-label="t('project.members.remove') + ': ' + m.key"
                :title="t('project.members.remove')"
                size="xs"
                color="error"
                variant="ghost"
                :disabled="busy"
                @click="removeMember(m.key)"
              />
            </template>
          </li>
        </ul>
        <form
          id="member_add"
          class="flex flex-col gap-1.5"
          @submit.prevent="addMember"
        >
          <label
            for="member_addr"
            class="text-sm font-medium"
          >{{ t("participants.add.label") }}</label>
          <span class="flex gap-2">
            <UInput
              id="member_addr"
              v-model="addr"
              class="flex-1"
              autocomplete="off"
              :placeholder="t('participants.add.placeholder')"
            />
            <UButton
              type="submit"
              :label="t('participants.add.submit')"
              color="neutral"
              variant="outline"
              :disabled="busy"
            />
          </span>
          <p class="hint">
            {{ t("participants.add.hint") }}
          </p>
        </form>
        <p
          class="dialog-result text-sm"
          role="status"
        >
          {{ result }}
        </p>
      </div>
    </template>
  </UModal>

  <UModal
    v-model:open="agentsOpen"
    :title="t('project.agents.title')"
    :description="name"
  >
    <template #body>
      <div class="flex flex-col gap-4">
        <p class="hint">
          {{ t("project.agents.hint") }}
        </p>
        <ul
          id="project_seats"
          class="flex flex-col gap-2"
        >
          <li
            v-if="!seatRows.length"
            class="text-sm text-muted"
          >
            {{ t("project.agents.empty") }}
          </li>
          <li
            v-for="s in seatRows"
            :key="s.id"
            :data-seat="s.id"
            class="flex items-start gap-2"
          >
            <span
              class="project-dot mt-2"
              :class="s.status === 'active' || s.status === 'running' || s.status === 'idle' ? 'on' : 'away'"
            />
            <span class="flex min-w-0 flex-1 flex-col">
              <strong class="text-sm text-highlighted">{{ s.label }}</strong>
              <span class="text-xs break-all text-muted">{{ [s.statusText, s.pendingText].filter(Boolean).join(' · ') }}</span>
              <span
                v-if="s.error"
                class="text-xs break-all text-error"
              >{{ s.error }}</span>
            </span>
            <UButton
              v-if="s.status === 'stopped' || s.error || !s.session_id"
              :id="'seat_start_' + s.id"
              :label="t('project.agents.start')"
              size="xs"
              variant="soft"
              :disabled="busy"
              @click="seat('start', s.id)"
            />
            <UButton
              v-else
              :id="'seat_stop_' + s.id"
              :label="t('project.agents.stop')"
              size="xs"
              color="neutral"
              variant="ghost"
              :disabled="busy"
              @click="seat('stop', s.id)"
            />
            <UButton
              :id="'seat_remove_' + s.id"
              :icon="icon('remove')"
              :aria-label="t('project.agents.remove') + ': ' + s.label"
              :title="t('project.agents.remove')"
              size="xs"
              color="error"
              variant="ghost"
              :disabled="busy"
              @click="removeSeat(s.id, s.label)"
            />
          </li>
        </ul>
        <span class="flex flex-wrap gap-2">
          <UButton
            id="seat_add_claude"
            :label="t('project.agents.add_claude')"
            color="neutral"
            variant="outline"
            :disabled="busy"
            @click="seat('add', 'claude')"
          />
          <UButton
            id="seat_add_codex"
            :label="t('project.agents.add_codex')"
            color="neutral"
            variant="outline"
            :disabled="busy"
            @click="seat('add', 'codex')"
          />
        </span>
        <p
          class="dialog-result text-sm"
          role="status"
        >
          {{ result }}
        </p>
      </div>
    </template>
  </UModal>

  <UModal
    v-model:open="inviteOpen"
    :title="t('project.invite.title')"
    :description="name"
  >
    <template #body>
      <div class="flex flex-col gap-3">
        <p class="hint">
          {{ t(view?.legacy ? "project.invite.hint_legacy" : "project.invite.hint") }}
        </p>
        <span class="flex gap-2">
          <UInput
            id="invite_value"
            :model-value="shown ? invite : '••••••••••••'"
            class="flex-1"
            readonly
            :aria-label="t('project.menu.invite')"
            :ui="{ base: 'font-mono' }"
          />
          <UButton
            id="invite_eye"
            :icon="icon(shown ? 'eyeoff' : 'eye')"
            color="neutral"
            variant="outline"
            :aria-label="t(shown ? 'project.invite.hide' : 'project.invite.show')"
            :title="t(shown ? 'project.invite.hide' : 'project.invite.show')"
            :aria-pressed="shown ? 'true' : 'false'"
            :disabled="busy"
            @click="toggleInvite"
          />
          <UButton
            id="invite_copy"
            :icon="icon('copy')"
            :label="t('project.invite.copy')"
            :disabled="busy"
            @click="copyInvite"
          />
        </span>
        <p
          class="dialog-result text-sm"
          role="status"
        >
          {{ result }}
        </p>
      </div>
    </template>
  </UModal>

  <UModal
    v-model:open="nameOpen"
    :title="t('project.name.title')"
    :description="t('project.name.hint')"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form
        id="name_form"
        class="flex flex-col gap-2"
        @submit.prevent="saveName"
      >
        <label
          for="project_name"
          class="text-sm font-medium"
        >{{ t("project.name.label") }}</label>
        <UInput
          id="project_name"
          v-model="newName"
          autocomplete="off"
          maxlength="80"
        />
        <p
          class="dialog-result text-sm text-error"
          role="status"
        >
          {{ result }}
        </p>
      </form>
    </template>
    <template #footer>
      <UButton
        :label="t('project.cancel')"
        color="neutral"
        variant="ghost"
        @click="projects.closeDialog"
      />
      <UButton
        id="name_save"
        type="submit"
        form="name_form"
        :label="t('project.save')"
        :disabled="busy || !newName.trim()"
      />
    </template>
  </UModal>

  <UModal
    v-model:open="folderOpen"
    :title="t('project.folder.title')"
    :description="t('project.folder.hint')"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form
        id="folder_form"
        class="flex flex-col gap-2"
        @submit.prevent="saveFolder"
      >
        <label
          for="project_dir"
          class="text-sm font-medium"
        >{{ t("project.folder.label") }}</label>
        <span class="flex gap-2">
          <UInput
            id="project_dir"
            v-model="dir"
            class="flex-1"
            autocomplete="off"
            spellcheck="false"
          />
          <UButton
            id="project_dir_pick"
            type="button"
            :label="t('settings.work_dir.pick')"
            color="neutral"
            variant="outline"
            @click="pick"
          />
        </span>
        <p class="hint warn">
          {{ t("project.folder.warn") }}
        </p>
        <template v-if="!view?.legacy">
          <label
            for="project_alias"
            class="mt-2 text-sm font-medium"
          >{{ t("project.alias.label") }}</label>
          <UInput
            id="project_alias"
            v-model="alias"
            autocomplete="off"
            maxlength="64"
            :placeholder="view?.name"
          />
          <p class="hint">
            {{ t("project.alias.hint") }}
          </p>
        </template>
        <p
          class="dialog-result text-sm text-error"
          role="status"
        >
          {{ result }}
        </p>
      </form>
    </template>
    <template #footer>
      <UButton
        :label="t('project.cancel')"
        color="neutral"
        variant="ghost"
        @click="projects.closeDialog"
      />
      <UButton
        id="folder_save"
        type="submit"
        form="folder_form"
        :label="t('project.save')"
        :disabled="busy"
      />
    </template>
  </UModal>

  <UModal
    v-model:open="leaveOpen"
    :title="t(view?.legacy ? 'project.leave.title' : 'project.delete.title')"
    :description="name"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <p class="text-sm">
        {{ view?.legacy ? t("project.leave.text_legacy") : fmt("project.delete.text", { name }) }}
      </p>
      <p
        class="dialog-result mt-2 text-sm text-error"
        role="status"
      >
        {{ result }}
      </p>
    </template>
    <template #footer>
      <UButton
        :label="t('project.cancel')"
        color="neutral"
        variant="ghost"
        @click="projects.closeDialog"
      />
      <UButton
        id="leave_confirm"
        :label="t(view?.legacy ? 'project.leave.submit' : 'project.delete.submit')"
        color="error"
        :disabled="busy"
        @click="leave"
      />
    </template>
  </UModal>
</template>
