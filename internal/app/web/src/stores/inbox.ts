// The inbox: a list of chats, the open chat and the toasts for news. Chats,
// their messages and the live activity of every participant come from
// /ui/api/chats; SSE "chats"/"messages"/"sessions" events reload them
// (stores/app.ts).
import { defineStore } from 'pinia'
import { ref, shallowRef, watch } from 'vue'
import { api } from '@/lib/api'
import { authorLabel, others, preview } from '@/lib/chat'
import { currentRoute, navigate, type Query } from '@/lib/nav'
import { browser, t } from '@/lib/runtime'
import { NARROW_QUERY } from '@/layout/composables/layout'
import { useAppStore } from './app'
import type { ChatInfo, ChatMessage } from '@/types'

export const PAGE_SIZE = 200
// A toast only announces news: it leaves on its own after
// MESSAGE_TOAST_TIMEOUT, and the close button drops it right away. Failures
// keep their own place — the connection banner stays until the problem is gone.
export const MESSAGE_TOAST_TIMEOUT = 6000
const MAX_TOASTS = 3

export interface Toast {
  id: number
  notificationID: string
  chat: string
  message: string
  from: string
  body: string
}

export interface NewChatPerson { name: string; online: boolean }

// Storage keys; built so no key reads as a dictionary key.
const READS_KEY = 'agentlink.reads.v1:'
const NOTIFICATIONS_KEY = 'agentlink.notifications.v1:'

function storageGet(key: string): string | null {
  try { return localStorage.getItem(key) } catch { return null }
}
function storageSet(key: string, value: string) {
  try { localStorage.setItem(key, value) } catch { /* storage may be unavailable */ }
}

export function chatPath(id: string) { return 'chats/' + encodeURIComponent(id) }
function narrow() { return typeof matchMedia === 'function' && matchMedia(NARROW_QUERY).matches }

