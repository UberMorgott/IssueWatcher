import { onBeforeUnmount, onMounted } from 'vue'

export interface ShortcutActions {
  sync: () => void
  search: () => void
  help: () => void
  sidebar: () => void
  go: (path: string) => void
}

const GO: Record<string, string> = { o: '/', i: '/issues', p: '/projects', a: '/agents', c: '/connections', s: '/settings' }

/** True when a key press belongs to a text field or an open overlay. */
export function typing(e: KeyboardEvent): boolean {
  const t = e.target as HTMLElement | null
  if (!t) return false
  return t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName) || !!t.closest('[role="dialog"], [role="listbox"], [role="menu"]')
}

/** Global single-key shortcuts (ignored while typing or with modifiers). */
export function useShortcuts(a: ShortcutActions) {
  let gAt = 0
  const onKey = (e: KeyboardEvent) => {
    if (e.ctrlKey || e.metaKey || e.altKey || typing(e)) return
    const k = e.key.toLowerCase()
    if (gAt && Date.now() - gAt < 1200 && GO[k]) {
      gAt = 0
      e.preventDefault()
      a.go(GO[k])
      return
    }
    gAt = 0
    switch (e.key) {
      case '/':
        e.preventDefault()
        a.search()
        break
      case '?':
        e.preventDefault()
        a.help()
        break
      case '[':
        a.sidebar()
        break
      case 'r':
      case 'R':
        a.sync()
        break
      case 'g':
      case 'G':
        gAt = Date.now()
        break
    }
  }
  onMounted(() => window.addEventListener('keydown', onKey))
  onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
}
