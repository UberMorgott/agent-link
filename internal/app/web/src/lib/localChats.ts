// The local chats section of the sidebar: every local binding grouped by its
// project, each only while its agents are at work
// (docs/notes/agent-chats-design.md R6, R7).
import { onScopeDispose, ref, watch } from 'vue'
import { t } from '@/lib/runtime'
import type { ProjectView } from '@/types'

// HIDE_GRACE_MS: a chat stays this long after its agents stopped (the app's
// live_ended_at), so it does not flicker away between two turns.
export const HIDE_GRACE_MS = 60_000

// NO_PROJECT is the group key of chats outside any project folder.
export const NO_PROJECT = ''

export interface LocalChatGroupView {
  key: string // the local project's id, NO_PROJECT for folderless chats
  name: string
  items: ProjectView[] // the project's own chat first
}

export function isTemporary(p: ProjectView): boolean {
  const scope = p.local_chat?.scope
  return scope === 'project_temporary' || scope === 'folderless_temporary'
}

// liveState: 'waiting' (a caller waits for a reply), 'live' (a turn runs or
// is queued) or '' (nothing at work).
export function liveState(p: ProjectView): '' | 'live' | 'waiting' {
  const state = p.local_chat || p.activity
  if (!state?.live) return ''
  return state.waiting ? 'waiting' : 'live'
}

// graceEnd is when a chat that stopped being live leaves (0: live, or never live).
function graceEnd(p: ProjectView): number {
  const state = p.local_chat || p.activity
  if (!state || state.live) return 0
  const ended = Date.parse(state.live_ended_at || '')
  return Number.isNaN(ended) ? 0 : ended + HIDE_GRACE_MS
}

// chatVisible: a local chat (a folder's project chat, a topic, a temporary
// chat) shows while live and for HIDE_GRACE_MS after, else only while keep
// holds it (its unread messages, the chat on screen). A retired chat never
// shows (its reply shows as needs_human).
export function chatVisible(p: ProjectView, now: number, keep?: (p: ProjectView) => boolean): boolean {
  if (p.local_chat?.retired) return false
  if (liveState(p) || keep?.(p)) return true
  return now < graceEnd(p)
}

// useGraceClock is the time local chats are shown at: it moves on when the
// next grace of list ends, so a chat leaves then without waiting for an event.
export function useGraceClock(list: () => ProjectView[]) {
  const now = ref(Date.now())
  let timer: ReturnType<typeof setTimeout> | undefined
  const schedule = () => {
    clearTimeout(timer)
    now.value = Date.now()
    const next = Math.min(...list().map(graceEnd).filter((at) => at > now.value))
    if (Number.isFinite(next)) timer = setTimeout(schedule, next - now.value)
  }
  watch(list, schedule, { immediate: true })
  onScopeDispose(() => clearTimeout(timer))
  return now
}

function groupKey(p: ProjectView): string {
  return p.local_chat ? p.local_chat.project || NO_PROJECT : p.id
}

// chatLabel is how a chat shows under its project: the project's own chat,
// a topic, or whose temporary chat it is.
export function chatLabel(p: ProjectView): string {
  const lc = p.local_chat
  if (!lc) return t('local_chat.project_chat')
  if (!isTemporary(p)) return lc.topic || p.display
  const o = lc.owner
  if (!o) return lc.topic || p.display
  const provider = o.provider === 'claude' ? 'Claude Code' : o.provider === 'codex' ? 'Codex' : ''
  const who = o.agent_type || (o.agent ? t('local_chat.subagent') : provider || t('local_chat.main'))
  return lc.topic ? who + ' · ' + lc.topic : who
}

// groupLocalChats groups the visible local bindings by project: groups by
// name, the chats outside projects last; in a group the project's own chat
// first, then the rest by name.
export function groupLocalChats(list: ProjectView[], now: number, keep?: (p: ProjectView) => boolean): LocalChatGroupView[] {
  const local = list.filter((p) => p.scope === 'local')
  const byKey = new Map<string, ProjectView[]>()
  for (const p of local) {
    if (!chatVisible(p, now, keep)) continue
    const key = groupKey(p)
    byKey.set(key, [...(byKey.get(key) || []), p])
  }
  const groups: LocalChatGroupView[] = []
  for (const [key, items] of byKey) {
    const head = key === NO_PROJECT ? undefined : local.find((p) => p.id === key)
    items.sort((a, b) => Number(!!a.local_chat) - Number(!!b.local_chat) || chatLabel(a).localeCompare(chatLabel(b), 'ru'))
    groups.push({ key, name: key === NO_PROJECT ? t('local_chat.no_project') : head?.display || items[0]!.display, items })
  }
  return groups.sort((a, b) => Number(a.key === NO_PROJECT) - Number(b.key === NO_PROJECT) || a.name.localeCompare(b.name, 'ru'))
}
