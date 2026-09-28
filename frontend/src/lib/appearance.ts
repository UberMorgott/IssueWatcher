import { ref } from 'vue'
import { palette as scale, updatePreset } from '@primeuix/themes'
import type { Appearance, Colors, Palette } from '../api/types'

/**
 * Runtime theming. The server stores mode + palette + custom colours + type
 * settings (data\config.json, so every browser looks the same); this module
 * turns the four base colours of the active mode into the app tokens (--iw-*
 * on <html>), PrimeVue's primary/surface scales (updatePreset + palette()) and
 * bumps themeVersion so ECharts re-reads its colours.
 */

export type Theme = 'dark' | 'light'

/** Effective light/dark after resolving mode "system". */
export const theme = ref<Theme>(document.documentElement.classList.contains('dark') ? 'dark' : 'light')
/** Bumped after every repaint: charts watch it and re-read the colours. */
export const themeVersion = ref(0)
/** Row height of the virtual Issues list (px): density × font scale. */
export const rowHeight = ref(60)

export const FONT_STACKS: Record<Appearance['fontFamily'], string> = {
  inter: "'Inter Variable', 'Segoe UI', system-ui, sans-serif",
  segoe: "'Segoe UI Variable Text', 'Segoe UI', system-ui, sans-serif",
  system: 'system-ui, -apple-system, sans-serif',
  mono: "'Cascadia Code', 'Cascadia Mono', ui-monospace, consolas, monospace",
}
const ROW: Record<Appearance['density'], number> = { compact: 48, comfortable: 60, spacious: 72 }
const CELL: Record<Appearance['density'], string> = { compact: '0.3rem 0.75rem', comfortable: '0.5rem 0.875rem', spacious: '0.85rem 1rem' }

const FALLBACK: Colors = { accent: '#86a5ff', background: '#0b1017', surface: '#121a24', text: '#ecf2f8' }

// --- colour math on #rrggbb
function rgb(hex: string): [number, number, number] {
  const v = parseInt(hex.slice(1), 16)
  return [(v >> 16) & 255, (v >> 8) & 255, v & 255]
}
function toHex(c: number[]): string {
  return '#' + c.map((x) => Math.round(Math.min(255, Math.max(0, x))).toString(16).padStart(2, '0')).join('')
}
/** a moved towards b by w (0..1). */
export function mix(a: string, b: string, w: number): string {
  const x = rgb(a)
  const y = rgb(b)
  return toHex(x.map((c, i) => c + (y[i] - c) * w))
}
function alpha(a: string, w: number): string {
  const [r, g, b] = rgb(a)
  return `rgb(${r} ${g} ${b} / ${Math.round(w * 100)}%)`
}
function lum(h: string): number {
  const f = (c: number) => {
    const s = c / 255
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
  }
  const [r, g, b] = rgb(h)
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
}
/** WCAG contrast ratio (1–21), same formula as the server check. */
export function contrast(a: string, b: string): number {
  const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p)
  return (x + 0.05) / (y + 0.05)
}

/** Base colours of mode for appearance a: the palette, then non-empty custom overrides. */
export function effectiveColors(a: Appearance, mode: Theme, list: Palette[]): Colors {
  const p = list.find((x) => x.id === a.paletteId) ?? list[0]
  const base: Colors = p ? { ...p[mode] } : { ...FALLBACK }
  const custom = a.custom?.[mode]
  if (custom) for (const k of ['accent', 'background', 'surface', 'text'] as const) if (custom[k]) base[k] = custom[k].toLowerCase()
  return base
}

/** The full token set derived from four base colours (also used for the live preview). */
export function tokens(c: Colors, mode: Theme): Record<string, string> {
  const dark = mode === 'dark'
  const { accent, background: bg, surface, text } = c
  const onPrimary = contrast(accent, '#ffffff') >= contrast(accent, '#0b1017') ? '#ffffff' : '#0b1017'
  // Muted text stays readable (AA 4.5:1 on the surface) whatever the custom colours.
  let w = 0.38
  let muted = mix(text, surface, w)
  while (contrast(muted, surface) < 4.5 && w > 0.02) muted = mix(text, surface, (w -= 0.04))
  return {
    '--iw-bg': bg,
    '--iw-surface': surface,
    '--iw-elevated': mix(surface, text, dark ? 0.06 : 0.045),
    '--iw-hover': mix(surface, text, dark ? 0.09 : 0.07),
    '--iw-border': mix(surface, text, dark ? 0.14 : 0.13),
    '--iw-border-strong': mix(surface, text, dark ? 0.22 : 0.21),
    '--iw-text': text,
    '--iw-muted': muted,
    '--iw-dimmed': mix(text, surface, 0.55),
    '--iw-primary': accent,
    '--iw-primary-hover': mix(accent, dark ? '#ffffff' : '#000000', 0.18),
    '--iw-primary-soft': alpha(accent, dark ? 0.16 : 0.1),
    '--iw-on-primary': onPrimary,
    '--iw-chart-opened': accent,
  }
}

const media = window.matchMedia?.('(prefers-color-scheme: dark)')
let current: Appearance | null = null
let palettes: Palette[] = []

function resolveMode(a: Appearance): Theme {
  if (a.mode === 'system') return media?.matches === false ? 'light' : 'dark'
  return a.mode
}

function save(key: string, value: string) {
  try {
    localStorage.setItem(key, value)
  } catch {
    /* storage blocked: the first paint falls back to the default look */
  }
}

function paint() {
  const a = current
  if (!a) return
  const mode = resolveMode(a)
  const root = document.documentElement
  const c = effectiveColors(a, mode, palettes)
  const tk = tokens(c, mode)
  const font = FONT_STACKS[a.fontFamily] ?? FONT_STACKS.inter
  root.classList.toggle('dark', mode === 'dark')
  for (const [k, v] of Object.entries(tk)) root.style.setProperty(k, v)
  root.style.setProperty('--iw-font', font)
  root.style.setProperty('--iw-fs', String(a.fontScale || 1))
  root.dataset.density = a.density
  theme.value = mode
  rowHeight.value = Math.round((ROW[a.density] ?? 60) * Math.max(1, a.fontScale || 1))
  // PrimeVue: accent scale via palette(); surfaces from light to dark (0 → 950).
  const steps = [0, 50, 100, 200, 300, 400, 500, 600, 700, 800, 900, 950]
  const from = mode === 'dark' ? c.text : c.surface
  const to = mode === 'dark' ? c.background : c.text
  const surface = Object.fromEntries(steps.map((s, i) => [s, mix(from, to, i / (steps.length - 1))]))
  const cell = CELL[a.density] ?? CELL.comfortable
  updatePreset({
    semantic: { primary: { ...scale(c.accent) }, surface },
    components: { datatable: { bodyCell: { padding: cell }, headerCell: { padding: cell } } },
  })
  // index.html applies these before the first paint (no flash of the default look).
  save('iw.theme', a.mode)
  save('iw.paint', JSON.stringify({ dark: mode === 'dark', tokens: tk, font, fs: a.fontScale || 1 }))
  themeVersion.value++
}

media?.addEventListener('change', () => {
  if (current?.mode === 'system') paint()
})

/** Applies the server-side appearance settings (and the palette list) to this tab. */
export function applyAppearance(a: Appearance, list: Palette[]) {
  current = a
  if (list?.length) palettes = list
  paint()
}
