import { describe, expect, it } from 'vitest'
import { clock, utcOffset, when } from './time'

// One moment, read in two fixed zones: the reader's own local time, the date
// only off today, the UTC offset only when asked (titles).
describe('time', () => {
  const at = '2026-09-30T10:51:00Z'
  const now = new Date('2026-09-30T12:00:00Z')
  const plus3 = 'Europe/Moscow'
  const minus5 = 'Etc/GMT+5'

  it('clock is the reader-zone time, with the date on another day', () => {
    expect(clock(at, { now, timeZone: plus3 })).toBe('13:51')
    expect(clock(at, { now, timeZone: minus5 })).toBe('05:51')
    expect(clock(at, { now, timeZone: plus3, offset: true })).toBe('13:51 UTC+3')
    expect(clock(at, { now, timeZone: minus5, offset: true })).toBe('05:51 UTC-5')
    // 02:00Z is still the 29th in UTC-5 while now is the 30th there.
    expect(clock('2026-09-30T02:00:00Z', { now, timeZone: minus5 })).toBe('29.09 21:00')
    expect(clock('', { now })).toBe('')
  })

  it('when is the full local date and time', () => {
    expect(when(at, { timeZone: plus3 })).toBe('30.09.2026, 13:51:00')
    expect(when(at, { timeZone: minus5, offset: true })).toBe('30.09.2026, 05:51:00 UTC-5')
  })

  it('utcOffset names the zone offset', () => {
    const d = new Date(at)
    expect(utcOffset(d, 'UTC')).toBe('UTC')
    expect(utcOffset(d, 'Asia/Kolkata')).toBe('UTC+5:30')
  })
})
