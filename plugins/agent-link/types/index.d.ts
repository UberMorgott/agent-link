/**
 * One batch of agent-link messages handed to the session and not yet
 * acknowledged to the node.
 *
 * `wake`: submitted as a prompt of its own to an idle session (`token` is in
 * the prompt, so the turn it starts is known by it). `append`: appended to
 * the running turn `turnId` while its step `afterStep` was in flight, so the
 * first later step carries it. `prompt`: attached to the person's own prompt,
 * carried by the turn that prompt starts.
 */
export type Delivery = {
  token: string
  mode: 'wake' | 'append' | 'prompt'
  /** `project|id` of each message (project may be empty). */
  keys: string[]
  /** chat id -> the message of it the session works on once it read this. */
  chats: Record<string, string>
  /** The turn that carries it; absent until that turn started. */
  turnId?: string
  /** `append` only: the step index in flight when it was appended (-1: none). */
  afterStep?: number
  /** How the node holds it: a wake claim (by `token`) or a turn's claim. */
  claim: 'wake' | 'turn'
  /** `$.clock.now()` when it was handed over, and when its claim was last taken. */
  at: number
  claimedAt: number
}

/** What the module knows of the session. */
export type Link = {
  /** host:port of the node's local API. */
  api: string
  project: string
  /** "" between a /clear's end and the id the process goes on under. */
  sessionId: string
  folder: string
  /** False when the node refused the folder or is down. */
  isRegistered: boolean
  /** False for a run nobody is behind (`claude -p`, a worker's job): tools only. */
  isAttended: boolean
  /** The session's first text for the model (the CLI's path) went out. */
  isGreeted: boolean
}

/** The running main turn: its id ("" while idle) and the step in flight (-1: none). */
export type Turn = { id: string; step: number }

/** chat id -> the message of that chat an agent works on. */
export type ChatMap = Record<string, string>

/** A live subagent of the session: its type and its last event. */
export type LiveAgent = { type: string; at: number }

/** Live subagents by id. */
export type AgentMap = Record<string, LiveAgent>

/** The chats of each subagent, by its id. */
export type AgentChats = Record<string, ChatMap>

declare module 'claude-code' {
  interface PluginState {
    'agent-link': {
      link: Link
      deliveries: Delivery[]
      turn: Turn
      /** Live subagents by id. */
      agents: AgentMap
      /** chat id -> message: the chats the main agent reports its activity to. */
      active: ChatMap
      /** The same per subagent, as they stood when it started. */
      agentChats: AgentChats
      /** Lines for the model that wait for its next delivery. */
      notes: string[]
    }
  }
}
