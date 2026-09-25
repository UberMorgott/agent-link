// Projects: the list the sidebar shows, each project's chats, and what the
// project dialogs do (create, join, rename, folder, invite, leave). Every
// request goes to /ui/api/projects… (docs/plans/projects-v1.md §7); SSE
// "projects" and "project:<pid>" events reload them (stores/app.ts).
import { defineStore } from 'pinia'
import { computed, ref, shallowRef, watch } from 'vue'
import { api, projectPath } from '@/lib/api'
import type { ChatInfo, InviteView, JoinResult, ProjectView } from '@/types'

export const LEGACY = 'legacy'
// The project opened last, so /ui/inbox comes back to it.
const LAST_KEY = 'agentlink' + '.project.last'

function storageGet(key: string): string | null {
  try { return localStorage.getItem(key) } catch { return null }
}
function storageSet(key: string, value: string) {
  try { localStorage.setItem(key, value) } catch { /* storage may be unavailable */ }
}

// sortProjects keeps the server's order rule: by display name, legacy last.
export function sortProjects(list: ProjectView[]): ProjectView[] {
  return [...list].sort((a, b) => Number(a.legacy) - Number(b.legacy) || a.display.localeCompare(b.display, 'ru'))
}

// The join dialog: invite → connecting (until the shared name arrives) →
// folder (bind a folder and an alias) → done.
export type JoinStep = 'invite' | 'connecting' | 'folder'

// The dialogs of a project's menu, and those that make or join a project.
export type ProjectDialog = '' | 'members' | 'invite' | 'name' | 'folder' | 'leave' | 'create' | 'join'

