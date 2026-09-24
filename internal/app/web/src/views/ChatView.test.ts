import { nextTick } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { browser, runtime } from '@/lib/runtime'
import { fakeApi, mountApp, settle } from '@/test/harness'
import { useAppStore } from '@/stores/app'
import { useInboxStore } from '@/stores/inbox'
import { useProjectsStore } from '@/stores/projects'
import type { ChatInfo, ChatMessage, Presence } from '@/types'

const base = 1700000000000
const iso = (ms: number) => new Date(ms).toISOString()
const group = 'c1 &'
const P = 'PROJ'
const prefix = 'projects/' + P + '/'

interface Fixture { info: ChatInfo; items: ChatMessage[] }

function fixtures(): Record<string, Fixture> {
  const opening: ChatMessage[] = [{ id: 'open', seq: 1, kind: 'chat_open', from: 'local', body: '', created_at: iso(base), direction: 'out' }]
  const history = opening.concat(Array.from<unknown, ChatMessage>({ length: 205 }, (_, i) => ({ id: 'm' + i, seq: i + 2, from: i % 2 ? 'bob' : 'local', direction: i % 2 ? 'in' : 'out', body: 'line ' + i, created_at: iso(base + i * 1000) })))
  // Delivery ticks and author kinds on a few messages.
  history[201]!.delivery = [{ peer: 'bob', status: 'sent', state: 'read', at: iso(base) }, { peer: 'карл & sons', status: 'queued', state: 'queued' }]
  history[199]!.delivery = [{ peer: 'bob', status: 'sent', state: 'answered' }, { peer: 'карл & sons', status: 'sent', state: 'delivered' }]
  history[197]!.delivery = [{ peer: 'bob', status: 'sent', state: 'read' }, { peer: 'карл & sons', status: 'sent', state: 'answered' }]
  history[195]!.author_kind = 'agent'
  history[202]!.author_kind = 'agent'
  return {
    [group]: {
      info: {
        id: group, participants: ['bob', 'local', 'карл & sons'], title: 'line 0', closed: false, archived: false, last_seq: 206, last_at: iso(base + 204000),
        members: [
          {
            name: 'bob', connected: true, compatible: true, queued: 0,
            jobs: [{ reply_to: 'm204', job_status: 'running', activity_info: { type: 'edit', text: 'app.go', phase: 'running', started_at: iso(base + 241000) }, updated_at: iso(base + 241000) }],
            held: [{ reply_to: 'm202', job_status: 'held', hold_reason: 'no_handler', activity: 'никто не отвечает — ждёт человека' }],
          },
          { name: 'local', self: true, connected: true, compatible: true, queued: 0 },
          { name: 'карл & sons', connected: false, compatible: true, queued: 2, jobs: [{ reply_to: 'm204', job_status: 'queued', updated_at: iso(base + 204000), stale: true }] },
        ],
      },
      items: history,
    },
    c2: { info: { id: 'c2', participants: ['alice', 'local'], closed: true, closed_by: 'alice', closed_at: iso(base), archived: true, last_seq: 3, members: [] }, items: [] },
    c3: { info: { id: 'c3', participants: ['bob', 'local'], closed: false, last_seq: 0, members: [] }, items: [] },
    c4: {
      info: {
        id: 'c4', participants: ['bob', 'local'], closed: false, archived: false, last_seq: 1,
        members: [{ name: 'bob', connected: true, compatible: true, queued: 0 }, { name: 'local', self: true, connected: true, compatible: true, queued: 0 }],
      },
      items: [{ id: 'u1', seq: 1, from: 'bob', author_kind: 'agent', direction: 'in', unread: true, body: 'hi', created_at: iso(base) }],
    },
  }
}

// bobPresence is a chat of two where this node's last message reached bob.
function bobPresence(presence: Presence | undefined, connected = true, state = 'delivered'): Fixture {
  return {
    info: {
      id: 'c5', participants: ['bob', 'local'], closed: false, archived: false, last_seq: 1,
      members: [{ name: 'bob', connected, compatible: true, queued: 0, presence }, { name: 'local', self: true, connected: true, compatible: true, queued: 0 }],
    },
    items: [{ id: 'o1', seq: 1, from: 'local', direction: 'out', body: 'q', created_at: iso(base), delivery: [{ peer: 'bob', status: 'sent', state }] }],
  }
}

