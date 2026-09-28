import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api } from '../api/client'
import type { LiveItemEvent, Provider, Repo, SyncStatus } from '../api/types'

export type Theme = 'dark' | 'light'

export interface ActivityEntry {
  key: number
  kind: 'item.new' | 'comment.new' | 'item.closed'
  at: string
  data: LiveItemEvent
}

function load(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

function save(key: string, value: string) {
  try {
    localStorage.setItem(key, value)
  } catch {
    /* storage blocked: preference lasts for this tab only */
  }
}

let activitySeq = 0

/** App-wide state: theme, shell, connections, sync status, repo list, live feed. */
export const useAppStore = defineStore('app', () => {
  // --- shell
  const theme = ref<Theme>(document.documentElement.classList.contains('dark') ? 'dark' : 'light')
  function setTheme(t: Theme) {
    theme.value = t
    document.documentElement.classList.toggle('dark', t === 'dark')
    save('iw.theme', t)
  }
  const sidebarCollapsed = ref(load('iw.sidebar') === 'collapsed')
  function toggleSidebar() {
    sidebarCollapsed.value = !sidebarCollapsed.value
    save('iw.sidebar', sidebarCollapsed.value ? 'collapsed' : 'expanded')
  }
  const version = ref('')

  // --- connections
  const providers = ref<Provider[]>([])
  const authLoaded = ref(false)
  const authError = ref('')
  const github = computed(() => providers.value.find((p) => p.id === 'github'))
  const githubConnected = computed(() => !!github.value?.connected)

  async function loadAuth() {
    const r = await api.authStatus()
    if (r.ok) {
      providers.value = r.data
      authError.value = ''
    } else {
      authError.value = r.status === 404 ? 'Sign-in is not available in this build' : r.error
    }
    authLoaded.value = true
  }

  /** Starts sign-in. Returns true when the server opened the platform page in a browser tab. */
  async function connect(id: string): Promise<boolean> {
    authError.value = ''
    const r = await api.authStart(id)
    if (!r.ok) {
      authError.value = r.error
      return false
    }
    if (r.data.opened) {
      // Completion arrives as auth.changed over SSE.
      providers.value = providers.value.map((p) => (p.id === id ? { ...p, state: 'connecting' } : p))
      return true
    }
    window.location.assign(r.data.url) // server could not open a browser: continue in this tab
    return false
  }

  async function disconnect(id: string) {
    const r = await api.authLogout(id)
    if (!r.ok) authError.value = r.error
    await loadAuth()
  }

  // --- sync
  const sync = ref<SyncStatus | null>(null)
  const syncRequested = ref(false)
  async function loadSync() {
    const r = await api.syncStatus()
    if (r.ok) sync.value = r.data
  }
  let pollTimer: number | undefined
  async function syncNow() {
    syncRequested.value = true
    const r = await api.syncNow()
    if (!r.ok) {
      syncRequested.value = false
      return r.error
    }
    // The poller has no "started" event: poll until the cycle ends (max ~2 min).
    window.clearTimeout(pollTimer)
    let tries = 0
    const tick = async () => {
      await loadSync()
      tries++
      if ((sync.value?.running || tries < 2) && tries < 60) {
        pollTimer = window.setTimeout(tick, 2000)
      } else {
        syncRequested.value = false
        bump()
      }
    }
    pollTimer = window.setTimeout(tick, 600)
    return ''
  }
  const syncing = computed(() => syncRequested.value || !!sync.value?.running)

  // --- data
  const repos = ref<Repo[]>([])
  const reposLoaded = ref(false)
  const reposAvailable = ref(true)
  async function loadRepos() {
    const r = await api.repos()
    if (r.ok) repos.value = r.data ?? []
    reposAvailable.value = r.ok || r.status !== 404
    reposLoaded.value = true
  }
  const unreadTotal = computed(() => repos.value.reduce((n, r) => n + r.unread, 0))
  /** First run: nothing connected and nothing synced yet → pages show the Connect CTA. */
  const onboarding = computed(() => authLoaded.value && reposLoaded.value && !githubConnected.value && repos.value.length === 0)
  /** Signed out but earlier data is still in the local database. */
  const offlineData = computed(() => authLoaded.value && !githubConnected.value && repos.value.length > 0)

  /** Bumped when live data changed; views watch it and refetch. */
  const dataVersion = ref(0)
  function bump() {
    dataVersion.value++
    void loadRepos()
  }

  const activity = ref<ActivityEntry[]>([])
  function pushActivity(kind: ActivityEntry['kind'], data: LiveItemEvent) {
    activity.value = [{ key: ++activitySeq, kind, at: new Date().toISOString(), data }, ...activity.value].slice(0, 50)
  }

  async function init() {
    const h = await api.health()
    if (h.ok) version.value = h.data.version
    await Promise.all([loadAuth(), loadSync(), loadRepos()])
  }

  return {
    theme,
    setTheme,
    sidebarCollapsed,
    toggleSidebar,
    version,
    providers,
    authLoaded,
    authError,
    github,
    githubConnected,
    loadAuth,
    connect,
    disconnect,
    sync,
    loadSync,
    syncNow,
    syncing,
    repos,
    reposLoaded,
    reposAvailable,
    loadRepos,
    unreadTotal,
    onboarding,
    offlineData,
    dataVersion,
    bump,
    activity,
    pushActivity,
    init,
  }
})
