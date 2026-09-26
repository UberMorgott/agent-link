// The open chat of a project and the toasts for news. A chat, its messages and
// the live activity of every participant come from /ui/api/projects/{pid}/…;
// the chat lists live in stores/projects.ts, and SSE "project:<pid>" events
// reload them (stores/app.ts), which reloads the open chat too.
import { defineStore } from 'pinia'
import { ref, shallowRef, watch } from 'vue'
import { api, chatPath, projectPath } from '@/lib/api'
import { authorLabel, others, preview } from '@/lib/chat'
import { currentRoute, openChat, openProject } from '@/lib/nav'
import { browser, fmt, t } from '@/lib/runtime'
import { NARROW_QUERY } from '@/layout/composables/layout'
import { useAppStore } from './app'
import { useAttachmentsStore } from './attachments'
import { useProjectsStore } from './projects'
import type { ChatInfo, ChatMessage, SentMessage } from '@/types'

export const PAGE_SIZE = 200
// A toast only announces news: it leaves on its own after
// MESSAGE_TOAST_TIMEOUT, and the close button drops it right away. Failures
// keep their own place — the connection banner stays until the problem is gone.
export const MESSAGE_TOAST_TIMEOUT = 6000
const MAX_TOASTS = 3

export interface Toast {
  id: number
  notificationID: string
  project: string
  chat: string
  message: string
  from: string
  body: string
}


// Storage keys; built so no key reads as a dictionary key.
const READS_KEY = 'agentlink.reads.v2:'
const NOTIFICATIONS_KEY = 'agentlink.notifications.v1:'

function storageGet(key: string): string | null {
  try { return localStorage.getItem(key) } catch { return null }
}
function storageSet(key: string, value: string) {
  try { localStorage.setItem(key, value) } catch { /* storage may be unavailable */ }
}

// chatKey names one chat across projects: drafts and read cursors use it.
export function chatKey(project: string, chat: string) { return project + ':' + chat }
function narrow() { return typeof matchMedia === 'function' && matchMedia(NARROW_QUERY).matches }

