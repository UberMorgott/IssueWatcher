import { onBeforeUnmount, onMounted } from 'vue'

export interface ShortcutActions {
  sync: () => void
  search: () => void
  help: () => void
  sidebar: () => void
  go: (path: string) => void
}

// Physical keys (e.code): the shortcuts work on any keyboard layout (ЙЦУКЕН too).
const GO: Record<string, string> = {
  KeyO: '/',
  KeyI: '/issues',
  KeyM: '/comments',
  KeyP: '/projects',
  KeyJ: '/jobs',
  KeyA: '/settings/agents',
  KeyC: '/connections',
  KeyS: '/settings',
}

/** True when a key press belongs to a text field or an open overlay. */
export function typing(e: KeyboardEvent): boolean {
  const t = e.target as HTMLElement | null
  if (!t) return false
  return t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName) || !!t.closest('[role="dialog"], [role="listbox"], [role="menu"]')
}

/**
 * Global single-key shortcuts (ignored while typing or with Ctrl/Alt/Meta).
 * Listens in the capture phase: the second key of a "G, then …" jump is
 * consumed here, so a page's own keys (J/K in a list) do not also fire.
 */
export function useShortcuts(a: ShortcutActions) {
  let gAt = 0
  const onKey = (e: KeyboardEvent) => {
    if (e.ctrlKey || e.metaKey || e.altKey || typing(e)) return
    if (gAt && Date.now() - gAt < 1200 && GO[e.code]) {
      gAt = 0
      e.preventDefault()
      e.stopPropagation()
      a.go(GO[e.code])
      return
    }
    gAt = 0
    switch (e.code) {
      case 'Slash':
      case 'NumpadDivide':
        e.preventDefault()
        if (e.shiftKey) a.help() // "?"
        else a.search()
        break
      case 'BracketLeft':
        a.sidebar()
        break
      case 'KeyR':
        a.sync()
        break
      case 'KeyG':
        gAt = Date.now()
        break
    }
  }
  onMounted(() => window.addEventListener('keydown', onKey, true))
  onBeforeUnmount(() => window.removeEventListener('keydown', onKey, true))
}
