<script setup lang="ts">
import { computed } from 'vue'
import type { DropdownMenuItem } from '@nuxt/ui'
import UButton from '@nuxt/ui/components/Button.vue'
import UDropdownMenu from '@nuxt/ui/components/DropdownMenu.vue'
import { icon } from '@/lib/icons'
import { fmt, t } from '@/lib/runtime'
import { useProjectsStore, type ProjectDialog } from '@/stores/projects'
import type { ProjectView } from '@/types'

// A project's "⋯" menu: members, the invite, the shared name, this member's
// folder, and leaving. Each item opens its dialog (ProjectDialogs.vue).
const props = defineProps<{ project: ProjectView; name: string }>()
const projects = useProjectsStore()

const items = computed<DropdownMenuItem[][]>(() => {
  const p = props.project
  const item = (kind: ProjectDialog, label: string, name: string, color?: 'error') => ({
    label: t(label), icon: icon(name), color, onSelect: () => projects.openDialog(kind, p.id),
  })
  const main = [item('members', "project.menu.members", 'participants'), item('invite', "project.menu.invite", 'invite')]
  if (p.can_rename) main.push(item('name', "project.menu.name", 'rename'))
  main.push(item('folder', "project.menu.folder", 'folder'))
  return [main, [item('leave', "project.menu.leave", 'leave', 'error')]]
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
