import { api } from './api'

// pickFolder asks the app for the native Windows folder dialog: a page cannot
// see absolute paths on disk. It returns the chosen folder, or the app's
// sentence of why there is none (cancelled, not Windows…).
export async function pickFolder(start: string): Promise<{ path?: string; message?: string }> {
  try {
    return await api<{ path?: string; message?: string }>('POST', 'pick-folder', { start: start.trim() })
  } catch (error) {
    return { message: (error as Error).message }
  }
}
