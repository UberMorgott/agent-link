import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import { ApiError, CORE_SLICES, TOKEN_HEADER, api, reloadOnNewVersion, type CoreSlice, type Slice } from '@/lib/api'
import { runtime, t, fmt } from '@/lib/runtime'
import type { AppSettings, ParticipantView, Session, Status, UpdateStatus } from '@/types'
import { useProjectsStore } from './projects'

export interface ChangeEvent {
  revision?: number
  topics?: string[]
}

// slicesForTopics names the slices one change event makes stale.
export function slicesForTopics(topics: string[]): CoreSlice[] {
  if (topics.includes('all')) return [...CORE_SLICES]
  const slices = new Set<CoreSlice>()
  for (const topic of topics) {
    if ((CORE_SLICES as readonly string[]).includes(topic)) slices.add(topic as CoreSlice)
    if (topic === 'peer' || topic === 'members') slices.add('status')
    // The sessions list is shown filtered by project.
    if (topic.startsWith('project:')) slices.add('sessions')
  }
  return [...slices]
}

// participantsStale says whether one change event makes the participants list
// stale: members, their connections and the message counts.
export function participantsStale(topics: string[]): boolean {
  return topics.some((topic) => ['all', 'participants', 'peer', 'members', 'messages', 'worker'].includes(topic))
}

// projectsForTopics names what of the projects one change event makes stale:
// every project, the list, or single projects (with their chats).
export function projectsForTopics(topics: string[]): { all: boolean; list: boolean; scoped: string[] } {
  const all = topics.includes('all')
  const list = all || topics.some((topic) => topic === 'projects' || topic === 'members' || topic === 'peer')
  const scoped = [...new Set(topics.filter((topic) => topic.startsWith('project:')).map((topic) => topic.slice('project:'.length)))]
  return { all, list, scoped: all ? [] : scoped.filter(Boolean) }
}

export function parseSSERecord(record: string): ChangeEvent | null {
  const lines = record.split(/\r?\n/)
  if (!lines.some((line) => line === 'event: change')) return null
  const data = lines.filter((line) => line.startsWith('data:')).map((line) => line.slice(5).trimStart()).join('\n')
  if (!data) return null
  try { return JSON.parse(data) as ChangeEvent } catch { return null }
}

// statusLine is the connection indicator of the top bar and the chat list.
export function statusLine(s: Status | null): { text: string; cls: 'on' | 'off' } {
  if (!s) return { text: '', cls: 'off' }
  let text: string
  let cls: 'on' | 'off' = 'off'
  if (!s.configured) text = t("link.unconfigured")
  else if (s.error) text = s.error
  else if (s.connected) {
    text = fmt("link.on_many", { online: s.online || 0, total: s.total || 0 })
    cls = 'on'
  } else if (s.problem) text = t(s.problem)
  // Online and total count the other members of every network, projects included.
  else if (s.total) text = fmt("link.on_many", { online: s.online || 0, total: s.total })
  else text = t("link.nobody")
  if (s.configured && !s.zerotier) text += ' · ' + t("link.no_zerotier")
  if (s.warning) { text += ' · ' + t(s.warning); cls = 'off' }
  return { text, cls }
}

