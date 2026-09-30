import { beforeEach, describe, expect, it } from 'vitest'
import { AGENT_STATUS_VERSION, SESSIONS, SUBAGENTS, activityDot, agentDot, agentName, agentStateText, agentTime, counted, countsText, duration, groupAgents, groupStateText, olderPeerRow, worstState } from '@/lib/agents'
import { runtime } from '@/lib/runtime'
import { readFileSync } from 'node:fs'

// The real strings (internal/app/strings.go).
const AGENT_STRINGS: Record<string, string> = {
  'inbox.agents.mine': 'Мой {name}',
  'inbox.agents.since_title': 'в этом состоянии с {at}',
  'inbox.agents.doing.thinking': 'думает',
  'inbox.agents.doing.edit': 'пишет код',
  'inbox.agents.doing.command': 'выполняет команду',
  'inbox.agents.doing.read': 'читает код',
  'inbox.agents.doing.waiting': 'ждёт ответа',
  'inbox.agents.doing.subagents': 'ждёт субагентов',
  'inbox.agents.doing.queued': 'ждёт очереди',
  'inbox.agents.doing.idle': 'ждёт вопроса',
  'inbox.agents.doing.paused': 'на паузе',
  'inbox.agents.doing.stopped': 'остановлен',
  'inbox.agents.doing.off': 'запустится по вопросу',
  'inbox.agents.doing.needs_human': 'нужен человек',
  'inbox.agents.subagents.one': '{n} субагент',
  'inbox.agents.subagents.few': '{n} субагента',
  'inbox.agents.subagents.many': '{n} субагентов',
  'inbox.agents.sessions.one': '{n} сессия',
  'inbox.agents.sessions.few': '{n} сессии',
  'inbox.agents.sessions.many': '{n} сессий',
  'inbox.agents.count.one': '{n} агент {p}',
  'inbox.agents.count.few': '{n} агента {p}',
  'inbox.agents.count.many': '{n} агентов {p}',
  'inbox.activity.unknown_count': 'Количество неизвестно',
  'inbox.activity.other': 'Другие',
  'inbox.activity.ago': '{t} назад',
  'inbox.agents.online': 'на связи',
  'inbox.agents.unreachable': 'не на связи',
  'inbox.agents.online_title': 'на связи с {at}',
  'inbox.agents.seen_title': 'последний раз на связи в {at}',
  'inbox.agents.old_peer': 'обновите agent-link у {name} до {v}+, чтобы видеть, что делают агенты',
  'inbox.agents.old_peer_title': 'у {name} сейчас agent-link {app}',
  'inbox.agents.dur.s': '{n} с',
  'inbox.agents.dur.m': '{n} мин',
  'inbox.agents.dur.h': '{n} ч',
  'inbox.agents.dur.d': '{n} дн',
}

beforeEach(() => { runtime.strings = { ...AGENT_STRINGS } })

const row = (a: Parameters<typeof agentStateText>[0], member = 'local', self = true) => agentName(a, member, self) + ' ' + agentStateText(a)

