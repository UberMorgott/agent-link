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

describe('local agents (seats)', () => {
  it('adds Claude and Codex in the agents dialog, stops, starts and removes one', async () => {
    const LOCAL = 'LOCAL_SEATS'
    const api = fakeBackend()
    const site = api.backend.projects.find((p) => p.id === SITE)!
    api.backend.projects.push({ ...site, id: LOCAL, scope: 'local', has_invite: false, members: site.members.filter((m) => m.self) })
    await mountApp('/p/' + LOCAL)
    useAppStore().status = { configured: true, node: 'alice' }
    await useProjectsStore().refreshAll()
    await settle()
    const { calls, backend } = api
    const projects = useProjectsStore()
    projects.openDialog('agents', LOCAL)
    await settle()
    expect(calls).toContain('GET projects/' + LOCAL + '/seats')
    expect(document.body.textContent).toContain('project.agents.empty')
    expect(document.body.textContent).toContain('project.agents.hint')
    $<HTMLButtonElement>('#seat_add_claude')!.click()
    await settle()
    $<HTMLButtonElement>('#seat_add_codex')!.click()
    await settle()
    const seats = backend.seats[LOCAL]!
    expect(seats.map((s) => s.label)).toEqual(['Claude', 'Codex'])
    expect($$('#project_seats [data-seat]')).toHaveLength(2)
    const codex = seats[1]!.id
    $<HTMLButtonElement>('#seat_stop_' + codex)!.click()
    await settle()
    expect(backend.seats[LOCAL]![1]!.status).toBe('stopped')
    expect($('#seat_start_' + codex)).not.toBeNull()
    $<HTMLButtonElement>('#seat_start_' + codex)!.click()
    await settle()
    expect(backend.seats[LOCAL]![1]!.status).toBe('closed')
    const confirm = vi.spyOn(browser, 'confirm').mockReturnValue(false)
    $<HTMLButtonElement>('#seat_remove_' + codex)!.click()
    await settle()
    expect(calls).not.toContain('POST projects/' + LOCAL + '/seats/' + codex + '/remove')
    confirm.mockReturnValue(true)
    $<HTMLButtonElement>('#seat_remove_' + codex)!.click()
    await settle()
    expect(backend.seats[LOCAL]!.map((s) => s.label)).toEqual(['Claude'])
    expect($$('#project_seats [data-seat]')).toHaveLength(1)
  })

  it('explains a network project has no add buttons and keeps stop/remove for an old seat', async () => {
    const api = fakeBackend()
    api.backend.seats[SITE] = [
      { id: 'old-a', provider: 'claude', label: 'Claude', status: 'idle', session_id: 'c-1' },
      { id: 'old-b', provider: 'codex', label: 'Codex', status: 'stopped', session_id: 'x-1' },
    ]
    await mountApp('/p/' + SITE)
    useAppStore().status = { configured: true, node: 'alice' }
    await useProjectsStore().refreshAll()
    await settle()
    useProjectsStore().openDialog('agents', SITE)
    await settle()
    expect(document.body.textContent).toContain('project.agents.network_hint')
    expect($('#seat_add_claude')).toBeNull()
    expect($('#seat_add_codex')).toBeNull()
    expect($('#seat_start_old-b')).toBeNull()
    expect($('#seat_stop_old-b')).toBeNull()
    expect($('#seat_stop_old-a')).not.toBeNull()
    expect($('#seat_remove_old-b')).not.toBeNull()
    // The API itself names why a network project takes no new seat.
    await expect(useProjectsStore().seatAction(SITE, 'add', 'claude')).rejects.toMatchObject({ code: 'seats_local_only' })
  })

  it('keeps local seats out of the shared project composer', async () => {
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
    expect($('#seat_row')).toBeNull()
    const inbox = useInboxStore()
    inbox.composer = 'посмотри тесты'
    await inbox.submitMessage()
    await settle()
    expect(api.backend.sent.at(-1)).toEqual(expect.objectContaining({ chat_id: chat!.id, body: 'посмотри тесты', ask: [] }))
    expect(api.backend.sent.at(-1)).not.toHaveProperty('ask_seats')
  })

  it('asks a local seat only inside a local Claude/Codex chat', async () => {
    const [chat] = fixture<ChatInfo[]>('chats')
    const api = fakeBackend()
    const local = 'LOCAL_CHAT'
    const localChat = 'local-chat-1'
    const site = api.backend.projects.find((p) => p.id === SITE)!
    api.backend.projects.push({ ...site, id: local, scope: 'local', name: 'Local Claude ↔ Codex', display: 'Local Claude ↔ Codex', members: site.members.filter((m) => m.self) })
    api.backend.chats[local] = [{ ...chat!, id: localChat, project: local, participants: ['alice'], members: chat!.members?.filter((m) => m.self) }]
    api.backend.messages[localChat] = []
    api.backend.seats[local] = [
      { id: 'seat-a', provider: 'claude', label: 'Claude', status: 'idle' },
      { id: 'seat-b', provider: 'codex', label: 'Codex', status: 'closed' },
    ]
    await mountApp('/p/' + local + '/c/' + localChat)
    useAppStore().status = { configured: true, node: 'alice' }
    await useProjectsStore().refreshAll()
    await settle()
    expect($('#ask_row')).toBeNull()
    expect($$('#seat_row [role="checkbox"]')).toHaveLength(2)
    $<HTMLButtonElement>('#seat_seat-b')!.click()
    await settle()
    const inbox = useInboxStore()
    expect(inbox.seatAskFor(inbox.chat!)).toEqual(['seat-b'])
    inbox.composer = 'посмотри тесты'
    await inbox.submitMessage()
    await settle()
    expect(api.backend.sent.at(-1)).toMatchObject({ chat_id: localChat, body: 'посмотри тесты', ask: [], ask_seats: ['seat-b'] })
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