let chats: Record<string, Fixture>
let releaseSend: (() => void) | null
const sent: unknown[] = []

function serve() {
  return fakeApi((method, path, body) => {
    if (path === 'projects') return [{ id: P, legacy: false, name: 'Сайт', alias: '', display: 'Сайт', dir: 'W:/work', state: 'ready', problem: '', online: 2, total: 2, can_rename: true, has_invite: true, busy: false, members: [{ name: 'local', self: true, online: true }, { name: 'bob', online: true }, { name: 'карл & sons', online: true }, { name: 'alice', online: false }] }]
    if (!path.startsWith(prefix)) throw new Error('unexpected call ' + method + ' ' + path)
    path = path.slice(prefix.length)
    if (method === 'POST' && path.endsWith('/close')) {
      const chat = chats[decodeURIComponent(path.split('/')[1]!)]!
      chat.info = { ...chat.info, closed: true, archived: true }
      return chat.info
    }
    if (method === 'POST' && path === 'send') {
      sent.push(body)
      return new Promise((resolve) => { releaseSend = () => resolve({ id: 's1' }) })
    }
    // The main list holds the chats not archived; the archive the rest.
    if (path === 'chats') return Object.values(chats).map((c) => c.info).filter((i) => !i.archived)
    if (path === 'chats?archive=1') return Object.values(chats).map((c) => c.info).filter((i) => i.archived)
    const m = path.match(/^chats\/([^/?]+)(\/messages)?(?:\?(.*))?$/)
    const chat = m && chats[decodeURIComponent(m[1]!)]
    if (!chat) throw new Error('unexpected call ' + method + ' ' + path)
    if (!m[2]) return chat.info
    const q = new URLSearchParams(m[3] || '')
    const limit = Number(q.get('limit'))
    if (q.get('after')) return chat.items.filter((x) => x.seq > Number(q.get('after'))).slice(0, limit)
    const before = Number(q.get('before') || 0)
    const older = chat.items.filter((x) => !before || x.seq < before)
    return older.slice(Math.max(0, older.length - limit))
  })
}

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))
const bubble = (id: string) => $('[data-message-id="' + id + '"]')!
const tick = (id: string) => bubble(id).querySelector<HTMLElement>('.msg-ticks')
const text = (el: Element | null) => el?.textContent || ''
// The timeline's rows: the older-page button, then one per message or event.
const timeline = () => $$('#messages .msg-older, #messages [data-message-id]')
// Ticked people among the checkboxes of a group (Nuxt UI checkboxes are buttons).
const ticked = (sel: string) => $$(sel + ' [role="checkbox"]').filter((box) => box.getAttribute('aria-checked') === 'true').map((box) => box.id)

beforeEach(() => {
  chats = fixtures()
  releaseSend = null
  sent.length = 0
  runtime.strings = { 'inbox.activity.type.edit': 'правит', 'inbox.activity.ago': '{t} назад','inbox.author.agent': 'агент {name}', 'inbox.member.queued': 'в очереди {n}' }
})

afterEach(() => { releaseSend?.() })

async function openInbox() {
  const api = serve()
  const mounted = await mountApp('/p/' + P + '/c/c3')
  const app = useAppStore()
  app.status = { node: 'local' }
  app.settings = { areas: ['dev'] }
  const projects = useProjectsStore()
  await projects.refreshAll()
  await settle()
  return { ...mounted, api, app, projects, inbox: useInboxStore() }
}

