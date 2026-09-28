import { ref } from 'vue'
import type { Settings } from '../api/types'

export type Theme = 'dark' | 'light'

/** Effective light/dark after resolving mode "system"; charts re-theme on change. */
export const theme = ref<Theme>(document.documentElement.classList.contains('dark') ? 'dark' : 'light')

const media = window.matchMedia?.('(prefers-color-scheme: dark)')
let mode: Settings['appearance']['mode'] = 'dark'

function save(key: string, value: string) {
  try {
    localStorage.setItem(key, value)
  } catch {
    /* storage blocked: first paint falls back to dark */
  }
}

function resolve(): Theme {
  if (mode === 'system') return media?.matches === false ? 'light' : 'dark'
  return mode
}

function paint() {
  const th = resolve()
  theme.value = th
  document.documentElement.classList.toggle('dark', th === 'dark')
  // index.html reads this before the first paint (no dark→light flash).
  save('iw.theme', mode)
}

media?.addEventListener('change', () => {
  if (mode === 'system') paint()
})

/** Applies the server-side appearance settings to this tab. */
export function applyAppearance(a: Settings['appearance']) {
  mode = a.mode
  paint()
}
