// The agents popover's rows: one per agent of a member (node.AgentStatus),
// each as who it is («Мой Codex», «KPECTIK · Claude 2»), what it does now
// («пишет код · 3 субагента») and since when. Older peers send per-provider
// counts alone: one row per member then («2 агента Codex»).

import { elapsed, clock } from '@/lib/chat'
import { fmt, t } from '@/lib/runtime'
import type { AgentCounts, AgentStatus } from '@/types'

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
  return { time: elapsed(now - at), title: fmt('inbox.agents.since_title', { at: clock(agent.since) }) }
}

// countsText: an older peer's agents by provider («2 агента Codex · 1 агент Claude»).
export function countsText(counts: AgentCounts | undefined): string {
  if (!counts) return t('inbox.activity.unknown_count')
  return ([['claude', counts.claude], ['codex', counts.codex], ['other', counts.other]] as const)
    .filter(([, n]) => (n || 0) > 0)
    .map(([p, n]) => counted(AGENTS, n!, { p: providerName(p === 'other' ? '' : p) }))
    .join(' · ')
}

// The toolbar dot: the most active state of all rows.
export function mostActive(dots: string[]): string {
  for (const d of ['running', 'waiting', 'idle', 'off', 'paused']) if (dots.includes(d)) return d === 'running' ? 'working' : d
  return 'none'
}
