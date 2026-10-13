// agent-link for Claude Code as an in-process hooks module. This much of it
// serves the tools of `agentlink mcp` (cmd/agentlink/mcp.go) without the MCP
// process: session.start registers the nine of them (tools.gen.ts) and
// tool.call answers each with the same call of the node's local API the MCP
// server makes (tools.ts).
import type { EngineInterface, Register } from 'claude-code'

import type { Link } from '../types'

import type { Answer, NodeApi } from './node'
import type { Caller } from './tools'
import { serve } from './tools'
import { TOOLS } from './tools.gen'

type Engine = EngineInterface

const LINK = { plugin: 'agent-link', key: 'link' } as const

const TOOL_PREFIX = 'mcp__agent-link__'

let job = ''
let seat = ''

/** settings.DefaultAPI: the desktop app's local API when nothing says else. */
const DEFAULT_API = '127.0.0.1:7520'

/** The node's local API: $AGENTLINK_API, else the desktop app's settings. */
async function findApi($: Engine): Promise<string> {
  const fromEnv = (await $.env.get('AGENTLINK_API'))?.trim()
  if (fromEnv !== undefined && fromEnv !== '') return fromEnv
  const appData = await $.env.get('APPDATA')
  const xdg = await $.env.get('XDG_CONFIG_HOME')
  const home = await $.env.get('HOME')
  const dir = appData ?? xdg ?? (home === undefined ? undefined : `${home}/.config`)
  if (dir === undefined) return DEFAULT_API
  try {
    // Only `api` is read; the file's other values never leave this function.
    const settings: unknown = JSON.parse(await $.fs.read(`${dir}/agentlink/config.json`))
    if (typeof settings === 'object' && settings !== null && 'api' in settings) {
      const api = settings.api
      if (typeof api === 'string' && api.trim() !== '') return api.trim()
    }
  } catch {
    // no settings file: the default
  }
  return DEFAULT_API
}

/** One call of the node's local API; never throws. */
async function call(
  $: Engine,
  api: string,
  method: string,
  path: string,
  query: Readonly<Record<string, string>>,
  body?: unknown,
): Promise<Answer> {
  const pairs = Object.entries(query).map(([k, v]) => `${k}=${encodeURIComponent(v)}`)
  const url = `http://${api}${path}${pairs.length > 0 ? `?${pairs.join('&')}` : ''}`
  try {
    const res = await $.http.fetch(url, {
      method,
      ...(body === undefined
        ? {}
        : { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }),
    })
    return { isOk: res.ok, status: res.status, text: res.text }
  } catch {
    return { isOk: false, status: 0, text: '' }
  }
}

/** The node, for the helpers outside this file (which never hold `$`). */
const nodeOf = ($: Engine, link: Link): NodeApi => ({
  api: link.api,
  call: (method, path, query, body) => call($, link.api, method, path, query, body),
  now: () => $.clock.now(),
})

/** Who calls a tool: the session, and its subagent when one does. */
function caller(link: Link, agentId: string): Caller {
  return { link, agentId, agentType: '', sessionId: job === '' ? link.sessionId : '', seat, job }
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    job = ((await $.env.get('AGENTLINK_JOB_ID')) ?? '').trim()
    seat = ((await $.env.get('AGENTLINK_SEAT')) ?? '').trim()
    await $.state.set(LINK, {
      api: await findApi($),
      project: ((await $.env.get('AGENTLINK_PROJECT_ID')) ?? '').trim(),
      sessionId: await $.session.id(),
      folder: e.cwd,
      isRegistered: false,
      isAttended: false,
      isGreeted: false,
    })
    for (const tool of TOOLS) {
      await $.tool.register({ name: tool.name, description: tool.description, inputSchema: tool.inputSchema })
    }
    return next(e)
  })

  on('tool.call', async ($, e, next) => {
    const tool = String(e.tool)
    if (!tool.startsWith(TOOL_PREFIX)) return next(e)
    const link = (await $.state.get(LINK)).value
    if (link === undefined) return { deny: 'agentlink: this session has no local node' }
    const args = e as unknown as Readonly<Record<string, unknown>>
    const answer = await serve(nodeOf($, link), caller(link, e.agentId ?? ''), tool.slice(TOOL_PREFIX.length), args)
    return 'error' in answer ? { deny: answer.error } : { result: answer.text }
  }).catch((_$, e, next) => next(e)) // this hook failing never holds a tool call back
}
