/**
 * One batch of agent-link messages handed to the session and not yet
 * acknowledged to the node.
 *
 * `wake`: submitted as a prompt of its own to an idle session (`token` is in
 * the prompt, so the turn it starts is known by it). `append`: appended to
 * the running turn `turnId` while its step `afterStep` was in flight, so the
 * first later step carries it.
 */
export type Delivery = {
  token: string
  mode: 'wake' | 'append'
  /** `project|id` of each message (project may be empty). */
  keys: string[]
  /** The messages as the model reads them, kept to hand them over again. */
  body: string
  /** The turn that carries it; absent until the wake's turn started. */
  turnId?: string
  /** `append` only: the step index in flight when it was appended (-1: none). */
  afterStep?: number
  /** `$.clock.now()` when it was handed over. */
  at: number
}

/** What the module knows of the session, kept across hot reloads. */
export type Link = {
  /** host:port of the node's local API. */
  api: string
  project: string
  sessionId: string
  folder: string
  /** False when the node refused the folder or is down. */
  isRegistered: boolean
}

declare module 'claude-code' {
  interface PluginState {
    'agent-link-native': {
      link: Link
      deliveries: Delivery[]
      /** The running main turn's id, "" while idle. */
      turn: string
    }
  }
}
