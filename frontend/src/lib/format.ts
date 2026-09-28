const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto', style: 'short' })
const dtf = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })

const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 365 * 24 * 3600],
  ['month', 30 * 24 * 3600],
  ['week', 7 * 24 * 3600],
  ['day', 24 * 3600],
  ['hour', 3600],
  ['minute', 60],
]

function parse(iso: string | undefined): Date | null {
  if (!iso) return null
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? null : d
}

/** "3 hr. ago"-style time; "" for empty/invalid input. */
export function relTime(iso: string | undefined, now = Date.now()): string {
  const d = parse(iso)
  if (!d) return ''
  const sec = Math.round((d.getTime() - now) / 1000)
  for (const [unit, size] of UNITS) {
    if (Math.abs(sec) >= size) return rtf.format(Math.round(sec / size), unit)
  }
  return 'just now'
}

/** Local date + time for tooltips. */
export function absTime(iso: string | undefined): string {
  const d = parse(iso)
  return d ? dtf.format(d) : ''
}

function hash(s: string): number {
  let h = 2166136261
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i)
    h = Math.imul(h, 16777619)
  }
  return h >>> 0
}

/** Stable hue for a label name (chip colours survive reloads and filters). */
export function labelHue(name: string): number {
  const known: Record<string, number> = { bug: 356, enhancement: 200, feature: 200, question: 280, documentation: 210, duplicate: 30, wontfix: 0, 'good first issue': 160, 'help wanted': 140 }
  const k = name.toLowerCase()
  return known[k] ?? hash(k) % 360
}

/** Series colours for per-project comparisons, fixed by project id. */
export const SERIES = ['#86a5ff', '#48c992', '#b58cfa', '#55c7d4', '#eca86a', '#e779ad', '#f1bb69', '#f17e84']

export function repoColor(id: number): string {
  return SERIES[Math.abs(id) % SERIES.length]
}

/** "owner/name" → "name". */
export function shortRepo(name: string): string {
  const i = name.lastIndexOf('/')
  return i >= 0 ? name.slice(i + 1) : name
}

export function repoOwner(name: string): string {
  const i = name.lastIndexOf('/')
  return i >= 0 ? name.slice(0, i) : ''
}

export function plural(n: number, one: string, many = one + 's'): string {
  return `${n} ${n === 1 ? one : many}`
}