export const useAppStore = defineStore('app', () => {
  const status = shallowRef<Status | null>(null)
  const participants = shallowRef<ParticipantView[] | null>(null)
  const update = shallowRef<UpdateStatus | null>(null)
  const settings = shallowRef<AppSettings | null>(null)
  const sessions = shallowRef<Session[] | null>(null)

  // The connection banner: a failure stays until every failing request recovers;
  // a refused token asks for a reload and never clears.
  const reloadRequired = ref(false)
  const coreFailures = new Map<string, Error>()
  const banner = ref('')

  const self = computed(() => status.value?.node || '')
  const link = computed(() => statusLine(status.value))

  function setSlice(name: Slice, value: unknown) {
    const target = { status, participants, update, settings, sessions }[name]
    ;(target as { value: unknown }).value = value
  }

  function showConnectionProblem(name: string, error: Error & { status?: number }) {
    coreFailures.set(name, error)
    if (error.status === 403) {
      reloadRequired.value = true
      banner.value = error.message
      return
    }
    if (!reloadRequired.value) banner.value = error.message || t("link.app_not_running")
  }

  // clearConnectionProblem drops the failure of name, and of every other
  // request stale (true) marks as answered too.
  function clearConnectionProblem(name: string, stale?: (key: string) => boolean) {
    coreFailures.delete(name)
    if (stale) for (const key of [...coreFailures.keys()]) if (stale(key)) coreFailures.delete(key)
    if (!reloadRequired.value && !coreFailures.size) banner.value = ''
  }

  async function refreshSlice(name: Slice): Promise<void> {
    try {
      setSlice(name, await api('GET', name))
      clearConnectionProblem(name)
    } catch (error) { showConnectionProblem(name, error as ApiError) }
  }

  // guarded runs one projects refresh under the connection banner.
  async function guarded(name: string, run: () => Promise<void>, stale?: (key: string) => boolean) {
    try {
      await run()
      clearConnectionProblem(name, stale)
    } catch (error) { showConnectionProblem(name, error as ApiError) }
  }

  const PROJECT_KEY = 'project:'
  function applyChange(event: ChangeEvent) {
    const topics = Array.isArray(event.topics) ? event.topics : []
    const projects = useProjectsStore()
    const scope = projectsForTopics(topics)
    const jobs: Promise<void>[] = slicesForTopics(topics).map(refreshSlice)
    if (participantsWatchers && participantsStale(topics)) jobs.push(refreshSlice('participants'))
    // Reading every project answers each project's own failed refresh; the
    // list answers those of projects no longer in it.
    const gone = (key: string) => key.startsWith(PROJECT_KEY) && !projects.byID(key.slice(PROJECT_KEY.length))
    if (scope.all) jobs.push(guarded('projects', projects.refreshAll, (key) => key.startsWith(PROJECT_KEY)))
    else if (scope.list) jobs.push(guarded('projects', projects.refreshList, gone))
    for (const pid of scope.scoped) jobs.push(guarded(PROJECT_KEY + pid, () => projects.refreshScoped(pid)))
    return Promise.all(jobs)
  }

  // The participants page reads its list while it is open, and change events
  // keep it fresh only then; watchParticipants returns the call that stops it.
  let participantsWatchers = 0
  function watchParticipants(): () => void {
    participantsWatchers++
    void refreshSlice('participants')
    let stopped = false
    return () => {
      if (stopped) return
      stopped = true
      participantsWatchers--
    }
  }

  let reconnectDelay = 500
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null

  function scheduleReconnect() {
    if (reloadRequired.value || reconnectTimer !== null) return
    const delay = reconnectDelay
    reconnectDelay = Math.min(reconnectDelay * 2, 10000)
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null
      void connectEvents()
    }, delay)
  }

  // connectEvents reads the app's server-sent change events; every event
  // refreshes only the slices its topics name. Nothing polls on a timer.
  async function connectEvents(): Promise<void> {
    try {
      const response = await fetch('/ui/api/events', { headers: { [TOKEN_HEADER]: runtime.token } })
      if (reloadOnNewVersion(response)) return
      if (response.status === 403) {
        showConnectionProblem('events', new ApiError(t("error.forbidden"), 403))
        return
      }
      if (!response.ok || !response.body) throw new Error(t("error.no_app"))
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      for (;;) {
        const { value, done } = await reader.read()
        if (done) throw new Error(t("error.no_app"))
        buffer += decoder.decode(value, { stream: true })
        let split: number
        while ((split = buffer.search(/\r?\n\r?\n/)) >= 0) {
          const record = buffer.slice(0, split)
          const separator = buffer.slice(split).match(/^\r?\n\r?\n/)![0]
          buffer = buffer.slice(split + separator.length)
          const event = parseSSERecord(record)
          if (event) {
            reconnectDelay = 500
            clearConnectionProblem('events')
            await applyChange(event)
          }
        }
      }
    } catch (error) {
      showConnectionProblem('events', error as Error)
      scheduleReconnect()
    }
  }

  return {
    status, participants, update, settings, sessions,
    reloadRequired, banner, self, link,
    refreshSlice, watchParticipants, applyChange, connectEvents, showConnectionProblem, clearConnectionProblem,
  }
})
