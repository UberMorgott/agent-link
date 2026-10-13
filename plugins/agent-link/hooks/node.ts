// The local node's API as this module's helpers see it: one request, the JSON
// it answers. The engine's `$` stays in register.ts (a hooks module's `$` is
// not followed across an import), which hands these helpers a NodeApi.
/** One answer of the node; `status` 0 when it did not answer at all. */
export type Answer = { isOk: boolean; status: number; text: string }

/** The node as a helper outside register.ts reaches it. */
export type NodeApi = {
  /** host:port, for an error's text. */
  api: string
  call: (method: string, path: string, query: Readonly<Record<string, string>>, body?: unknown) => Promise<Answer>
  /** `$.clock.now()`. */
  now: () => Promise<number>
}
/** The JSON of a 2xx answer, or undefined. */
export function parse<T>(a: Answer): T | undefined {
  if (!a.isOk) return undefined
  try {
    return JSON.parse(a.text) as T
  } catch {
    return undefined
  }
}

/** A path segment, escaped. */
export const seg = (s: string): string => encodeURIComponent(s)