describe('the open chat', () => {
  it('renders ticks, authors, members, whom to ask and live activity', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    const { api, inbox } = await openInbox()
    await inbox.selectChat(P, group, 'm204')
    await settle()
    expect(api.calls).toContain('GET projects/PROJ/chats/c1%20%26')
    expect(api.calls).toContain('GET projects/PROJ/chats/c1%20%26/messages?limit=200')
    const items = timeline()
    expect(items).toHaveLength(201)
    expect(items[0]!.className).toContain('msg-older')
    // The message a link names is revealed.
    expect(bubble('m204').scrollIntoView).toHaveBeenCalled()
    expect(document.activeElement).toBe(bubble('m204'))

    const hold = tick('m202')!
    expect(hold.className).toContain('held')
    expect(hold.title).toBe('bob: никто не отвечает — ждёт человека')
    expect(hold.getAttribute('aria-label')).toBe(hold.title)
    expect(tick('m204')).toBeNull()
    expect(bubble('m202').querySelector('.msg-reply')).toBeNull()
    // The lowest state of a group; answered reads as read; per-recipient tooltip.
    expect(tick('m200')!.className).toContain('queued')
    expect(tick('m200')!.title).toContain('bob: inbox.tick.read')
    expect(tick('m200')!.title).toContain('карл & sons: inbox.tick.queued')
    expect(tick('m198')!.className).toContain('delivered')
    expect(tick('m198')!.title).toContain('bob: inbox.tick.answered')
    expect(tick('m196')!.className).toContain('read')
    expect(tick('m196')!.innerHTML).toContain('<svg')
    // Authors: a person or an agent, on either side.
    expect(text(bubble('m194').querySelector('.msg-author'))).toBe('inbox.author.own_agent')
    expect(bubble('m194').className).toContain('agent')
    expect(text(bubble('m201').querySelector('.msg-author'))).toBe('агент bob')
    expect(text(bubble('m199').querySelector('.msg-author'))).toBe('bob')
    expect(text(bubble('m198').querySelector('.msg-author'))).toBe('inbox.you')

    expect(text($('#conversation_title'))).toContain('bob, карл & sons')
    // Members and sessions live in the chat's info popover.
    inbox.infoOpen = true
    await settle()
    const chips = $$('#chat_members > li')
    expect(chips).toHaveLength(3)
    expect($('#chat_close')).not.toBeNull()
    expect($('#send')).not.toBeNull()
    expect(chips[2]!.className).toContain('away')
    expect(text(chips[2]!)).toContain('в очереди 2')
    // A group chat asks nobody by default and says so.
    expect($('#ask_row')).not.toBeNull()
    expect(ticked('#ask_choices')).toEqual([])
    expect($('#ask_hint')).not.toBeNull()

    // Live activity: one row per job, local timers, no app calls.
    const rows = $$('#chat_activity > li')
    expect(rows).toHaveLength(2)
    expect(text(rows[0]!)).toContain('правит app.go')
    expect(rows[1]!.className).toContain('stale')
    expect(text(rows[1]!)).toContain('inbox.activity.stale')
    const before = api.calls.length
    vi.spyOn(Date, 'now').mockReturnValue(base + 246000)
    vi.advanceTimersByTime(1000)
    await nextTick()
    // A running line's time is how long ago its agent was last heard of.
    expect(text(rows[0]!.querySelector('.act-time'))).toBe('· 0:05 назад')
    vi.spyOn(Date, 'now').mockReturnValue(base + 306000)
    vi.advanceTimersByTime(1000)
    await nextTick()
    expect(text(rows[0]!.querySelector('.act-time'))).toBe('· 1:05 назад')
    expect(api.calls.length).toBe(before)
  })

  it('keeps bubbles, focus, draft, caret and scroll across refreshes, then loads older and sends once', async () => {
    const { api, inbox } = await openInbox()
    await inbox.selectChat(P, group, '')
    await settle()
    const list = $('#messages')!
    const kept = timeline()[11]!
    const replyControl = kept.querySelector<HTMLButtonElement>('.msg-reply')!
    replyControl.focus()
    await inbox.loadChat(group, false)
    await settle()
    expect(timeline()[11]).toBe(kept)
    expect(kept.querySelector('.msg-reply')).toBe(replyControl)
    expect(document.activeElement).toBe(replyControl)

    const body = $<HTMLTextAreaElement>('#body')!
    body.value = 'first\nsecond'
    body.dispatchEvent(new Event('input'))
    body.focus()
    body.setSelectionRange(3, 3)
    let top = 45
    Object.defineProperty(list, 'scrollTop', { configurable: true, get: () => top, set: (v: number) => { top = v } })
    Object.defineProperty(list, 'clientHeight', { configurable: true, value: 100 })
    Object.defineProperty(list, 'scrollHeight', { configurable: true, value: 20500 })
    chats[group]!.items[20] = { ...chats[group]!.items[20]!, body: 'edited' }
    await inbox.loadChat(group, false)
    await settle()
    expect(timeline()[11]).toBe(kept)
    expect(body.value).toBe('first\nsecond')
    expect(document.activeElement).toBe(body)
    expect(body.selectionStart).toBe(3)
    expect(top).toBe(45)
    top = 20400
    chats[group]!.items.push({ id: 'm205', seq: 207, from: 'bob', direction: 'in', body: 'new', created_at: iso(base + 300000) })
    await inbox.loadChat(group, false)
    await settle()
    expect(top).toBe(20500)

    // The older page, then a reply with its author asked, guarded against a double submit.
    $<HTMLButtonElement>('.msg-older button')!.click()
    await settle()
    expect(timeline()).toHaveLength(207)
    expect($('.msg-older')).toBeNull()
    bubble('m203').querySelector<HTMLButtonElement>('.msg-reply')!.click()
    await settle()
    expect($('#replying')).not.toBeNull()
    expect(inbox.replyTo?.id).toBe('m203')
    expect(ticked('#ask_choices')).toEqual(['ask_bob'])
    const form = $<HTMLFormElement>('#send')!
    form.dispatchEvent(new Event('submit', { cancelable: true }))
    form.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    expect(api.calls.filter((c) => c === 'POST projects/PROJ/send')).toHaveLength(1)
    expect($<HTMLButtonElement>('#send_button')!.disabled).toBe(true)
    expect(sent[0]).toEqual({ chat_id: group, body: 'first\nsecond', ask: ['bob'], reply_to: 'm203' })
    releaseSend!()
    await settle()
    expect($<HTMLButtonElement>('#send_button')!.disabled).toBe(false)
    expect($<HTMLTextAreaElement>('#body')!.value).toBe('')
    expect($('#replying')).toBeNull()
  })

  it('sends on Enter like a messenger, keeps Shift+Enter, IME and blank input from sending', async () => {
    const { api, inbox } = await openInbox()
    await inbox.selectChat(P, group, '')
    await settle()
    expect(text($('#composer_hint'))).toBe('inbox.body.hint')
    const body = $<HTMLTextAreaElement>('#body')!
    const press = (init: KeyboardEventInit) => {
      const e = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true, ...init })
      body.dispatchEvent(e)
      return e
    }
    const sends = () => api.calls.filter((c) => c === 'POST projects/PROJ/send').length
    body.value = '   '
    body.dispatchEvent(new Event('input'))
    await settle()
    press({})
    await settle()
    expect(sends()).toBe(0)

    body.value = 'hello'
    body.dispatchEvent(new Event('input'))
    await settle()
    // Shift+Enter is left to the textarea (a new line); a composition in progress is not sent.
    expect(press({ shiftKey: true }).defaultPrevented).toBe(false)
    expect(press({ isComposing: true }).defaultPrevented).toBe(false)
    await settle()
    expect(sends()).toBe(0)

    expect(press({}).defaultPrevented).toBe(true)
    await settle()
    expect(sends()).toBe(1)
    expect(sent[0]).toMatchObject({ chat_id: group, body: 'hello' })
  })
})

