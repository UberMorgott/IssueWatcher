import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api } from '../api/client'
import type { Capabilities, DataChange, LiveItemEvent, PlatformStatus, Provider, Repo, SyncProgress, SyncStatus } from '../api/types'
import { fallbackCaps } from '../lib/platforms'
import { t } from '../i18n'
import { theme, type Theme } from '../lib/appearance'
import { useSettingsStore } from './settings'

export type { Theme }

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
  // --- shell: the effective light/dark theme (lib/appearance); setTheme saves
  // the mode server-side, so every tab and browser follows.
  function setTheme(t: Theme) {
    void useSettingsStore().patch({ appearance: { mode: t } })
  }
  const sidebarCollapsed = ref(load('iw.sidebar') === 'collapsed')
  function toggleSidebar() {
    sidebarCollapsed.value = !sidebarCollapsed.value
    save('iw.sidebar', sidebarCollapsed.value ? 'collapsed' : 'expanded')
  }
  const version = ref('')
  /** A newer release is available (Settings › Updates; set by the update status). */
  const updateAvailable = ref(false)

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
      authError.value = r.status === 404 ? t('common.signInUnavailable') : r.error
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

  // --- platforms (Settings › Платформы): state + capabilities per platform
  const platforms = ref<PlatformStatus[]>([])
  async function loadPlatforms() {
    const r = await api.platforms()
    if (r.ok) platforms.value = r.data ?? []
  }
  function setPlatform(p: PlatformStatus) {
    platforms.value = platforms.value.some((x) => x.id === p.id) ? platforms.value.map((x) => (x.id === p.id ? p : x)) : [...platforms.value, p]
  }
  /** A platform's capabilities (labels, reply, threading); a fallback until loaded. */
  function caps(platform: string): Capabilities {
    return platforms.value.find((p) => p.id === platform)?.capabilities ?? fallbackCaps(platform)
  }

  // --- sync (progress arrives as sync.status live events)
  const sync = ref<SyncStatus | null>(null)
  const syncRequested = ref(false)
  const progress = ref<SyncProgress | null>(null)
  async function loadSync() {
    const r = await api.syncStatus()
    if (r.ok) sync.value = r.data
  }
  let requestTimer: number | undefined
  async function syncNow() {
    syncRequested.value = true
    const r = await api.syncNow()
    if (!r.ok) {
      syncRequested.value = false
      return r.error
    }
    // The cycle reports itself (sync.status started…done); signed out it stays
    // silent, so the request indicator gives up after a while.
    window.clearTimeout(requestTimer)
    requestTimer = window.setTimeout(() => {
      syncRequested.value = false
      void loadSync()
    }, 15000)
    return ''
  }
  /** Applies one sync.status step; a finished cycle refreshes status and counts. */
  async function onSyncStatus(p: SyncProgress | null) {
    syncRequested.value = false
    window.clearTimeout(requestTimer)
    if (p && (p.state === 'started' || p.state === 'progress')) {
      progress.value = p
      return
    }
    progress.value = null
    void loadRepos() // unread badge, per-project last sync
    void loadPlatforms() // account state per platform
    const wasSignedIn = sync.value?.signedIn
    await loadSync()
    if (sync.value && sync.value.signedIn !== wasSignedIn) await loadAuth()
  }
  const syncing = computed(() => syncRequested.value || !!progress.value || !!sync.value?.running)

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
  const offlineData = computed(() => authLoaded.value && !githubConnected.value && repos.value.some((r) => r.platform === 'github'))

  /**
   * Bumped when stored data changed (data.changed, auth.changed, SSE reconnect);
   * every view watches it and refetches quietly, keeping selection, filters and scroll.
   */
  const dataVersion = ref(0)
  /**
   * What changed since the previous bump (read when dataVersion changes); null =
   * unknown (reconnect, auth, sync without details): views re-check everything.
   */
  let changes: DataChange[] | null = []
  const lastChanges = ref<DataChange[] | null>(null)
  function flush(what: DataChange[] | null) {
    window.clearTimeout(invalidateTimer)
    lastChanges.value = what
    changes = []
    dataVersion.value++
    void loadRepos()
  }
  /** Immediate full refresh (reconnect, take-over). */
  function bump() {
    flush(null)
  }
  let invalidateTimer: number | undefined
  /** Debounced bump: a burst of changes (sync of many projects) refetches once. */
  function invalidate(change?: DataChange) {
    if (change && changes) changes.push(change)
    else changes = null
    window.clearTimeout(invalidateTimer)
    invalidateTimer = window.setTimeout(() => flush(changes), 300)
  }

  const activity = ref<ActivityEntry[]>([])
  function pushActivity(kind: ActivityEntry['kind'], data: LiveItemEvent) {
    activity.value = [{ key: ++activitySeq, kind, at: new Date().toISOString(), data }, ...activity.value].slice(0, 50)
  }

  async function init() {
    const h = await api.health()
    if (h.ok) version.value = h.data.version
    await Promise.all([loadAuth(), loadSync(), loadRepos(), loadPlatforms()])
  }

  return {
    theme,
    setTheme,
    sidebarCollapsed,
    toggleSidebar,
    version,
    updateAvailable,
    providers,
    authLoaded,
    authError,
    github,
    githubConnected,
    loadAuth,
    connect,
    disconnect,
    platforms,
    loadPlatforms,
    setPlatform,
    caps,
    sync,
    loadSync,
    syncNow,
    syncing,
    progress,
    onSyncStatus,
    repos,
    reposLoaded,
    reposAvailable,
    loadRepos,
    unreadTotal,
    onboarding,
    offlineData,
    dataVersion,
    lastChanges,
    bump,
    invalidate,
    activity,
    pushActivity,
    init,
  }
})
