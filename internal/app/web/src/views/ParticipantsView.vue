<script setup lang="ts">
import { nextTick, ref } from 'vue'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import { api } from '@/lib/api'
import { navigate } from '@/lib/nav'
import { browser, fmt, t } from '@/lib/runtime'
import { useAppStore } from '@/stores/app'
import type { ParticipantView, Status } from '@/types'

const app = useAppStore()
const addr = ref('')
const result = ref('')
const adding = ref(false)
const removing = ref<string[]>([])
const list = ref<HTMLElement | null>(null)
const addrInput = ref<{ $el: HTMLInputElement } | null>(null)

function participantDetail(person: ParticipantView): string[] {
  const details: string[] = []
  if (!person.online && person.seen) details.push(fmt("participants.seen", { when: new Date(person.seen).toLocaleString('ru-RU') }))
  if (person.app) details.push(fmt("participants.version", { version: person.app }))
  if (person.old_auth) details.push(t("participants.old_auth"))
  else if (person.legacy) details.push(t("participants.legacy"))
  if ((person.addrs || []).length) details.push(fmt("participants.addresses", { addresses: person.addrs!.join(', ') }))
  return details
}

async function refreshParticipantViews() {
  await Promise.all([app.refreshSlice('participants'), app.refreshSlice('dashboard')])
}

async function addParticipant() {
  if (adding.value) return
  const value = addr.value.trim()
  if (!value) {
    result.value = t("participants.add.empty")
    return
  }
  adding.value = true
  try {
    app.status = await api<Status>('POST', 'members/add', { addr: value })
    addr.value = ''
    result.value = fmt("participants.add.added", { addr: value })
    await refreshParticipantViews()
  } catch (e) {
    result.value = (e as Error).message
  } finally {
    adding.value = false
  }
}

async function removeParticipant(name: string, index: number) {
  if (removing.value.includes(name)) return
  if (!browser.confirm(fmt("participants.confirm", { name }))) return
  removing.value = [...removing.value, name]
  try {
    app.status = await api<Status>('POST', 'members/remove', { name })
    result.value = fmt("participants.removed", { name })
    await refreshParticipantViews()
    await nextTick()
    const next = list.value?.querySelectorAll<HTMLElement>('.participant-main') || []
    if (next.length) next[Math.min(index, next.length - 1)]!.focus()
    else addrInput.value?.$el.focus()
  } catch (e) {
    result.value = (e as Error).message
  } finally {
    removing.value = removing.value.filter((n) => n !== name)
  }
}
</script>

<template>
  <section
    data-view="participants"
    class="h-full overflow-y-auto"
  >
    <div class="mx-auto flex max-w-3xl flex-col gap-8 px-4 py-10">
      <header class="view-header">
        <h1>{{ t("participants.h1") }}</h1>
      </header>
      <form
        id="participant_add"
        class="participant-add flex flex-col gap-2"
        novalidate
        :aria-busy="adding ? 'true' : undefined"
        @submit.prevent="addParticipant"
      >
        <h2>{{ t("participants.add.legend") }}</h2>
        <label class="flex flex-col gap-1.5">
          <span class="text-sm">{{ t("participants.add.label") }}</span>
          <span class="flex gap-2">
            <InputText
              id="participant_addr"
              ref="addrInput"
              v-model="addr"
              name="participant_addr"
              autocomplete="off"
              spellcheck="false"
              class="flex-1"
              :placeholder="t('participants.add.placeholder')"
              :disabled="adding"
            />
            <Button
              id="add_participant"
              type="submit"
              :label="t('participants.add.submit')"
              :disabled="adding"
            />
          </span>
        </label>
        <p class="hint">
          {{ t("participants.add.hint") }}
        </p>
        <p
          id="participants_result"
          role="status"
          class="text-sm"
        >
          {{ result }}
        </p>
      </form>
      <p
        v-if="app.participants && !app.participants.length"
        id="participants_empty"
        class="hint"
      >
        {{ t("participants.empty") }}
      </p>
      <ul
        id="participants"
        ref="list"
        class="participant-list flex flex-col divide-y divide-[var(--app-line)]"
      >
        <li
          v-for="(person, index) in app.participants || []"
          :key="person.name"
          class="participant-card flex items-center gap-3 py-3"
          :aria-busy="removing.includes(person.name) ? 'true' : undefined"
        >
          <button
            type="button"
            class="participant-main flex min-w-0 flex-1 cursor-pointer flex-col items-start gap-0.5 rounded-lg px-2 py-1 text-left hover:bg-[var(--app-soft)]"
            :aria-label="fmt('participants.open_chat', { name: person.name })"
            @click="navigate('inbox', { peer: person.name })"
          >
            <span class="participant-heading flex items-baseline gap-2">
              <strong>{{ person.name }}</strong>
              <span
                class="text-xs"
                :class="person.online ? 'on' : 'off'"
              >{{ t(person.online ? "participants.online" : "participants.lost") }}</span>
            </span>
            <span class="participant-counts text-sm text-[var(--app-muted)]">
              {{ fmt("participants.counts", { sent: person.sent || 0, received: person.received || 0, total: person.total || 0 }) }}
            </span>
            <span
              v-if="participantDetail(person).length"
              class="participant-detail text-xs text-[var(--app-muted)]"
            >
              {{ participantDetail(person).join(" · ") }}
            </span>
          </button>
          <Button
            class="participant-remove"
            :label="t('participants.remove')"
            :aria-label="fmt('participants.remove_named', { name: person.name })"
            severity="danger"
            text
            size="small"
            :disabled="removing.includes(person.name)"
            @click="removeParticipant(person.name, index)"
          />
        </li>
      </ul>
    </div>
  </section>
</template>
