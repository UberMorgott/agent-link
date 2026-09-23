import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import { ApiError, CORE_SLICES, TOKEN_HEADER, api, reloadOnNewVersion, type CoreSlice } from '@/lib/api'
import { runtime, t, fmt } from '@/lib/runtime'
import type { AppSettings, DashboardSummary, ParticipantView, Session, Status, UpdateStatus } from '@/types'
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
    if (topic === 'peer' || topic === 'members') {
      slices.add('status'); slices.add('dashboard'); slices.add('participants')
    }
    if (topic === 'messages' || topic === 'worker') {
      slices.add('dashboard'); slices.add('participants')
    }
    // The sessions list is shown filtered by project.
    if (topic.startsWith('project:')) slices.add('sessions')
  }
  return [...slices]
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
  else text = fmt("link.off_count", { online: s.online || 0, total: s.total || 0 })
  if (s.configured && !s.zerotier) text += ' · ' + t("link.no_zerotier")
  if (s.warning) { text += ' · ' + t(s.warning); cls = 'off' }
  return { text, cls }
}

export const useAppStore = defineStore('app', () => {
  const status = shallowRef<Status | null>(null)
  const dashboard = shallowRef<DashboardSummary | null>(null)
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

  function setSlice(name: CoreSlice, value: unknown) {
    const target = { status, dashboard, participants, update, settings, sessions }[name]
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

  function clearConnectionProblem(name: string) {
    coreFailures.delete(name)
    if (!reloadRequired.value && !coreFailures.size) banner.value = ''
  }

  async function refreshSlice(name: CoreSlice): Promise<void> {
    try {
      setSlice(name, await api('GET', name))
      clearConnectionProblem(name)
    } catch (error) { showConnectionProblem(name, error as ApiError) }
  }

  // guarded runs one projects refresh under the connection banner.
  async function guarded(name: string, run: () => Promise<void>) {
    try {
      await run()
      clearConnectionProblem(name)
    } catch (error) { showConnectionProblem(name, error as ApiError) }
  }

  function applyChange(event: ChangeEvent) {
    const topics = Array.isArray(event.topics) ? event.topics : []
    const projects = useProjectsStore()
    const scope = projectsForTopics(topics)
    const jobs: Promise<void>[] = slicesForTopics(topics).map(refreshSlice)
    if (scope.all) jobs.push(guarded('projects', projects.refreshAll))
    else if (scope.list) jobs.push(guarded('projects', projects.refreshList))
    for (const pid of scope.scoped) jobs.push(guarded('project:' + pid, () => projects.refreshScoped(pid)))
    return Promise.all(jobs)
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
    status, dashboard, participants, update, settings, sessions,
    reloadRequired, banner, self, link,
    refreshSlice, applyChange, connectEvents, showConnectionProblem, clearConnectionProblem,
  }
})
