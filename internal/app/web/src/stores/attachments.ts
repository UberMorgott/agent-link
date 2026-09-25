import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api, projectPath } from '@/lib/api'
import { clipName, MAX_FILES, MAX_SIZE, MAX_TOTAL } from '@/lib/attachments'
import { fmt } from '@/lib/runtime'
import type { Attachment } from '@/types'

// A file in the composer: uploaded to the app as soon as it is added, so the
// message only names it. key tells chips apart while one is still uploading.
export interface Pending {
  key: number
  name: string
  size: number
  project: string
  attachment?: Attachment // set once uploaded
  error?: string
}

// The composer's files of the open chat: pasted, dropped or picked, shown as
// chips until the message is sent (clear) or the chat changes.
export const useAttachmentsStore = defineStore('attachments', () => {
  const items = ref<Pending[]>([])
  const error = ref('')
  let next = 0

  const uploading = computed(() => items.value.some((p) => !p.attachment && !p.error))
  // What a send carries: the uploaded files, id and name.
  const ready = computed(() => items.value.filter((p) => p.attachment).map((p) => ({ id: p.attachment!.id, name: p.attachment!.name })))

  // add uploads files for project, within the limits; a refused one leaves a
  // line in error and no chip.
  function add(project: string, files: File[]) {
    error.value = ''
    for (const f of files) {
      if (items.value.length >= MAX_FILES) {
        error.value = fmt("inbox.attach.too_many", { n: MAX_FILES })
        return
      }
      const name = clipName(f)
      if (f.size > MAX_SIZE) {
        error.value = fmt("inbox.attach.too_big", { name })
        continue
      }
      if (items.value.reduce((sum, p) => sum + p.size, 0) + f.size > MAX_TOTAL) {
        error.value = fmt("inbox.attach.too_much", {})
        return
      }
      const item: Pending = { key: ++next, name, size: f.size, project }
      items.value = [...items.value, item]
      void upload(item, f)
    }
  }

  async function upload(item: Pending, f: File) {
    try {
      const att = await api<Attachment>('POST', projectPath(item.project, 'attachments?name=' + encodeURIComponent(item.name)), f)
      patch(item.key, { attachment: att, name: att.name })
    } catch (e) {
      // The chip goes; the reason stays under the composer.
      error.value = item.name + ': ' + (e as Error).message
      remove(item.key)
    }
  }

  function patch(key: number, part: Partial<Pending>) {
    items.value = items.value.map((p) => (p.key === key ? { ...p, ...part } : p))
  }

  function remove(key: number) {
    items.value = items.value.filter((p) => p.key !== key)
  }

  function clear() {
    items.value = []
    error.value = ''
  }

  return { items, error, uploading, ready, add, remove, clear }
})