describe('the chat list and the ways into a chat', () => {
  it('marks unread and live chats, opens chats by row, peer link and close', async () => {
    const { router, app, projects, inbox } = await openInbox()
    await inbox.selectChat(P, group, '')
    await settle()
    const second = { ...chats.c2!.info, last_seq: 3, last_message: { id: 'a1', seq: 3, from: 'alice', direction: 'in', body: 'old from alice', created_at: iso(base) } }
    projects.chats = { [P]: [chats[group]!.info, second] }
    await settle()
    projects.chats = { [P]: [chats[group]!.info, { ...second, last_seq: 4, last_message: { id: 'a2', seq: 4, from: 'alice', direction: 'in', body: 'latest from alice', created_at: iso(base) } }] }
    await settle()
    const rows = $$('#project_tree [data-project="PROJ"] .chat-row')
    expect(text(rows[0]!)).toContain('правит app.go')
    expect(rows[0]!.className).toContain('live')
    expect(text(rows[0]!)).not.toContain('inbox.unread')
    expect(rows[1]!.className).toContain('fresh')
    expect(text(rows[1]!)).toContain('inbox.unread')
    const alice = rows[1]!
    expect(alice.tagName).toBe('BUTTON')
    expect(alice.tabIndex).toBeGreaterThanOrEqual(0)
    alice.click()
    await settle()
    expect(router.currentRoute.value.params.chat).toBe('c2')

    // A closed chat is readable but has no composer.
    expect($('#send')).toBeNull()
    expect($('#chat_note')).not.toBeNull()
    expect($('#chat_close')).toBeNull()
    // A peer link opens the open chat with that peer.
    projects.chats = { [P]: [chats.c3!.info, ...projects.chats[P]!] }
    await router.push('/p/PROJ?peer=bob')
    await settle()
    expect(router.currentRoute.value.params.chat).toBe('c3')

    // A chat of two: nobody to choose; an unread message the local agent has
    // not taken says why it waits.
    app.sessions = [{ session_id: 's', provider: 'claude', folder: 'W:/work', area: '', wake: 'next-event' }]
    await inbox.selectChat(P, 'c4', '')
    inbox.infoOpen = true
    await settle()
    expect($('#ask_row')).toBeNull()
    expect(text($('#chat_activity'))).toContain('inbox.activity.waiting_session')
    expect(text($('#chat_sessions'))).toContain('claude')
    expect(text($('#chat_sessions'))).toContain('inbox.session.next_event')
    app.sessions = []
    await settle()
    expect(text($('#chat_activity'))).toContain('inbox.activity.no_session')
    app.sessions = [{ session_id: 's', provider: 'claude', folder: 'W:/work', area: '', wake: 'rewake' }]
    await settle()
    expect($('#chat_activity')).toBeNull()
    app.sessions = [{ session_id: 's', provider: 'codex', folder: 'W:/work', area: '', wake: 'queue' }]
    await settle()
    expect($('#chat_activity')).toBeNull()
    expect(text($('#chat_sessions'))).toContain('inbox.session.queue')

    // The peer's session, under an own message it has but has not read: one muted line.
    const cases: [Presence, string][] = [
      [{ area: '', session: 'rewake' }, 'inbox.presence.rewake'],
      [{ area: '', session: 'next-event', auto_answer: true }, 'inbox.presence.next_event'],
      [{ area: '', session: 'queue' }, 'inbox.presence.queue'],
      [{ area: '' }, 'inbox.presence.none'],
      [{ area: '', auto_answer: true }, 'inbox.presence.worker'],
    ]
    for (const [presence, key] of cases) {
      chats.c5 = bobPresence(presence)
      await inbox.selectChat(P, 'c5', '')
      await settle()
      const lines = $$('#chat_activity > li')
      expect(lines, key).toHaveLength(1)
      expect(lines[0]!.className).toContain('presence')
      expect(text(lines[0]!)).toContain('bob:')
      expect(text(lines[0]!)).toContain(key)
    }
    for (const fixture of [bobPresence({ area: '', session: 'rewake' }, false, 'queued'), bobPresence({ area: '', session: 'rewake' }, true, 'read'), bobPresence(undefined)]) {
      chats.c5 = fixture
      await inbox.selectChat(P, 'c5', '')
      await settle()
      expect($$('#chat_activity > li.presence')).toHaveLength(0)
    }

    // Closing: the chat leaves the list at once and the view moves to the next chat.
    await router.push('/p/PROJ/c/c4')
    await settle()
    projects.chats = { [P]: [chats.c4!.info, chats.c3!.info] }
    await settle()
    const confirm = vi.spyOn(browser, 'confirm').mockReturnValue(true)
    $<HTMLButtonElement>('#chat_close')!.click()
    await settle()
    expect(confirm).toHaveBeenCalledWith('inbox.close.confirm')
    expect(projects.chats[P]!.some((c) => c.id === 'c4')).toBe(false)
    expect($('[data-chat="c4"]')).toBeNull()
    expect(router.currentRoute.value.params.chat).toBe('c3')
  })
})

describe('the shell of the inbox', () => {
  it('labels the new chat people and the "who answers" groups', async () => {
    const { inbox } = await openInbox()
    await inbox.selectChat(P, group, '')
    await settle()
    const ask = $('#ask_choices')!.closest('[role="group"]')!
    expect(document.getElementById(ask.getAttribute('aria-labelledby')!)).not.toBeNull()
    inbox.showNewChat(P, [])
    await settle()
    const people = $('#new_chat_members')!.closest('[role="group"]')!
    expect(document.getElementById(people.getAttribute('aria-labelledby')!)).not.toBeNull()
  })
})