export const useProjectsStore = defineStore('projects', () => {
  const list = shallowRef<ProjectView[] | null>(null)
  const chats = shallowRef<Record<string, ChatInfo[]>>({})
  const archives = shallowRef<Record<string, ChatInfo[]>>({})
  const archiveOpen = ref<Record<string, boolean>>({})
  // The project on screen (a project page or one of its chats); "" elsewhere.
  const current = ref('')

  // The invite of one project, only while its dialog is open: never kept.
  const invite = ref('')
  const inviteFor = ref('')

  const dialog = ref<ProjectDialog>('')
  const dialogProject = ref('')

  const joinStep = ref<JoinStep>('invite')
  const joinProject = ref('')
  const joinCreated = ref(false)

  const chatTickets = new Map<string, number>()
  // left: projects this page left; the app's late project:<pid> events for
  // them would only answer 404.
  const left = new Set<string>()
  let listTicket = 0
  let latestList: Promise<void> | null = null

  const currentProject = computed(() => byID(current.value))
  const hasLegacy = computed(() => (list.value || []).some((p) => p.legacy))
  // loaded: the list and every project's chats have arrived.
  const loaded = computed(() => !!list.value && list.value.every((p) => Object.hasOwn(chats.value, p.id)))

  function byID(pid: string): ProjectView | null {
    return (list.value || []).find((p) => p.id === pid) || null
  }

  // upsert puts a project answer into the list, in place or as a new row.
  function upsert(view: ProjectView) {
    left.delete(view.id)
    const rest = (list.value || []).filter((p) => p.id !== view.id)
    list.value = sortProjects([...rest, view])
  }

  function drop(pid: string) {
    list.value = (list.value || []).filter((p) => p.id !== pid)
    const keptChats = { ...chats.value }
    const keptArchives = { ...archives.value }
    delete keptChats[pid]
    delete keptArchives[pid]
    chats.value = keptChats
    archives.value = keptArchives
    if (inviteFor.value === pid) hideInvite()
  }

  // --- reading ---

  function loadList(): Promise<void> {
    latestList = readList()
    return latestList
  }

  // listSettled waits for the newest list request, whichever caller made it.
  async function listSettled() {
    let seen: Promise<void> | null = null
    while (latestList && latestList !== seen) {
      seen = latestList
      await seen.catch(() => {})
    }
  }

  async function readList() {
    const ticket = ++listTicket
    const items = await api<ProjectView[]>('GET', 'projects')
    if (ticket !== listTicket) return
    list.value = sortProjects(Array.isArray(items) ? items : [])
    // A project gone from the list takes its chats with it.
    const known = new Set(list.value.map((p) => p.id))
    for (const pid of Object.keys(chats.value)) if (!known.has(pid)) drop(pid)
  }

  // refreshList reads the list, and the chats of a project new in it.
  async function refreshList() {
    await loadList()
    await Promise.all((list.value || []).filter((p) => !Object.hasOwn(chats.value, p.id)).map((p) => refreshChats(p.id)))
  }

  async function refreshProject(pid: string) {
    try {
      upsert(await api<ProjectView>('GET', projectPath(pid)))
    } catch (error) {
      if ((error as { status?: number }).status === 404) { drop(pid); return }
      throw error
    }
  }

  // refreshChats reads one project's chats, and its archive while it is open.
  async function refreshChats(pid: string) {
    const ticket = (chatTickets.get(pid) || 0) + 1
    chatTickets.set(pid, ticket)
    const archive = archiveOpen.value[pid] ? api<ChatInfo[]>('GET', projectPath(pid, 'chats?archive=1')) : null
    const [main, archived] = await Promise.all([api<ChatInfo[]>('GET', projectPath(pid, 'chats')), archive])
    if (chatTickets.get(pid) !== ticket || !byID(pid)) return
    chats.value = { ...chats.value, [pid]: Array.isArray(main) ? main : [] }
    if (archived) archives.value = { ...archives.value, [pid]: Array.isArray(archived) ? archived : [] }
  }

  async function refreshAll() {
    await loadList()
    await Promise.all((list.value || []).map((p) => refreshChats(p.id)))
  }

  // refreshScoped answers a "project:<pid>" event: the project and its chats.
  async function refreshScoped(pid: string) {
    if (left.has(pid)) return
    await refreshProject(pid)
    if (byID(pid)) await refreshChats(pid)
  }

  function toggleArchive(pid: string) {
    archiveOpen.value = { ...archiveOpen.value, [pid]: !archiveOpen.value[pid] }
    if (archiveOpen.value[pid]) return refreshChats(pid)
  }

  // --- the project on screen and the one to come back to ---

  function open(pid: string) {
    current.value = pid
    if (pid) storageSet(LAST_KEY, pid)
  }

  // landing is the project /ui/inbox opens: the last one used while it
  // exists, else the first; "" when there is none.
  function landing(): string {
    const items = list.value || []
    const last = storageGet(LAST_KEY) || ''
    return items.some((p) => p.id === last) ? last : items[0]?.id || ''
  }

  // --- changing projects ---

  async function create(body: { name: string; dir?: string; alias?: string }) {
    const view = await api<ProjectView>('POST', 'projects', body)
    upsert(view)
    chats.value = { ...chats.value, [view.id]: [] }
    return view
  }

  async function rename(pid: string, name: string) {
    const view = await api<ProjectView>('POST', projectPath(pid, 'name'), { name })
    upsert(view)
    return view
  }

  // bind changes this member's own alias and folder; an absent field is kept.
  async function bind(pid: string, body: { alias?: string; dir?: string }) {
    const view = await api<ProjectView>('POST', projectPath(pid, 'binding'), body)
    upsert(view)
    return view
  }

  async function addMember(pid: string, addr: string) {
    const view = await api<ProjectView>('POST', projectPath(pid, 'members/add'), { addr })
    upsert(view)
    return view
  }

  // removeMember removes a member from the whole project, for every member.
  async function removeMember(pid: string, name: string) {
    const view = await api<ProjectView>('POST', projectPath(pid, 'members/remove'), { name })
    upsert(view)
    return view
  }

  // leave returns the project to open next ("" when none is left).
  async function leave(pid: string): Promise<string> {
    await api('POST', projectPath(pid, 'leave'))
    left.add(pid)
    drop(pid)
    if (current.value === pid) current.value = ''
    return landing()
  }

  async function createChat(pid: string, participants: string[]) {
    const info = await api<ChatInfo>('POST', projectPath(pid, 'chats'), { participants })
    chats.value = { ...chats.value, [pid]: [info, ...(chats.value[pid] || []).filter((c) => c.id !== info.id)] }
    return info
  }

  // --- the invite: read once per open dialog, dropped when it closes ---

  async function revealInvite(pid: string): Promise<string> {
    if (inviteFor.value === pid && invite.value) return invite.value
    const view = await api<InviteView>('POST', projectPath(pid, 'invite'))
    inviteFor.value = pid
    invite.value = view.invite || ''
    return invite.value
  }

  function hideInvite() {
    invite.value = ''
    inviteFor.value = ''
  }

  // --- joining ---

  function joinReset() {
    joinStep.value = 'invite'
    joinProject.value = ''
    joinCreated.value = false
  }

  // join sends the invite. A project that was here already opens as it is
  // (created: false); a new one waits for its shared name, then for a folder.
  // dir is the legacy network's working folder, sent after a 400 work_dir.
  async function join(inviteText: string, addr: string, dir = ''): Promise<JoinResult> {
    const body: Record<string, string> = { invite: inviteText.trim() }
    if (addr.trim()) body.addr = addr.trim()
    if (dir.trim()) body.dir = dir.trim()
    const result = await api<JoinResult>('POST', 'projects/join', body)
    upsert(result.project)
    joinProject.value = result.project.id
    joinCreated.value = !!result.created
    if (result.created) {
      chats.value = { ...chats.value, [result.project.id]: [] }
      joinStep.value = joinNamed() ? 'folder' : 'connecting'
    }
    return result
  }

  function joinNamed(): boolean {
    const view = byID(joinProject.value)
    return !!view && (view.legacy || !!view.name)
  }

  // joinProgress moves a waiting join on once the shared name has arrived.
  function joinProgress() {
    if (joinStep.value === 'connecting' && joinNamed()) joinStep.value = 'folder'
  }

  // joinCancel leaves a project only this join created; it never leaves one
  // that was here before.
  async function joinCancel() {
    const pid = joinProject.value
    const created = joinCreated.value
    joinReset()
    if (pid && created) await leave(pid)
  }

  // --- the project dialogs (ProjectDialogs.vue): one open at a time ---

  function openDialog(kind: ProjectDialog, pid: string) {
    hideInvite()
    dialogProject.value = pid
    dialog.value = kind
  }

  function closeDialog() {
    // A shown invite never outlives its dialog.
    if (dialog.value === 'invite') hideInvite()
    dialog.value = ''
  }

  watch(list, joinProgress)

  return {
    list, chats, archives, archiveOpen, current, currentProject, hasLegacy, loaded, invite, inviteFor,
    joinStep, joinProject, joinCreated,
    byID, upsert, listSettled, refreshList, refreshProject, refreshChats, refreshAll, refreshScoped, toggleArchive,
    open, landing, create, rename, bind, addMember, removeMember, leave, createChat, revealInvite, hideInvite,
    joinReset, join, joinProgress, joinCancel, dialog, dialogProject, openDialog, closeDialog,
  }
})
