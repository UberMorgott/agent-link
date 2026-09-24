<script setup lang="ts">
import UButton from '@nuxt/ui/components/Button.vue'
import UCheckbox from '@nuxt/ui/components/Checkbox.vue'
import UModal from '@nuxt/ui/components/Modal.vue'
import { whoColor } from '@/lib/chat'
import { t } from '@/lib/runtime'
import { useInboxStore } from '@/stores/inbox'

// Starting a chat in a project: whom it is with. The list is fixed once the
// chat is made; who must answer is chosen per message in the composer.
const inbox = useInboxStore()

function toggle(name: string, on: boolean | 'indeterminate') {
  const rest = inbox.newChatChosen.filter((n) => n !== name)
  inbox.newChatChosen = on === true ? [...rest, name] : rest
}
</script>

<template>
  <UModal
    v-model:open="inbox.newChatOpen"
    :title="t('inbox.new.title')"
    :description="t('inbox.new.hint')"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form
        id="new_chat_form"
        class="flex flex-col gap-3"
        :aria-busy="inbox.newChatBusy ? 'true' : undefined"
        @submit.prevent="inbox.createChat"
      >
        <div
          class="flex flex-col gap-2"
          role="group"
          aria-labelledby="new_chat_people"
        >
          <span
            id="new_chat_people"
            class="text-sm font-medium"
          >{{ t("inbox.new.participants") }}</span>
          <span
            id="new_chat_members"
            class="flex flex-col gap-2"
          >
            <UCheckbox
              v-for="person in inbox.newChatPeople"
              :id="'new_chat_' + person.name"
              :key="person.name"
              :label="person.name"
              :model-value="inbox.newChatChosen.includes(person.name)"
              class="choice"
              :class="{ on: person.online }"
              :style="{ '--who': whoColor(person.name) }"
              :ui="{ label: 'text-[var(--who)]' }"
              @update:model-value="toggle(person.name, $event)"
            />
          </span>
        </div>
        <p
          v-if="!inbox.newChatPeople.length"
          id="new_chat_empty"
          class="hint"
        >
          {{ t("inbox.new.no_members") }}
        </p>
        <p
          id="new_chat_result"
          role="status"
          class="text-sm text-error"
        >
          {{ inbox.newChatResult }}
        </p>
      </form>
    </template>
    <template #footer>
      <UButton
        id="new_chat_dismiss"
        type="button"
        :label="t('inbox.new.dismiss')"
        color="neutral"
        variant="ghost"
        @click="inbox.hideNewChat"
      />
      <UButton
        id="new_chat_create"
        type="submit"
        form="new_chat_form"
        :label="t('inbox.new.create')"
        :disabled="!inbox.newChatChosen.length || inbox.newChatBusy"
      />
    </template>
  </UModal>
</template>
