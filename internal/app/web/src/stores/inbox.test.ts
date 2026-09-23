import { describe, expect, it, vi } from 'vitest'
import { fakeApi, mountApp, settle } from '@/test/harness'
import { useAppStore } from './app'
import { MESSAGE_TOAST_TIMEOUT, useInboxStore } from './inbox'
import type { ChatInfo, ChatMessage } from '@/types'

const message = (id: string, from: string, direction: string, body: string): ChatMessage => ({ id, seq: 1, from, direction, body, created_at: '' })
const chat = (id: string, msg: ChatMessage): ChatInfo => ({ id, participants: ['local', msg.from], last_seq: 1, members: [], last_message: msg })

describe('message toasts', () => {
  it('seed the history silently, then announce each new incoming message once', async () => {
    fakeApi(() => ({}))
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const { router } = await mountApp('/dashboard')
    const app = useAppStore()
    const inbox = useInboxStore()
    app.status = { node: 'local' }
    await settle()
    const initial = [chat('c-old', message('old', 'bob', 'in', 'old'))]
    const long = '😀'.repeat(121)
    const next = [chat('c-new', message('new', 'карл & sons', 'in', long)), ...initial, chat('c-out', message('out', 'local', 'out', 'ignore'))]
    const toasts = () => Array.from(document.querySelectorAll<HTMLElement>('#message-toast-region .message-toast'))

    inbox.processIncomingChats(initial)
    await settle()
    expect(toasts()).toHaveLength(0)
    inbox.processIncomingChats(next)
    await settle()
    expect(toasts()).toHaveLength(1)
    const toast = toasts()[0]!
    expect(toast.textContent).toContain('карл & sons')
    const preview = toast.querySelector('.message-toast-preview')!.textContent!.trim()
    expect(preview.endsWith('…')).toBe(true)
    expect(Array.from(preview.replace(/…$/, ''))).toHaveLength(120)
    const close = toast.querySelector<HTMLButtonElement>('.message-toast-close')!
    expect(close.tagName).toBe('BUTTON')
    expect(close.getAttribute('aria-label')).toBe('inbox.toast.close')
    expect(vi.getTimerCount()).toBe(1)
    toast.querySelector<HTMLButtonElement>('.message-toast-main')!.click()
    await settle()
    expect(router.currentRoute.value.name).toBe('inbox')
    expect(router.currentRoute.value.query).toEqual({ chat: 'c-new', message: 'new' })
    expect(toasts()).toHaveLength(0)
    expect(vi.getTimerCount()).toBe(0)
    const saved = JSON.parse(localStorage.getItem('agentlink.notifications.v1:local')!) as string[]
    expect(saved).toContain('old')
    expect(saved).toContain('new')
    expect(saved).not.toContain('out')

    // The chat on screen gets no toast.
    inbox.selectedChat = 'c-old'
    inbox.processIncomingChats([chat('c-old', message('old2', 'bob', 'in', 'seen here')), ...next])
    await settle()
    expect(toasts()).toHaveLength(0)
    // At most three at a time; each leaves on its own or by its close button.
    inbox.processIncomingChats([...next, ...Array.from({ length: 4 }, (_, i) => chat('c' + i, message('bulk' + i, 'peer' + i, 'in', 'bulk ' + i)))])
    await settle()
    expect(toasts()).toHaveLength(3)
    toasts()[0]!.querySelector<HTMLButtonElement>('.message-toast-close')!.click()
    await settle()
    expect(toasts()).toHaveLength(2)
    expect(vi.getTimerCount()).toBe(2)
    vi.advanceTimersByTime(MESSAGE_TOAST_TIMEOUT)
    await settle()
    expect(toasts()).toHaveLength(0)
  })
})