export const useInboxStore = defineStore('inbox', () => {
  const app = useAppStore()

  const selectedChat = ref('')
  const selectedMessage = ref('')
  const chat = shallowRef<ChatInfo | null>(null)
  const messages = shallowRef<ChatMessage[]>([]) // the open chat, ascending by seq
  const hasOlder = ref(false)
  // How the timeline keeps its scroll after the next render.
  const scrollIntent = ref<{ kind: 'reset' | 'prepended' | 'auto'; n: number }>({ kind: 'auto', n: 0 })
  const drafts = ref<Record<string, string>>({})
  const composer = ref('')
  const replyTo = shallowRef<ChatMessage | null>(null)
  const sendResult = ref('')
  const subtitleError = ref('')
  const sending = ref(false)
  const closing = ref(false)
  const infoOpen = ref(false)
  const focusComposer = ref(0)
  const askState = ref<Record<string, string[]>>({}) // chat id -> names asked to answer

  const newChatOpen = ref(false)
  const newChatPeople = ref<NewChatPerson[]>([])
  const newChatChosen = ref<string[]>([])
  const newChatArea = ref('')
  const newChatAreas = ref<string[]>([])
  const newChatResult = ref('')
  const newChatBusy = ref(false)
  const focusNewChat = ref(0)

  const reads = ref<Record<string, number>>({})
  const toasts = ref<Toast[]>([])

  let loadTicket = 0
  let pendingPeer = ''
  let readsNode = ''
  let notificationNode = ''
  let notificationIDs: string[] = []
  let toastSeq = 0
  const toastTimers = new Map<number, ReturnType<typeof setTimeout>>()

  function inboxVisible() { return currentRoute() === 'inbox' }
  function intend(kind: 'reset' | 'prepended' | 'auto') { scrollIntent.value = { kind, n: scrollIntent.value.n + 1 } }

  // --- read markers: the last seq seen per chat, kept per node ---

  function persistReads() {
    if (readsNode) storageSet(READS_KEY + readsNode, JSON.stringify(reads.value))
  }
  function loadReads(chats: ChatInfo[] | null) {
    const node = app.self
    if (!node || readsNode === node || !Array.isArray(chats)) return
    readsNode = node
    const saved = storageGet(READS_KEY + node)
    let value: unknown
    try { value = saved === null ? null : JSON.parse(saved) } catch { value = {} }
    if (!value || typeof value !== 'object') {
      // First visit: the history so far counts as read.
      const seeded: Record<string, number> = {}
      for (const c of chats) seeded[c.id] = c.last_seq || 0
      reads.value = seeded
      persistReads()
      return
    }
    reads.value = value as Record<string, number>
  }
  function markRead(info: ChatInfo) {
    if ((reads.value[info.id] || 0) >= (info.last_seq || 0)) return
    reads.value = { ...reads.value, [info.id]: info.last_seq || 0 }
    persistReads()
  }

  // --- whom a message asks ---

  // askFor is the chat's "who must answer" choice: in a chat of two the other
  // side by default, in a group nobody until the user picks.
  function askFor(info: ChatInfo): string[] {
    const saved = askState.value[info.id]
    if (saved) return saved
    const names = others(info, app.self)
    return names.length === 1 ? names : []
  }
  function keepAsk(info: ChatInfo) {
    // The default needs to know who is "me".
    if (!askState.value[info.id] && app.self) askState.value = { ...askState.value, [info.id]: askFor(info) }
  }
  function setAsk(info: ChatInfo, names: string[]) {
    askState.value = { ...askState.value, [info.id]: names }
  }

  // --- loading ---

  async function loadChat(id: string, reset: boolean) {
    const ticket = ++loadTicket
    const first = messages.value[0]?.seq || 0
    const query = !reset && first ? '?after=' + (first - 1) + '&limit=1000' : '?limit=' + PAGE_SIZE
    const [info, items] = await Promise.all([
      api<ChatInfo>('GET', chatPath(id)),
      api<ChatMessage[]>('GET', chatPath(id) + '/messages' + query),
    ])
    if (ticket !== loadTicket || selectedChat.value !== id) return
    if (reset) hasOlder.value = Array.isArray(items) && items.length === PAGE_SIZE
    messages.value = Array.isArray(items) ? [...items].sort((a, b) => a.seq - b.seq) : []
    chat.value = info
    keepAsk(info)
    markRead(info)
    intend(reset ? 'reset' : 'auto')
  }

  async function loadOlder() {
    const id = selectedChat.value
    const first = messages.value[0]?.seq || 0
    if (!id || !first) return
    try {
      const items = await api<ChatMessage[]>('GET', chatPath(id) + '/messages?before=' + first + '&limit=' + PAGE_SIZE)
      if (selectedChat.value !== id || !Array.isArray(items)) return
      hasOlder.value = items.length === PAGE_SIZE
      const known = new Set(messages.value.map((m) => m.id))
      messages.value = items.filter((m) => !known.has(m.id)).concat(messages.value).sort((a, b) => a.seq - b.seq)
      intend('prepended')
    } catch (error) { sendResult.value = (error as Error).message }
  }

  function saveDraft(id: string) {
    if (id) drafts.value = { ...drafts.value, [id]: composer.value }
  }

  function setReply(m: ChatMessage | null) {
    replyTo.value = m
    const info = chat.value
    if (m && info && m.from !== app.self && others(info, app.self).includes(m.from)) setAsk(info, [m.from])
    if (m) focusComposer.value++
  }

  async function selectChat(id: string, messageID = '') {
    const previous = selectedChat.value
    if (previous !== id) {
      saveDraft(previous)
      messages.value = []
      hasOlder.value = false
      chat.value = null
      composer.value = drafts.value[id] || ''
      setReply(null)
      sendResult.value = ''
      subtitleError.value = ''
      infoOpen.value = false
    }
    newChatOpen.value = false
    selectedChat.value = id || ''
    selectedMessage.value = messageID || ''
    if (!id) return
    try { await loadChat(id, previous !== id || !messages.value.length) }
    catch (error) { sendResult.value = (error as Error).message; subtitleError.value = (error as Error).message }
  }

  // --- the composer ---

  async function submitMessage() {
    const info = chat.value
    if (sending.value || !info) return
    const id = info.id
    sending.value = true
    sendResult.value = ''
    try {
      const body: Record<string, unknown> = { chat_id: id, body: composer.value, ask: [...askFor(info)].sort() }
      if (replyTo.value) body.reply_to = replyTo.value.id
      const sent = await api<ChatMessage>('POST', 'send', body)
      drafts.value = { ...drafts.value, [id]: '' }
      if (selectedChat.value === id) { composer.value = ''; setReply(null) }
      // A legacy chat continues elsewhere: in a real chat, or in the plain
      // message's own legacy chat; a closed chat in its conversation's next one.
      const next = info.legacy ? sent.chat_id || 'legacy-' + sent.id + '-' + info.peer : sent.chat_id || id
      if (next !== id) {
        void app.refreshSlice('chats')
        if (selectedChat.value === id) navigate('inbox', { chat: next })
        return
      }
      await loadChat(id, false)
    } catch (error) { sendResult.value = (error as Error).message }
    finally { sending.value = false }
  }

  // --- starting a chat ---

  function showNewChat(preselect: string[] = []) {
    saveDraft(selectedChat.value)
    newChatOpen.value = true
    const people: NewChatPerson[] = (app.status?.members || []).filter((m) => !m.self).map((m) => ({ name: m.name, online: !!m.online }))
    for (const name of preselect) if (!people.some((m) => m.name === name)) people.push({ name, online: false })
    newChatPeople.value = people
    newChatChosen.value = preselect.filter((name) => people.some((m) => m.name === name))
    const s = app.settings || {}
    newChatAreas.value = [...new Set([...(s.areas || []), ...Object.keys(s.projects || {})])]
    newChatArea.value = ''
    newChatResult.value = ''
    focusNewChat.value++
  }

  function hideNewChat() {
    newChatOpen.value = false
  }

  async function createChat() {
    const participants = newChatPeople.value.map((p) => p.name).filter((name) => newChatChosen.value.includes(name))
    newChatBusy.value = true
    try {
      const body: Record<string, unknown> = { participants }
      if (newChatArea.value.trim()) body.area = newChatArea.value.trim()
      const info = await api<ChatInfo>('POST', 'chats', body)
      newChatOpen.value = false
      void app.refreshSlice('chats')
      navigate('inbox', { chat: info.id })
      focusComposer.value++
    } catch (error) { newChatResult.value = (error as Error).message }
    finally { newChatBusy.value = false }
  }

  // chatWith finds the newest open chat of exactly this node and peer.
  function chatWith(peer: string) {
    const want = [app.self, peer].sort().join('\n')
    return (app.chats || []).find((c) => !c.legacy && !c.closed && [...(c.participants || [])].sort().join('\n') === want)
  }

  function resolvePendingPeer() {
    if (!pendingPeer || !app.self || !app.chats) return
    const peer = pendingPeer
    pendingPeer = ''
    const found = chatWith(peer)
    if (found) navigate('inbox', { chat: found.id })
    else showNewChat([peer])
  }

  function openInbox(query: Query = {}) {
    if (typeof query.chat === 'string' && query.chat) { void selectChat(query.chat, typeof query.message === 'string' ? query.message : ''); return }
    if (typeof query.peer === 'string' && query.peer) {
      pendingPeer = query.peer
      resolvePendingPeer()
      return
    }
    void selectChat(selectedChat.value, '')
  }

  // --- closing: the chat leaves the list at once and the view moves on ---

  async function closeChat() {
    const info = chat.value
    if (!info || closing.value) return
    closing.value = true
    try {
      await api('POST', chatPath(info.id) + '/close')
      const rest = (app.chats || []).filter((c) => c.id !== info.id)
      const next = narrow() ? null : rest.find((c) => !c.legacy) || rest[0]
      // Leave the chat before the list changes, so nothing reloads it.
      await selectChat('', '')
      app.chats = rest
      navigate('inbox', next ? { chat: next.id } : undefined)
      void app.refreshSlice('chats')
    } catch (error) {
      sendResult.value = (error as Error).message
      subtitleError.value = (error as Error).message
    } finally { closing.value = false }
  }

  function confirmClose() {
    const info = chat.value
    const key = !info?.legacy ? "inbox.close.confirm" : legacyPeerOldOf(info) ? "inbox.close.confirm_old" : "inbox.close.confirm_legacy"
    if (!browser.confirm(t(key))) return
    return closeChat()
  }

  function legacyPeerOldOf(info: ChatInfo) {
    return (info.members || []).some((m) => !m.self && m.connected && !m.compatible)
  }

  // --- notifications: a toast for a new incoming message in any chat ---

  function persistNotificationIDs(node: string, ids: string[]) {
    const bounded = [...new Set(ids)].slice(0, 500)
    storageSet(NOTIFICATIONS_KEY + node, JSON.stringify(bounded))
    notificationIDs = bounded
  }

  function dismissToast(id: number) {
    const timer = toastTimers.get(id)
    if (timer !== undefined) clearTimeout(timer)
    toastTimers.delete(id)
    toasts.value = toasts.value.filter((toast) => toast.id !== id)
  }

  function showMessageToast(item: Omit<Toast, 'id'>) {
    while (toasts.value.length >= MAX_TOASTS) dismissToast(toasts.value[0]!.id)
    const id = ++toastSeq
    toasts.value = [...toasts.value, { ...item, id }]
    toastTimers.set(id, setTimeout(() => dismissToast(id), MESSAGE_TOAST_TIMEOUT))
  }

  function openToast(toast: Toast) {
    dismissToast(toast.id)
    navigate('inbox', { chat: toast.chat, message: toast.message })
  }

  function processIncomingChats(chats: ChatInfo[] | null) {
    const node = app.self
    if (!node || !Array.isArray(chats)) return
    const items = chats.filter((c) => c.last_message && c.last_message.direction === 'in').map((c) => ({
      notificationID: c.last_message!.id, chat: c.id, message: c.last_message!.id,
      from: authorLabel(c.last_message!, node), body: preview(c.last_message!.body, 120),
    }))
    if (notificationNode !== node) {
      notificationNode = node
      const saved = storageGet(NOTIFICATIONS_KEY + node)
      if (saved === null) { persistNotificationIDs(node, items.map((item) => item.notificationID)); return }
      let parsed: unknown
      try { parsed = JSON.parse(saved) } catch { parsed = [] }
      notificationIDs = Array.isArray(parsed) ? parsed as string[] : []
    }
    const known = new Set(notificationIDs)
    const open = inboxVisible() ? selectedChat.value : ''
    const unseen = items.filter((item) => !known.has(item.notificationID) && item.chat !== open)
    persistNotificationIDs(node, items.map((item) => item.notificationID).concat(notificationIDs))
    for (const item of unseen.slice(0, MAX_TOASTS)) showMessageToast(item)
  }

  // --- wiring ---

  function onChats() {
    loadReads(app.chats)
    processIncomingChats(app.chats)
    resolvePendingPeer()
    const id = selectedChat.value
    if (id && inboxVisible()) loadChat(id, !messages.value.length).catch((error: Error) => { sendResult.value = error.message })
  }

  watch(() => app.chats, onChats)
  watch(() => app.status, () => {
    loadReads(app.chats)
    processIncomingChats(app.chats)
    resolvePendingPeer()
  })

  return {
    selectedChat, selectedMessage, chat, messages, hasOlder, scrollIntent, drafts, composer, replyTo, sendResult,
    subtitleError, sending, closing, infoOpen, focusComposer, askState, reads, toasts,
    newChatOpen, newChatPeople, newChatChosen, newChatArea, newChatAreas, newChatResult, newChatBusy, focusNewChat,
    askFor, setAsk, loadChat, loadOlder, saveDraft, setReply, selectChat, submitMessage, showNewChat, hideNewChat,
    createChat, openInbox, closeChat, confirmClose, processIncomingChats, dismissToast, openToast, loadReads,
  }
})
