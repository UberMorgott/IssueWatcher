import { lang, t } from '../i18n'

// Intl formatters per UI language; reading lang() inside render keeps them reactive.
const cache = new Map<string, Intl.RelativeTimeFormat | Intl.DateTimeFormat | Intl.NumberFormat>()
function fmt<T extends Intl.RelativeTimeFormat | Intl.DateTimeFormat | Intl.NumberFormat>(kind: string, make: (l: string) => T): T {
  const k = kind + ':' + lang()
  let f = cache.get(k)
  if (!f) {
    f = make(lang())
    cache.set(k, f)
  }
  return f as T
}
const rtf = () => fmt('rel', (l) => new Intl.RelativeTimeFormat(l, { numeric: 'auto', style: 'short' }))
const dtf = () => fmt('abs', (l) => new Intl.DateTimeFormat(l, { dateStyle: 'medium', timeStyle: 'short' }))
const dayf = () => fmt('day', (l) => new Intl.DateTimeFormat(l, { day: '2-digit', month: '2-digit', timeZone: 'UTC' }))
const numf = () => fmt('num', (l) => new Intl.NumberFormat(l))
const minf = () => fmt('min', (l) => new Intl.NumberFormat(l, { style: 'unit', unit: 'minute', unitDisplay: 'short' }))
const secf = () => fmt('sec', (l) => new Intl.NumberFormat(l, { style: 'unit', unit: 'second', unitDisplay: 'short' }))

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

/** "3 hr. ago" / "3 ч назад"-style time; "" for empty/invalid input. */
export function relTime(iso: string | undefined, now = Date.now()): string {
  const d = parse(iso)
  if (!d) return ''
  const sec = Math.round((d.getTime() - now) / 1000)
  for (const [unit, size] of UNITS) {
    if (Math.abs(sec) >= size) return rtf().format(Math.round(sec / size), unit)
  }
  return t('time.justNow')
}

/** Local date + time for tooltips. */
export function absTime(iso: string | undefined): string {
  const d = parse(iso)
  return d ? dtf().format(d) : ''
}

/** "2026-09-28" (UTC week start) → "28.09" / "09/28". */
export function shortDay(iso: string): string {
  const d = parse(iso)
  return d ? dayf().format(d) : iso
}

export function num(n: number): string {
  return numf().format(n)
}

/** Go duration ("5m0s", "1h0m0s", "30s") → "5 мин" / "5 min"; unknown input as is. */
export function duration(s: string | undefined): string {
  if (!s) return ''
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/.exec(s)
  if (!m || !(m[1] || m[2] || m[3])) return s
  const secs = Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Number(m[3] ?? 0)
  return secs >= 60 && secs % 60 === 0 ? minf().format(secs / 60) : secf().format(secs)
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
