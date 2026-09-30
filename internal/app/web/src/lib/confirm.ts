// confirmAction asks before an action nobody can take back, in the app's own
// dialog (ConfirmModal.vue, through Nuxt UI's overlay): true once the member
// pressed the action's button, false on «Отмена», Escape or a click outside.
import { useOverlay } from '@nuxt/ui/composables/useOverlay'
import ConfirmModal from '@/components/ConfirmModal.vue'

export async function confirmAction(text: string, action: string): Promise<boolean> {
  const modal = useOverlay().create(ConfirmModal, { destroyOnClose: true })
  return (await modal.open({ text, action })) === true
}
