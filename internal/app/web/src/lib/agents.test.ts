import { beforeEach, describe, expect, it } from 'vitest'
import { SUBAGENTS, agentDot, agentName, agentStateText, agentTime, counted, countsText, mostActive } from '@/lib/agents'
import { runtime } from '@/lib/runtime'

// The real strings (internal/app/strings.go).
const AGENT_STRINGS: Record<string, string> = {
  'inbox.agents.mine': 'Мой {name}',
  'inbox.agents.since_title': 'в этом состоянии с {at}',
  'inbox.agents.doing.thinking': 'думает',
  'inbox.agents.doing.edit': 'пишет код',
  'inbox.agents.doing.command': 'выполняет команду',
  'inbox.agents.doing.read': 'читает код',
  'inbox.agents.doing.waiting': 'ждёт ответа',
  'inbox.agents.doing.queued': 'ждёт очереди',
  'inbox.agents.doing.idle': 'ждёт вопроса',
  'inbox.agents.doing.paused': 'на паузе',
  'inbox.agents.doing.stopped': 'остановлен',
  'inbox.agents.doing.off': 'запустится по вопросу',
  'inbox.agents.doing.needs_human': 'нужен человек',
  'inbox.agents.subagents.one': '{n} субагент',
  'inbox.agents.subagents.few': '{n} субагента',
  'inbox.agents.subagents.many': '{n} субагентов',
  'inbox.agents.count.one': '{n} агент {p}',
  'inbox.agents.count.few': '{n} агента {p}',
  'inbox.agents.count.many': '{n} агентов {p}',
  'inbox.activity.unknown_count': 'Количество неизвестно',
  'inbox.activity.other': 'Другие',
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

  it('color the dot by state and the button by the most active one', () => {
    expect(['thinking', 'edit', 'command', 'read', 'waiting', 'idle', 'off', 'paused', 'queued'].map(agentDot))
      .toEqual(['running', 'running', 'running', 'running', 'waiting', 'idle', 'off', 'paused', 'off'])
    expect(mostActive(['idle', 'waiting', 'running'])).toBe('working')
    expect(mostActive(['idle', 'waiting'])).toBe('waiting')
    expect(mostActive(['off', 'idle'])).toBe('idle')
    expect(mostActive([])).toBe('none')
  })

  it('time a state since it began, not a pause or an old turn', () => {
    const now = Date.parse('2026-09-29T10:01:05Z')
    expect(agentTime({ provider: 'codex', state: 'edit', since: '2026-09-29T10:00:00Z' }, now).time).toBe('1:05')
    expect(agentTime({ provider: 'codex', state: 'off', since: '2026-09-29T10:00:00Z' }, now).time).toBe('')
    expect(agentTime({ provider: 'codex', state: 'idle' }, now).time).toBe('')
  })
})