describe('agent rows', () => {
  it('say whose agent it is, what it does and its subagents', () => {
    expect(row({ provider: 'codex', state: 'thinking', subagents: 2 }, 'local', false).replace('local · ', '')).toBe('Codex думает · 2 субагента')
    expect(row({ provider: 'claude', state: 'edit', subagents: 3 }, 'local', false).replace('local · ', '')).toBe('Claude пишет код · 3 субагента')
    expect(row({ provider: 'codex', state: 'waiting', subagents: 5 }, 'local', false).replace('local · ', '')).toBe('Codex ждёт ответа · 5 субагентов')
    expect(row({ provider: 'codex', state: 'thinking', subagents: 2 })).toBe('Мой Codex думает · 2 субагента')
    expect(row({ provider: 'claude', seat: 'Claude 2', state: 'command' }, 'KPECTIK', false)).toBe('KPECTIK · Claude 2 выполняет команду')
    expect(row({ provider: 'codex', state: 'read', subagents: 0 })).toBe('Мой Codex читает код')
    expect(row({ provider: 'claude', seat: 'Claude', state: 'off' })).toBe('Мой Claude запустится по вопросу')
    expect(row({ provider: 'codex', state: 'idle' })).toBe('Мой Codex ждёт вопроса')
  })

  it('pluralize субагент/субагента/субагентов', () => {
    const forms = [1, 2, 4, 5, 11, 12, 21, 22, 25, 101, 111].map((n) => counted(SUBAGENTS, n))
    expect(forms).toEqual(['1 субагент', '2 субагента', '4 субагента', '5 субагентов', '11 субагентов', '12 субагентов',
      '21 субагент', '22 субагента', '25 субагентов', '101 субагент', '111 субагентов'])
  })

  it('phrase an older peer\'s counts as «2 агента Codex»', () => {
    expect(countsText({ codex: 2 })).toBe('2 агента Codex')
    expect(countsText({ claude: 1, codex: 5 })).toBe('1 агент Claude · 5 агентов Codex')
    expect(countsText(undefined)).toBe('Количество неизвестно')
  })

  it('color the dot green at work, yellow idle, grey with no session, and the button by the worst', () => {
    expect(['thinking', 'edit', 'command', 'read', 'waiting', 'subagents', 'queued', 'idle', 'paused', 'needs_human', 'off', 'stopped'].map(agentDot))
      .toEqual(['running', 'running', 'running', 'running', 'running', 'running', 'running', 'idle', 'idle', 'idle', 'off', 'off'])
    expect(['running', 'queued', 'stale', 'waiting', 'presence', 'idle'].map(activityDot))
      .toEqual(['running', 'running', 'idle', 'idle', 'idle', 'idle'])
    expect(worstState(['running', 'running'])).toBe('working')
    expect(worstState(['running', 'idle'])).toBe('idle')
    expect(worstState(['running', 'idle', 'off'])).toBe('off')
    expect(worstState([])).toBe('none')
  })

  it('say an idle agent waits for its subagents', () => {
    expect(agentStateText({ provider: 'claude', state: 'subagents', subagents: 2 })).toBe('ждёт субагентов · 2')
    expect(agentDot('subagents')).toBe('running')
  })

  it('put the sessions of an agent on one row, by the most active of them', () => {
    const codex = (state: string, since: string, subagents = 0) => ({ provider: 'codex', state, since, subagents })
    const groups = groupAgents([
      codex('idle', '2026-09-29T10:00:00Z'),
      codex('command', '2026-09-29T10:01:00Z', 1),
      codex('command', '2026-09-29T10:02:00Z', 2),
      { provider: 'claude', state: 'idle' },
    ], 'KPECTIK', false)
    expect(groups.map((g) => [g.name, g.sessions, g.agent.since, g.agent.subagents])).toEqual([
      ['KPECTIK · Codex', 3, '2026-09-29T10:02:00Z', 3],
      ['KPECTIK · Claude', 1, undefined, 0],
    ])
    expect(groups.map(groupStateText)).toEqual(['выполняет команду · 3 субагента · 3 сессии', 'ждёт вопроса'])
    // Waiting for subagents beats idle; the main agent at work beats both.
    expect(groupAgents([codex('idle', ''), codex('subagents', '', 1)], 'x', true)[0]!.agent.state).toBe('subagents')
    expect(groupAgents([codex('subagents', '', 1), codex('read', '')], 'x', true)[0]!.agent.state).toBe('read')
    // Seats keep their own rows.
    expect(groupAgents([{ provider: 'claude', seat: 'Claude', state: 'off' }, { provider: 'claude', seat: 'Claude 2', state: 'idle' }], 'x', true)
      .map((g) => g.name)).toEqual(['Мой Claude', 'Мой Claude 2'])
    expect([1, 2, 5].map((n) => counted(SESSIONS, n))).toEqual(['1 сессия', '2 сессии', '5 сессий'])
  })

  it('time a state since it began, not a pause or an old turn', () => {
    const now = Date.parse('2026-09-29T10:01:05Z')
    expect(agentTime({ provider: 'codex', state: 'edit', since: '2026-09-29T10:00:00Z' }, now).time).toBe('1 мин')
    expect(agentTime({ provider: 'codex', state: 'off', since: '2026-09-29T10:00:00Z' }, now).time).toBe('')
    expect(agentTime({ provider: 'codex', state: 'idle' }, now).time).toBe('')
  })

  it('say a span in one whole unit, never like a clock', () => {
    expect([0, 12_000, 59_999, 60_000, 177_000, 3_599_000, 3_600_000, 10_800_000, 86_399_000, 86_400_000, 4 * 86_400_000].map(duration))
      .toEqual(['0 с', '12 с', '59 с', '1 мин', '2 мин', '59 мин', '1 ч', '3 ч', '23 ч', '1 дн', '4 дн'])
  })

  it('show an older peer on the line with its counts and what to update', () => {
    const now = Date.parse('2026-09-29T22:37:00Z')
    const on = olderPeerRow({ name: 'KPECTIK', counts: { codex: 3 }, online: true, seen: '2026-09-29T22:34:30Z', app: '0.6.41' }, now)
    expect(on).toMatchObject({ dot: 'idle', state: '3 агента Codex · на связи', time: '2 мин', noteTitle: 'у KPECTIK сейчас agent-link 0.6.41' })
    expect(on.title).toMatch(/^на связи с \d\d:\d\d/)
    expect(on.note).toBe('обновите agent-link у KPECTIK до 0.6.42+, чтобы видеть, что делают агенты')
    expect(AGENT_STATUS_VERSION).toBe('0.6.42')
    const off = olderPeerRow({ name: 'bob', counts: { claude: 1 }, online: false, seen: '2026-09-29T19:37:00Z' }, now)
    expect(off).toMatchObject({ dot: 'off', state: '1 агент Claude · не на связи', time: '3 ч назад', noteTitle: '' })
    expect(off.title).toMatch(/^последний раз на связи в /)
    expect(olderPeerRow({ name: 'x', online: true }, now)).toMatchObject({ state: 'Количество неизвестно · на связи', time: '—', title: '' })
  })
})

describe('agent dot colours', () => {
  const styles = readFileSync('src/assets/styles.css', 'utf8') // vitest runs in internal/app/web
  // Yellow for idle (its own token), amber only for a pause: the two never look alike.
  it('paints idle yellow and paused amber, in the rows and on the button', () => {
    expect(styles).toContain('.agent-row.idle .agent-dot { background: var(--app-dot-one); }')
    expect(styles).toContain('.agent-row.paused .agent-dot { background: var(--app-off); }')
    expect(styles).toContain('.chat-tool-dot.idle { background: var(--app-dot-one); }')
    expect(styles).toContain('.chat-tool-dot.paused { background: var(--app-off); }')
  })
})