export const useInboxStore = defineStore('inbox', () => {
  const app = useAppStore()
  const projects = useProjectsStore()

  const project = ref('') // the project of the open chat
  const selectedChat = ref('')
  const selectedMessage = ref('')
  const chat = shallowRef<ChatInfo | null>(null)
  const messages = shallowRef<ChatMessage[]>([]) // the open chat, ascending by seq
  const hasOlder = ref(false)
  // How the timeline keeps its scroll after the next render.
  const scrollIntent = ref<{ kind: 'reset' | 'prepended' | 'auto'; n: number }>({ kind: 'auto', n: 0 })
  const drafts = ref<Record<string, string>>({}) // by chatKey
  const composer = ref('')
  const replyTo = shallowRef<ChatMessage | null>(null)
  const sendResult = ref('')
  const subtitleError = ref('')
  const sending = ref(false)
  const closing = ref(false)
  const focusComposer = ref(0)
  const askState = ref<Record<string, string[]>>({}) // chatKey -> names asked to answer
  const seatAskState = ref<Record<string, string[]>>({}) // chatKey -> this member's local agents (seat ids) asked

  // starting: the project's one chat is being started (startChat).
  const starting = ref(false)
  const startResult = ref('')

  // Read cursors (D11): the last seq seen per chatKey; "<pid>:" marks a
  // project whose history has been seeded.
  const reads = ref<Record<string, number>>({})
  const toasts = ref<Toast[]>([])

  // Every answer carries the generation it was asked in: switching the
  // project or the chat drops what was still on its way.
  let generation = 0
  let readsNode = ''
  let notificationNode = ''
  let notificationIDs: string[] = []
  let toastSeq = 0
  const toastTimers = new Map<number, ReturnType<typeof setTimeout>>()

  function chatVisible() { return currentRoute() === 'chat' }
  function intend(kind: 'reset' | 'prepended' | 'auto') { scrollIntent.value = { kind, n: scrollIntent.value.n + 1 } }
  function openKey() { return chatKey(project.value, selectedChat.value) }

  // --- read markers, kept per node ---

  function persistReads() {
    if (readsNode) storageSet(READS_KEY + readsNode, JSON.stringify(reads.value))
  }
  function loadReads() {
    const node = app.self
    if (!node || readsNode === node) return
    readsNode = node
    const saved = storageGet(READS_KEY + node)
    let value: unknown
    try { value = saved === null ? null : JSON.parse(saved) } catch { value = null }
    reads.value = value && typeof value === 'object' ? value as Record<string, number> : {}
  }
  // seedReads: the first time a project's chats are seen, its history so far
  // counts as read.
  function seedReads() {
    loadReads()
    if (!readsNode) return
    let next: Record<string, number> | null = null
    for (const [pid, list] of Object.entries(projects.chats)) {
      if (Object.hasOwn(reads.value, pid + ':')) continue
      next ??= { ...reads.value }
      next[pid + ':'] = 0
      for (const c of list) next[chatKey(pid, c.id)] = c.last_seq || 0
    }
    if (next) { reads.value = next; persistReads() }
  }
  function markRead(pid: string, info: ChatInfo) {
    const key = chatKey(pid, info.id)
    if ((reads.value[key] || 0) >= (info.last_seq || 0)) return
    reads.value = { ...reads.value, [key]: info.last_seq || 0 }
    persistReads()
  }
  // readOf is the read cursor of a chat.
  function readOf(pid: string, id: string): number { return reads.value[chatKey(pid, id)] || 0 }

  // --- whom a message asks ---

  // askFor is the chat's "who must answer" choice: in a chat of two the other
  // side by default, in a group nobody until the user picks.
  function askFor(info: ChatInfo): string[] {
    const saved = askState.value[chatKey(project.value, info.id)]
    if (saved) return saved
    const names = others(info, app.self)
    return names.length === 1 ? names : []
  }
  function keepAsk(info: ChatInfo) {
    // The default needs to know who is "me".
    const key = chatKey(project.value, info.id)
    if (!askState.value[key] && app.self) askState.value = { ...askState.value, [key]: askFor(info) }
  }
  function setAsk(info: ChatInfo, names: string[]) {
    askState.value = { ...askState.value, [chatKey(project.value, info.id)]: names }
  }
  // seatAskFor: the local agents (seats) the next message asks; none by default.
  function seatAskFor(info: ChatInfo): string[] { return seatAskState.value[chatKey(project.value, info.id)] || [] }
  function setSeatAsk(info: ChatInfo, ids: string[]) {
    seatAskState.value = { ...seatAskState.value, [chatKey(project.value, info.id)]: ids }
  }

  // --- loading ---

  async function loadChat(id: string, reset: boolean) {
    const ticket = ++generation
    const pid = project.value
    const first = messages.value[0]?.seq || 0
    const query = !reset && first ? '?after=' + (first - 1) + '&limit=1000' : '?limit=' + PAGE_SIZE
    const [info, items] = await Promise.all([
      api<ChatInfo>('GET', chatPath(pid, id)),
      api<ChatMessage[]>('GET', chatPath(pid, id, 'messages' + query)),
    ])
    if (ticket !== generation || project.value !== pid || selectedChat.value !== id) return
    if (reset) hasOlder.value = Array.isArray(items) && items.length === PAGE_SIZE
    messages.value = Array.isArray(items) ? [...items].sort((a, b) => a.seq - b.seq) : []
    chat.value = info
    keepAsk(info)
    markRead(pid, info)
    intend(reset ? 'reset' : 'auto')
  }

  async function loadOlder() {
    const id = selectedChat.value
    const pid = project.value
    const first = messages.value[0]?.seq || 0
    if (!id || !first) return
    const ticket = generation
    try {
      const items = await api<ChatMessage[]>('GET', chatPath(pid, id, 'messages?before=' + first + '&limit=' + PAGE_SIZE))
      if (ticket !== generation || selectedChat.value !== id || !Array.isArray(items)) return
      hasOlder.value = items.length === PAGE_SIZE
      const known = new Set(messages.value.map((m) => m.id))
      messages.value = items.filter((m) => !known.has(m.id)).concat(messages.value).sort((a, b) => a.seq - b.seq)
      intend('prepended')
    } catch (error) { sendResult.value = (error as Error).message }
  }

  function saveDraft(key: string) {
    if (key && !key.endsWith(':')) drafts.value = { ...drafts.value, [key]: composer.value }
  }

  function setReply(m: ChatMessage | null) {
    replyTo.value = m
    const info = chat.value
    if (m && info && m.from !== app.self && others(info, app.self).includes(m.from)) setAsk(info, [m.from])
    if (m) focusComposer.value++
  }

  // selectChat opens chat id of project pid ("" = none).
  async function selectChat(pid: string, id: string, messageID = '') {
    const previous = openKey()
    const next = chatKey(pid, id)
    if (previous !== next) {
      generation++
      saveDraft(previous)
      messages.value = []
      hasOlder.value = false
      chat.value = null
      composer.value = drafts.value[next] || ''
      setReply(null)
      sendResult.value = ''
      subtitleError.value = ''
    }
    if (project.value !== pid) startResult.value = ''
    project.value = pid
    selectedChat.value = id || ''
    selectedMessage.value = messageID || ''
    if (!id) return
    try { await loadChat(id, previous !== next || !messages.value.length) }
    catch (error) {
      if (project.value !== pid || selectedChat.value !== id) return
      sendResult.value = (error as Error).message
      subtitleError.value = (error as Error).message
    }
  }

  // --- the composer ---

  // canSend: there is something to send (text or a ready file) and nothing
  // on its way; the send button is disabled otherwise.
  function canSend(): boolean {
    const files = useAttachmentsStore()
    return !sending.value && !files.uploading && (composer.value.trim() !== '' || files.ready.length > 0)
  }

  async function submitMessage() {
    const info = chat.value
    // An empty message is never sent, and says nothing: the button is off.
    if (sending.value || !info || !canSend()) return
    const pid = project.value
    const id = info.id
    const key = chatKey(pid, id)
    sending.value = true
    sendResult.value = ''
    try {
      const body: Record<string, unknown> = { chat_id: id, body: composer.value, ask: [...askFor(info)].sort() }
      if (replyTo.value) body.reply_to = replyTo.value.id
      const seats = seatAskFor(info)
      if (seats.length) body.ask_seats = [...seats].sort()
      const files = useAttachmentsStore()
      if (files.uploading) return // the send button waits for the uploads
      if (files.ready.length) body.attachments = files.ready
      const sent = await api<SentMessage>('POST', projectPath(pid, 'send'), body)
      drafts.value = { ...drafts.value, [key]: '' }
      const here = openKey() === key
      if (here) { composer.value = ''; setReply(null); files.clear() }
      // A legacy chat continues elsewhere: in a real chat, or in the plain
      // message's own legacy chat.
      const next = info.legacy ? sent.chat_id || 'legacy-' + sent.id + '-' + info.peer : sent.chat_id || id
      if (next !== id) {
        void projects.refreshChats(pid)
        if (here) openChat(pid, next)
        return
      }
      if (here) await loadChat(id, false)
    } catch (error) { sendResult.value = (error as Error).message }
    finally { sending.value = false }
  }

  // --- a project's one chat ---

  // activeChat is a project's one active chat, if it has one.
  function activeChat(pid: string): ChatInfo | null {
    return (projects.chats[pid] || []).find((c) => !c.legacy && !c.closed && !c.archived) || null
  }

  // openActive opens a project's one chat; without one (a new project) the
  // project's page offers to start it.
  function openActive(pid: string) {
    const found = activeChat(pid)
    if (found) openChat(pid, found.id)
    else openProject(pid)
  }

  // startChat starts the project's one chat with every member (the node
  // returns the chat the project already has, if any) and opens it.
  async function startChat(pid: string) {
    if (starting.value) return
    starting.value = true
    startResult.value = ''
    try {
      const view = projects.byID(pid)
      const everyone = (view?.members || []).filter((m) => !m.self).map((m) => m.name)
      const info = await projects.createChat(pid, everyone)
      openChat(pid, info.id)
      focusComposer.value++
    } catch (error) { startResult.value = (error as Error).message }
    finally { starting.value = false }
  }

  // openPeer opens the newest open chat of exactly this node and peer (the
  // network from before projects), or starts it.
  async function openPeer(pid: string, peer: string) {
    const want = [app.self, peer].sort().join('\n')
    const found = (projects.chats[pid] || []).find((c) => !c.legacy && !c.closed && [...(c.participants || [])].sort().join('\n') === want)
    if (found) { openChat(pid, found.id); return }
    try {
      const info = await projects.createChat(pid, [peer])
      openChat(pid, info.id)
    } catch (error) { startResult.value = (error as Error).message }
  }
  // --- members of a project chat: its owner invites and removes them ---

  const membersBusy = ref(false)

  // setMembers changes who is in chat id of project pid; it throws on failure.
  async function setMembers(pid: string, id: string, add: string[], remove: string[] = []) {
    if (membersBusy.value) return
    membersBusy.value = true
    try {
      const info = await api<ChatInfo>('POST', chatPath(pid, id, 'members'), { add, remove })
      if (openKey() === chatKey(pid, id)) chat.value = info
      projects.chats = { ...projects.chats, [pid]: (projects.chats[pid] || []).map((c) => (c.id === id ? info : c)) }
      void projects.refreshChats(pid)
    } finally { membersBusy.value = false }
  }

  function confirmRemove(pid: string, id: string, name: string) {
    if (!browser.confirm(fmt("inbox.members.remove_confirm", { name }))) return
    return setMembers(pid, id, [], [name])
  }

  // --- closing: the chat leaves the list at once and the view moves on ---

  async function closeChat() {
    const info = chat.value
    const pid = project.value
    if (!info || closing.value) return
    closing.value = true
    try {
      await api('POST', chatPath(pid, info.id, 'close'))
      const rest = (projects.chats[pid] || []).filter((c) => c.id !== info.id)
      const next = narrow() ? null : rest.find((c) => !c.legacy) || rest[0]
      // Leave the chat before the list changes, so nothing reloads it.
      await selectChat(pid, '', '')
      projects.chats = { ...projects.chats, [pid]: rest }
      if (next) openChat(pid, next.id)
      else openProject(pid)
      void projects.refreshChats(pid)
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

  // --- «Очистить чат»: for every member the messages go to the project's
  // history (a dated snapshot) and the chat goes on empty with the same
  // members; the view moves to it ---

  // clearChat clears project pid's chat; the view moves to the emptied chat
  // when the old one was on screen. It throws on failure.
  async function clearChat(pid: string) {
    const info = activeChat(pid)
    if (!info || closing.value) return
    closing.value = true
    try {
      const fresh = await api<ChatInfo>('POST', chatPath(pid, info.id, 'archive'))
      const here = openKey() === chatKey(pid, info.id)
      projects.chats = { ...projects.chats, [pid]: [fresh, ...(projects.chats[pid] || []).filter((c) => c.id !== info.id && c.id !== fresh.id)] }
      if (here) {
        openChat(pid, fresh.id)
        focusComposer.value++
      }
      void projects.refreshChats(pid)
    } finally { closing.value = false }
  }

  function confirmClear(pid: string) {
    if (!browser.confirm(t("inbox.clear.confirm"))) return
    return clearChat(pid)
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
    openChat(toast.project, toast.chat, toast.message)
  }

  // processIncomingChats announces the chats' new incoming messages, by project.
  function processIncomingChats(byProject: Record<string, ChatInfo[]>) {
    const node = app.self
    // Nothing is announced before the first chats have arrived: they seed.
    if (!node || !Object.keys(byProject).length) return
    const items = Object.entries(byProject).flatMap(([pid, list]) => (list || [])
      .filter((c) => c.last_message && c.last_message.direction === 'in')
      .map((c) => ({
        notificationID: c.last_message!.id, project: pid, chat: c.id, message: c.last_message!.id,
        from: authorLabel(c.last_message!, node), body: preview(c.last_message!.body, 120),
      })))
    if (notificationNode !== node) {
      notificationNode = node
      const saved = storageGet(NOTIFICATIONS_KEY + node)
      if (saved === null) { persistNotificationIDs(node, items.map((item) => item.notificationID)); return }
      let parsed: unknown
      try { parsed = JSON.parse(saved) } catch { parsed = [] }
      notificationIDs = Array.isArray(parsed) ? parsed as string[] : []
    }
    const known = new Set(notificationIDs)
    const open = chatVisible() ? openKey() : ''
    const unseen = items.filter((item) => !known.has(item.notificationID) && chatKey(item.project, item.chat) !== open)
    persistNotificationIDs(node, items.map((item) => item.notificationID).concat(notificationIDs))
    for (const item of unseen.slice(0, MAX_TOASTS)) showMessageToast(item)
  }

  // --- wiring ---

  let seenLists: Record<string, ChatInfo[]> = {}
  function onChats() {
    const lists = projects.chats
    seedReads()
    if (projects.loaded) processIncomingChats(lists)
    // The open chat reloads when its own project's list changed.
    const pid = project.value
    const id = selectedChat.value
    const changed = lists[pid] !== seenLists[pid]
    seenLists = lists
    if (id && changed && chatVisible()) loadChat(id, !messages.value.length).catch((error: Error) => { sendResult.value = error.message })
  }

  watch(() => projects.chats, onChats)
  watch(() => app.status, () => {
    seedReads()
    if (projects.loaded) processIncomingChats(projects.chats)
  })

  return {
    project, selectedChat, selectedMessage, chat, messages, hasOlder, scrollIntent, drafts, composer, replyTo, sendResult,
    subtitleError, sending, closing, focusComposer, askState, reads, toasts, starting, startResult,
    askFor, setAsk, seatAskFor, setSeatAsk, loadChat, loadOlder, saveDraft, setReply, selectChat, canSend, submitMessage,
    startChat, openPeer, activeChat, openActive, closeChat, confirmClose, clearChat, confirmClear, membersBusy, setMembers, confirmRemove, processIncomingChats, dismissToast, openToast, readOf, openKey,
  }
})
