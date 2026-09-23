// The settings form as data: what a save sends, and when the project rows are
// complete enough to send.
import type { AppSettings, Project } from '@/types'

export const DEFAULT_API = '127.0.0.1:7520'
// Letters and digits without 0/O and 1/I: 32 symbols, so a byte & 31 is uniform.
export const CODE_ALPHABET = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'

export interface SettingsFields {
  node: string
  code: string
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
    code: f.code.trim(),
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

// newCode is 12 random symbols, 60 bits, shown as XXXX-XXXX-XXXX.
export function newCode(random: (bytes: Uint8Array<ArrayBuffer>) => Uint8Array<ArrayBuffer> = (b) => crypto.getRandomValues(b)): string {
  const bytes = random(new Uint8Array(12))
  const s = Array.from(bytes, (b) => CODE_ALPHABET[b & 31]).join('')
  return s.slice(0, 4) + '-' + s.slice(4, 8) + '-' + s.slice(8)
}
