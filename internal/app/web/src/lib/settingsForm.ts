// The settings form as data: what a save sends, and when the area folder rows
// are complete enough to send. The legacy network's code is not the page's:
// it goes by joining and leaving (docs/plans/projects-v1.md §8).
import type { AppSettings, Project } from '@/types'

export const DEFAULT_API = '127.0.0.1:7520'

export interface SettingsFields {
  node: string
  handler: string
  agent_path: string
  work_dir: string
  listen: string
  api: string
  areas: string
  discovery: boolean
  max_jobs: string
  autostart: boolean
  auto_answer: boolean
}

export interface ProjectRow { key: number; area: string; dir: string; hooks: string }

export interface ProjectsBody { projects: Record<string, Project>; valid: boolean; duplicate: boolean }

export function effectiveAPI(settings: AppSettings | null | undefined): string {
  return String(settings?.api || DEFAULT_API).trim()
}

// projectsBody reads the rows: a blank row is skipped, a row with only an area
// or only a folder makes the rows incomplete, and one area twice is a duplicate.
export function projectsBody(rows: ProjectRow[]): ProjectsBody {
  const out: Record<string, Project> = {}
  let complete = true, duplicate = false
  for (const r of rows) {
    const area = r.area.trim(), dir = r.dir.trim()
    if (!area && !dir) continue
    if (!area || !dir) { complete = false; continue }
    if (Object.hasOwn(out, area)) { duplicate = true; continue }
    out[area] = { dir }
  }
  return { projects: out, valid: complete && !duplicate, duplicate }
}

// projectsKey compares project sets regardless of row order.
export function projectsKey(projects: Record<string, Project> | undefined): string {
  return JSON.stringify(Object.keys(projects || {}).sort().map((area) => [area, projects![area]!.dir || '']))
}

// settingsBody is the save request. Rows that are not valid yet are not sent:
// the saved projects go instead, so a half-typed row never replaces them.
export function settingsBody(f: SettingsFields, rows: ProjectsBody, saved: AppSettings | null): AppSettings {
  const maxJobs = f.max_jobs.trim()
  return {
    node: f.node.trim(),
    handler: f.handler,
    agent_path: f.agent_path,
    work_dir: f.work_dir.trim(),
    listen: f.listen.trim(),
    api: f.api.trim(),
    areas: f.areas.split(',').map((a) => a.trim()).filter(Boolean),
    projects: rows.valid ? rows.projects : (saved?.projects || {}),
    discovery: f.discovery,
    // Empty is the default; anything that is not a whole number is sent as -1
    // so the server names the field instead of silently using the default.
    max_jobs: maxJobs === '' ? 0 : (/^\d+$/.test(maxJobs) ? Number(maxJobs) : -1),
    autostart: f.autostart,
    auto_answer: f.auto_answer,
  }
}

