// Runs before every test file: a clean page, storage and dictionary.
import { afterEach, beforeEach, vi } from 'vitest'
import { runtime } from '@/lib/runtime'
import { unmountAll } from './harness'

beforeEach(() => {
  localStorage.clear()
  runtime.strings = {}
  runtime.version = ''
  runtime.token = 'test-token'
  // jsdom lays nothing out and cannot scroll an element into view.
  Element.prototype.scrollIntoView = vi.fn()
  // Nor does it observe sizes (the composer grows with its text).
  vi.stubGlobal('ResizeObserver', class { observe() {} unobserve() {} disconnect() {} })
})

afterEach(() => {
  unmountAll()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  vi.useRealTimers()
  document.body.innerHTML = ''
})
