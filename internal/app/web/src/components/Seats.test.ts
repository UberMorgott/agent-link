import { describe, expect, it, vi } from 'vitest'
import { authorLabel, continues } from '@/lib/chat'
import { browser } from '@/lib/runtime'
import { SITE, fixture } from '@/test/backend'
import { fakeBackend, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'
import type { ChatInfo, ChatMessage } from '@/types'

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))

async function open(path: string) {
  const api = fakeBackend()
  const mounted = await mountApp(path)
  useAppStore().status = { configured: true, node: 'alice' }
  await useProjectsStore().refreshAll()
  await settle()
  return { ...mounted, ...api }
}

describe('local agents (seats)', () => {
  it('adds Claude and Codex in the agents dialog, stops, starts and removes one', async () => {
    const { calls, backend } = await open('/p/' + SITE)
    const projects = useProjectsStore()
    projects.openDialog('agents', SITE)
    await settle()
    expect(calls).toContain('GET projects/' + SITE + '/seats')
    expect(document.body.textContent).toContain('project.agents.empty')
    $<HTMLButtonElement>('#seat_add_claude')!.click()
    await settle()
    $<HTMLButtonElement>('#seat_add_codex')!.click()
    await settle()
    const seats = backend.seats[SITE]!
    expect(seats.map((s) => s.label)).toEqual(['Claude', 'Codex'])
    expect($$('#project_seats [data-seat]')).toHaveLength(2)
    const codex = seats[1]!.id
    $<HTMLButtonElement>('#seat_stop_' + codex)!.click()
    await settle()
    expect(backend.seats[SITE]![1]!.status).toBe('stopped')
    expect($('#seat_start_' + codex)).not.toBeNull()
    $<HTMLButtonElement>('#seat_start_' + codex)!.click()
    await settle()
    expect(backend.seats[SITE]![1]!.status).toBe('closed')
    const confirm = vi.spyOn(browser, 'confirm').mockReturnValue(false)
    $<HTMLButtonElement>('#seat_remove_' + codex)!.click()
    await settle()
    expect(calls).not.toContain('POST projects/' + SITE + '/seats/' + codex + '/remove')
    confirm.mockReturnValue(true)
    $<HTMLButtonElement>('#seat_remove_' + codex)!.click()
    await settle()
    expect(backend.seats[SITE]!.map((s) => s.label)).toEqual(['Claude'])
    expect($$('#project_seats [data-seat]')).toHaveLength(1)
  })

  it('asks a chosen local agent from the composer', async () => {
    const [chat] = fixture<ChatInfo[]>('chats')
    const api = fakeBackend()
    api.backend.seats[SITE] = [
      { id: 'seat-a', provider: 'claude', label: 'Claude', status: 'idle', session_id: 'c-1' },
      { id: 'seat-b', provider: 'codex', label: 'Codex', status: 'closed', session_id: 'x-1' },
    ]
    await mountApp('/p/' + SITE + '/c/' + chat!.id)
    useAppStore().status = { configured: true, node: 'alice' }
    await useProjectsStore().refreshAll()
    await settle()
    expect($$('#seat_row input[type="checkbox"], #seat_row button[role="checkbox"]')).toHaveLength(2)
    const codex = $<HTMLElement>('#seat_seat-b')!
    codex.click()
    await settle()
    const inbox = useInboxStore()
    expect(inbox.seatAskFor(inbox.chat!)).toEqual(['seat-b'])
    inbox.composer = 'посмотри тесты'
    await inbox.submitMessage()
    await settle()
    expect(api.backend.sent.at(-1)).toMatchObject({ chat_id: chat!.id, body: 'посмотри тесты', ask_seats: ['seat-b'] })
    // The message says whom it asks (a chat of two says so only for agents).
    expect(document.body.textContent).toContain('посмотри тестыinbox.asks')
  })

  it('names a local agent as "<node> · <label>" and keeps agents apart', () => {
    const at = '2026-09-01T10:00:00Z'
    const codex: ChatMessage = { id: '1', seq: 1, from: 'alice', created_at: at, author_kind: 'agent', agent: { seat: 's2', label: 'Codex', provider: 'codex' } }
    const claude: ChatMessage = { id: '2', seq: 2, from: 'alice', created_at: at, author_kind: 'agent', agent: { seat: 's1', provider: 'claude' } }
    expect(authorLabel(codex, 'alice')).toBe('alice · Codex')
    expect(authorLabel(claude, 'alice')).toBe('alice · Claude')
    expect(authorLabel({ ...codex, agent: undefined }, 'alice')).toBe('inbox.author.own_agent')
    expect(continues(codex, claude)).toBe(false)
    expect(continues(codex, { ...codex, id: '3', seq: 3 })).toBe(true)
  })
})
