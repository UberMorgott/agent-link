// The one place the web UI formats a moment for its reader: the browser's own
// time zone (or timeZone, for tests). The API's times are RFC 3339 UTC; the
// visible text stays short and the zone's UTC offset goes only into titles.

export interface TimeOpts {
  now?: Date
  timeZone?: string
  // offset appends the zone's UTC offset («13:51 UTC+3»), for titles.
  offset?: boolean
}

function parse(iso: string | undefined): Date | null {
  const at = new Date(iso || '')
  return Number.isNaN(at.getTime()) ? null : at
}

function parts(at: Date, timeZone?: string): Record<string, string> {
  const f = new Intl.DateTimeFormat('ru-RU', {
    timeZone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
  })
  return Object.fromEntries(f.formatToParts(at).map((p) => [p.type, p.value]))
}

// utcOffset is the zone's offset at that moment: «UTC», «UTC+3», «UTC-5», «UTC+5:30».
export function utcOffset(at: Date, timeZone?: string): string {
  const name = new Intl.DateTimeFormat('en-US', { timeZone, timeZoneName: 'shortOffset' })
    .formatToParts(at).find((p) => p.type === 'timeZoneName')?.value || 'GMT'
  return name === 'GMT+0' ? 'UTC' : name.replace('GMT', 'UTC')
}

// clock is a short time: «13:51» today, «29.09 13:51» on another day.
export function clock(iso: string | undefined, opts: TimeOpts = {}): string {
  const at = parse(iso)
  if (!at) return ''
  const p = parts(at, opts.timeZone)
  const n = parts(opts.now || new Date(), opts.timeZone)
  let text = p.hour + ':' + p.minute
  if (p.year !== n.year || p.month !== n.month || p.day !== n.day) text = p.day + '.' + p.month + ' ' + text
  return opts.offset ? text + ' ' + utcOffset(at, opts.timeZone) : text
}

// when is the full date and time: «30.09.2026, 13:51:00».
export function when(iso: string | undefined, opts: TimeOpts = {}): string {
  const at = parse(iso)
  if (!at) return ''
  const text = at.toLocaleString('ru-RU', { timeZone: opts.timeZone })
  return opts.offset ? text + ' ' + utcOffset(at, opts.timeZone) : text
}
