import { describe, expect, it } from 'vitest'
import { useToast } from '@nuxt/ui/composables/useToast'
import { fakeApi, mountApp, settle } from '@/test/harness'
import { useAppStore } from './app'
import { MESSAGE_TOAST_TIMEOUT, useInboxStore } from './inbox'
import { useProjectsStore } from './projects'
import type { ChatInfo, ChatMessage, ProjectView } from '@/types'

const message = (id: string, from: string, direction: string, body: string): ChatMessage => ({ id, seq: 1, from, direction, body, created_at: '' })
const chat = (id: string, msg: ChatMessage): ChatInfo => ({ id, participants: ['local', msg.from], last_seq: 1, members: [], last_message: msg })

describe('open chat refresh', () => {
  // serveChat answers one chat of project P whose messages are items.
  function serveChat(items: ChatMessage[]) {
    return fakeApi((_method, path) => {
      if (path === 'projects') return []
      if (path === 'projects/P/chats/c1') return { id: 'c1', participants: ['local', 'bob'], last_seq: items.length, members: [] }
      const q = new URLSearchParams(path.split('?')[1] || '')
      const limit = Number(q.get('limit'))
      const before = Number(q.get('before') || 0)
      const older = items.filter((m) => !before || m.seq < before)
      return older.slice(Math.max(0, older.length - limit))
    })
  }
  const numbered = (n: number, from = 1) => Array.from({ length: n }, (_, i) => ({ ...message('m' + (from + i), 'bob', 'in', 'x'), seq: from + i }))

  it('keeps the older pages and the newest messages past 1000 shown', async () => {
    const items = numbered(1300)
    serveChat(items)
    await mountApp('/welcome')
    const inbox = useInboxStore()
    await inbox.selectChat('P', 'c1')
    for (let i = 0; i < 5; i++) await inbox.loadOlder()
    expect(inbox.messages).toHaveLength(1200)
    items.push(...numbered(3, 1301))
    await inbox.loadChat('c1', false)
    expect(inbox.messages).toHaveLength(1203)
    expect(inbox.messages[0]!.seq).toBe(101)
    expect(inbox.messages.at(-1)!.seq).toBe(1303)
    expect(inbox.readOf('P', 'c1')).toBe(1303)
  })

  it('starts over when more arrived than one answer holds', async () => {
    const items = numbered(10)
    serveChat(items)
    await mountApp('/welcome')
    const inbox = useInboxStore()
    await inbox.selectChat('P', 'c1')
    items.push(...numbered(500, 11))
    await inbox.loadChat('c1', false)
    expect(inbox.messages).toHaveLength(210)
    expect(inbox.messages.at(-1)!.seq).toBe(510)
    expect(inbox.hasOlder).toBe(true)
  })
})

describe('read cursors', () => {
  it('drop the cursors of a project gone from the list', async () => {
    localStorage.setItem('agentlink.reads.v2:local', JSON.stringify({ 'P:': 0, 'P:c1': 3, 'GONE:': 0, 'GONE:c9': 7 }))
    fakeApi((_method, path) => (path === 'projects' ? [] : {}))
    await mountApp('/welcome')
    const app = useAppStore()
    const projects = useProjectsStore()
    const inbox = useInboxStore()
    app.status = { node: 'local' }
    projects.list = [{ id: 'P', display: 'P', legacy: false } as ProjectView]
    projects.chats = { P: [chat('c1', message('m', 'bob', 'in', 'x'))] }
    await settle()
    expect(inbox.readOf('P', 'c1')).toBe(3)
    expect(inbox.readOf('GONE', 'c9')).toBe(0)
    expect(Object.keys(JSON.parse(localStorage.getItem('agentlink.reads.v2:local')!) as object).sort()).toEqual(['P:', 'P:c1'])
  })
})

describe('message toasts', () => {
  it('seed the history silently, then announce each new incoming message once', async () => {
    fakeApi((_method, path) => (path === 'projects' ? [] : {}))
    const { router } = await mountApp('/welcome')
    const app = useAppStore()
    const inbox = useInboxStore()
    const nuxt = useToast()
    nuxt.clear()
    app.status = { node: 'local' }
    await settle()
    const initial = [chat('c-old', message('old', 'bob', 'in', 'old'))]
    const long = '😀'.repeat(121)
    const next = [chat('c-new', message('new', 'карл & sons', 'in', long)), ...initial, chat('c-out', message('out', 'local', 'out', 'ignore'))]
    // The message toasts still open in Nuxt UI's toaster.
    const toasts = () => nuxt.toasts.value.filter((x) => x.open && String(x.id).startsWith('message:'))

    inbox.processIncomingChats({ P: initial })
    await settle()
    expect(toasts()).toHaveLength(0)
    inbox.processIncomingChats({ P: next })
    await settle()
    expect(toasts()).toHaveLength(1)
    const toast = toasts()[0]!
    expect(toast.title).toBe('карл & sons')
    const preview = String(toast.description)
    expect(preview.endsWith('…')).toBe(true)
    expect(Array.from(preview.replace(/…$/, ''))).toHaveLength(120)
    expect(toast.duration).toBe(MESSAGE_TOAST_TIMEOUT)
    expect(toast.close).toEqual({ 'aria-label': 'inbox.toast.close' })
    expect(toast.actions?.[0]?.label).toBe('inbox.toast.open')
    toast.onClick!(toast)
    await settle()
    expect(router.currentRoute.value.name).toBe('chat')
    expect(router.currentRoute.value.params).toEqual({ project: 'P', chat: 'c-new' })
    expect(router.currentRoute.value.query).toEqual({ message: 'new' })
    expect(toasts()).toHaveLength(0)
    const saved = JSON.parse(localStorage.getItem('agentlink.notifications.v1:local')!) as string[]
    expect(saved).toContain('old')
    expect(saved).toContain('new')
    expect(saved).not.toContain('out')

    // The chat on screen gets no toast.
    inbox.project = 'P'
    inbox.selectedChat = 'c-old'
    inbox.processIncomingChats({ P: [chat('c-old', message('old2', 'bob', 'in', 'seen here')), ...next] })
    await settle()
    expect(toasts()).toHaveLength(0)
    // One refresh announces at most three; «Открыть» opens its message.
    inbox.processIncomingChats({ P: [...next, ...Array.from({ length: 4 }, (_, i) => chat('c' + i, message('bulk' + i, 'peer' + i, 'in', 'bulk ' + i)))] })
    await settle()
    expect(toasts().map((x) => x.title)).toEqual(['peer0', 'peer1', 'peer2'])
    const open = toasts()[1]!.actions![0]!.onClick as (event: MouseEvent) => void
    open(new MouseEvent('click'))
    await settle()
    expect(router.currentRoute.value.params).toEqual({ project: 'P', chat: 'c1' })
    expect(toasts().map((x) => x.title)).toEqual(['peer0', 'peer2'])
  })
})
