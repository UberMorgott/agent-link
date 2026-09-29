// The agents popover's rows: one per agent of a member (node.AgentStatus),
// each as who it is («Мой Codex», «KPECTIK · Claude 2»), what it does now
// («пишет код · 3 субагента») and since when. Older peers send per-provider
// counts alone: one row per member then («2 агента Codex · на связи») with a
// note to update it.

import { clock } from '@/lib/chat'
import { fmt, t } from '@/lib/runtime'
import type { AgentCounts, AgentStatus } from '@/types'

// AGENT_STATUS_VERSION: the first app release whose node reports its agents one
// by one (node.CapAgentStatus, agent-status-v1); an older peer sends counts alone.
export const AGENT_STATUS_VERSION = '0.6.42'

// States at work: they count on the agents button.
export const BUSY_STATES = ['thinking', 'edit', 'command', 'read', 'waiting']
// States with no time worth showing (it would be a pause's or an old turn's).
const TIMELESS = ['paused', 'stopped', 'off', 'needs_human']

const plural = new Intl.PluralRules('ru')

// Plural forms of a counted phrase: the strings keys of one, few and many.
export interface Forms { one: string; few: string; many: string }
export const SUBAGENTS: Forms = { one: 'inbox.agents.subagents.one', few: 'inbox.agents.subagents.few', many: 'inbox.agents.subagents.many' }
export const AGENTS: Forms = { one: 'inbox.agents.count.one', few: 'inbox.agents.count.few', many: 'inbox.agents.count.many' }

// counted is n in its plural form: «1 субагент», «2 субагента», «5 субагентов».
export function counted(forms: Forms, n: number, vars: Record<string, string | number> = {}): string {
  const form = plural.select(n)
  return fmt(form === 'one' || form === 'few' ? forms[form] : forms.many, { n, ...vars })
}

export function providerName(provider: string): string {
  if (provider === 'claude') return 'Claude'
  if (provider === 'codex') return 'Codex'
  return provider || t('inbox.activity.other')
}

// agentName: a seat by its label, a session by its provider; this computer's
// as «Мой …», another member's with the member's name first.
export function agentName(agent: AgentStatus, member: string, self: boolean): string {
  const label = agent.seat || providerName(agent.provider)
  return self ? fmt('inbox.agents.mine', { name: label }) : member + ' · ' + label
}

// agentStateText: the state's verb, then the subagents when there are any.
export function agentStateText(agent: AgentStatus): string {
  const verb = t('inbox.agents.doing.' + agent.state)
  const subs = agent.subagents || 0
  return subs > 0 ? verb + ' · ' + counted(SUBAGENTS, subs) : verb
}

// The row's dot: running (at work), waiting (for an answer), idle, off, paused.
export function agentDot(state: string): string {
  if (state === 'waiting') return 'waiting'
  if (BUSY_STATES.includes(state)) return 'running'
  if (state === 'paused' || state === 'stopped' || state === 'needs_human') return 'paused'
  if (state === 'off' || state === 'queued') return 'off'
  return 'idle'
}

export function agentTime(agent: AgentStatus, now: number): { time: string; title: string } {
  const at = Date.parse(agent.since || '')
  if (Number.isNaN(at) || TIMELESS.includes(agent.state)) return { time: '', title: '' }
  return { time: duration(now - at), title: fmt('inbox.agents.since_title', { at: clock(agent.since) }) }
}

// countsText: an older peer's agents by provider («2 агента Codex · 1 агент Claude»).
export function countsText(counts: AgentCounts | undefined): string {
  if (!counts) return t('inbox.activity.unknown_count')
  return ([['claude', counts.claude], ['codex', counts.codex], ['other', counts.other]] as const)
    .filter(([, n]) => (n || 0) > 0)
    .map(([p, n]) => counted(AGENTS, n!, { p: providerName(p === 'other' ? '' : p) }))
    .join(' · ')
}

// duration is a span in one whole unit, never a clock: «12 с», «3 мин», «2 ч», «4 дн».
export function duration(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000))
  if (s < 60) return fmt('inbox.agents.dur.s', { n: s })
  if (s < 3600) return fmt('inbox.agents.dur.m', { n: Math.floor(s / 60) })
  if (s < 86400) return fmt('inbox.agents.dur.h', { n: Math.floor(s / 3600) })
  return fmt('inbox.agents.dur.d', { n: Math.floor(s / 86400) })
}

// A member with no per-agent status (an older peer): its counts, whether it is
// on the line and since when, and what to update to see more.
export interface PeerInfo { name: string; counts?: AgentCounts; online: boolean; seen?: string; app?: string }
export function olderPeerRow(peer: PeerInfo, now: number): { dot: string; state: string; time: string; title: string; note: string; noteTitle: string } {
  const presence = t(peer.online ? 'inbox.agents.online' : 'inbox.agents.unreachable')
  const at = Date.parse(peer.seen || '')
  const known = !Number.isNaN(at)
  const span = known ? duration(now - at) : ''
  return {
    dot: peer.online ? 'online' : 'off',
    state: countsText(peer.counts) + ' · ' + presence,
    time: !known ? '—' : peer.online ? span : fmt('inbox.activity.ago', { t: span }),
    title: known ? fmt(peer.online ? 'inbox.agents.online_title' : 'inbox.agents.seen_title', { at: clock(peer.seen) }) : '',
    note: olderPeerNote(peer.name),
    noteTitle: peer.app ? fmt('inbox.agents.old_peer_title', { name: peer.name, app: peer.app }) : '',
  }
}

// olderPeerNote: what to do to see an older peer's agents one by one.
export function olderPeerNote(name: string): string {
  return fmt('inbox.agents.old_peer', { name, v: AGENT_STATUS_VERSION })
}

// The toolbar dot: the most active state of all rows (a peer on the line with
// agents but no status reads as idle).
export function mostActive(dots: string[]): string {
  dots = dots.map((d) => d === 'online' ? 'idle' : d)
  for (const d of ['running', 'waiting', 'idle', 'off', 'paused']) if (dots.includes(d)) return d === 'running' ? 'working' : d
  return 'none'
}
