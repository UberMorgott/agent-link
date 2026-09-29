// The local chats section of the sidebar: every local binding grouped by its
// project, temporary chats only while someone is in them
// (docs/notes/agent-chats-design.md R6, R7).
import { t } from '@/lib/runtime'
import type { ProjectView } from '@/types'

// HIDE_GRACE_MS: a temporary chat stays this long after it was last seen live
// or last active, so it does not flicker away between two turns.
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

// chatVisible: project chats and named topics always show; a temporary chat
// while live, and for HIDE_GRACE_MS after it was last seen live or active.
// An app without the live flag shows it as before.
export function chatVisible(p: ProjectView, now: number, lastLive: ReadonlyMap<string, number>): boolean {
  const lc = p.local_chat
  if (!lc || !isTemporary(p) || lc.live === undefined || lc.live) return true
  const active = Date.parse(lc.last_active || '') || 0
  return now - Math.max(active, lastLive.get(p.id) || 0) < HIDE_GRACE_MS
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
export function groupLocalChats(list: ProjectView[], now: number, lastLive: ReadonlyMap<string, number>): LocalChatGroupView[] {
  const local = list.filter((p) => p.scope === 'local')
  const byKey = new Map<string, ProjectView[]>()
  for (const p of local) {
    if (!chatVisible(p, now, lastLive)) continue
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
