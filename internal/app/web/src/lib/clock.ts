// One shared clock for everything on screen that shows elapsed time or
// expires with it: it ticks once a second while a view uses it.
import { createSharedComposable, useIntervalFn, useNow } from '@vueuse/core'

export const useClock = createSharedComposable(() => useNow({ scheduler: cb => useIntervalFn(cb, 1000) }))
