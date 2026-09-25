<script setup lang="ts">
import { computed } from 'vue'
import type { DropdownMenuItem } from '@nuxt/ui'
import UButton from '@nuxt/ui/components/Button.vue'
import UDropdownMenu from '@nuxt/ui/components/DropdownMenu.vue'
import { icon } from '@/lib/icons'
import { browser, fmt, t } from '@/lib/runtime'
import { chatKey, useInboxStore } from '@/stores/inbox'
import { useProjectsStore, type ProjectDialog } from '@/stores/projects'
import type { ProjectView } from '@/types'

// A project's "⋯" menu, the project's own controls (the chat has no header):
// archiving the history of its chat, members, the invite, the shared name,
// this member's folder, and deleting the project here (leaving it).
const props = defineProps<{ project: ProjectView; name: string }>()
const projects = useProjectsStore()
const inbox = useInboxStore()

// run does a menu action; a failure is said at once, whatever is on screen.
async function run(action: () => Promise<unknown> | undefined) {
  try { await action() } catch (error) { browser.alert((error as Error).message) }
}

const items = computed<DropdownMenuItem[][]>(() => {
  const p = props.project
  const item = (kind: ProjectDialog, label: string, name: string, color?: 'error') => ({
    label: t(label), icon: icon(name), color, onSelect: () => projects.openDialog(kind, p.id),
  })
  const chat: DropdownMenuItem[] = []
  if (!p.legacy) {
    chat.push({
      label: t("inbox.archive_history"), icon: icon('archive'), disabled: !inbox.activeChat(p.id) || inbox.closing,
      onSelect: () => run(() => inbox.confirmArchive(p.id)),
    })
  }
  // A chat of the network from before projects is closed instead; the open one.
  const open = inbox.chat
  if (open && inbox.openKey() === chatKey(p.id, open.id) && (open.legacy || p.legacy) && !open.closed && !open.archived) {
    chat.push({
      label: t(open.legacy ? "inbox.close.legacy" : "inbox.close"), icon: icon(open.legacy ? 'archive' : 'finish'), disabled: inbox.closing,
      onSelect: () => run(() => inbox.confirmClose()),
    })
  }
  const main: DropdownMenuItem[] = [item('members', "project.menu.members", 'participants'), item('invite', "project.menu.invite", 'invite')]
  // This member's local agents (Claude Code, Codex) in the project's conversation.
  if (!p.legacy) main.push(item('agents', "project.menu.agents", 'agent'))
  if (p.can_rename) main.push(item('name', "project.menu.name", 'rename'))
  main.push(item('folder', "project.menu.folder", 'folder'))
  if (!p.legacy) {
    // Off by default: a message then waits for a session of this member.
    main.push({
      label: t("project.menu.auto_open"), type: 'checkbox', checked: !!p.auto_open,
      onUpdateChecked: (on: boolean) => { void run(() => projects.bind(p.id, { auto_open: on })) },
    })
  }
  const leave = p.legacy ? item('leave', "project.menu.leave", 'leave', 'error') : item('leave', "project.menu.delete", 'delete', 'error')
  return [...(chat.length ? [chat] : []), main, [leave]]
})
</script>

<template>
  <UDropdownMenu
    :items="items"
    :content="{ align: 'start' }"
    :ui="{ content: 'z-50' }"
  >
    <UButton
      class="project-more"
      :icon="icon('more')"
      color="neutral"
      variant="ghost"
      size="xs"
      :aria-label="fmt('project.menu', { name })"
      :title="fmt('project.menu', { name })"
    />
  </UDropdownMenu>
</template>
