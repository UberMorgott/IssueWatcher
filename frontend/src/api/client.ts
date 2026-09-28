import type {
  Comment,
  CommentChunk,
  Health,
  IssueDetail,
  IssueChunk,
  IssueQuery,
  Provider,
  Repo,
  RepoChunk,
  SettingsDoc,
  SettingsPatch,
  FolderRow,
  FolderSuggestion,
  Stats,
  SyncStatus,
} from './types'
import { t, te } from '../i18n'

/**
 * Result of an API call. Never throws: pages render empty/error states from
 * `ok` + `status` instead (404 = endpoint not built yet, 401 = session gone).
 */
export type Result<T> = { ok: true; data: T; status: number } | { ok: false; status: number; error: string }

async function call<T>(method: string, url: string, body?: unknown): Promise<Result<T>> {
  let res: Response
  try {
    res = await fetch(url, {
      method,
      headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
    })
  } catch {
    return { ok: false, status: 0, error: t('common.notRunning') }
  }
  if (!res.ok) {
    let error = res.statusText || `HTTP ${res.status}`
    try {
      const j = (await res.json()) as { error?: string }
      if (j.error) error = j.error
    } catch {
      /* non-JSON error body */
    }
    return { ok: false, status: res.status, error }
  }
  if (res.status === 204 || res.status === 202) return { ok: true, data: undefined as T, status: res.status }
  try {
    return { ok: true, data: (await res.json()) as T, status: res.status }
  } catch {
    return { ok: false, status: res.status, error: t('common.badResponse') }
  }
}

// --- auth -------------------------------------------------------------------

/** Legacy single-provider shape served by internal/api/github_auth.go. */
interface GitHubAuthStatus {
  app: boolean
  appSlug?: string
  appUrl?: string
  installUrl?: string
  signedIn: boolean
  login?: string
  device?: Provider['device']
}

function avatarFor(login?: string): string | undefined {
  return login ? `https://github.com/${encodeURIComponent(login)}.png?size=96` : undefined
}

/** Accepts both the multi-provider shape and the GitHub-only shape. */
function normaliseAuth(raw: unknown): Provider[] {
  const r = raw as { providers?: Provider[] } & Partial<GitHubAuthStatus>
  if (Array.isArray(r.providers)) {
    // GitHub extras (app links, device flow) still come as top-level legacy fields.
    return r.providers.map((p) =>
      p.id === 'github'
        ? { ...p, avatarUrl: p.avatarUrl || avatarFor(p.login), appUrl: r.appUrl, installUrl: r.installUrl, device: r.device }
        : p,
    )
  }
  const gh = r as GitHubAuthStatus
  return [
    {
      id: 'github',
      name: 'GitHub',
      connected: !!gh.signedIn,
      login: gh.login,
      avatarUrl: avatarFor(gh.login),
      setupNeeded: !gh.app,
      state: gh.signedIn ? 'connected' : gh.device?.pending ? 'connecting' : gh.device?.error ? 'error' : 'disconnected',
      error: gh.device?.error,
      appUrl: gh.appUrl,
      installUrl: gh.installUrl,
      device: gh.device,
    },
  ]
}

export const api = {
  health: () => call<Health>('GET', '/api/health'),

  async authStatus(): Promise<Result<Provider[]>> {
    const r = await call<unknown>('GET', '/api/auth/status')
    return r.ok ? { ok: true, status: r.status, data: normaliseAuth(r.data) } : r
  },
  /** Starts sign-in; the server opens the platform page in the default browser (`opened`). */
  authStart: (provider: string) => call<{ step?: string; url: string; opened?: boolean }>('POST', `/api/auth/${encodeURIComponent(provider)}/start`),
  authLogout: (provider: string) => call<unknown>('POST', `/api/auth/${encodeURIComponent(provider)}/logout`),
  authDevice: () => call<{ userCode?: string; verificationUri?: string }>('POST', '/api/auth/github/device'),

  repos: () => call<Repo[]>('GET', '/api/projects'),
  reposChunk(sort: string, desc: boolean, text: string, cursor: string, limit = 50) {
    const p = new URLSearchParams({ sort, dir: desc ? 'desc' : 'asc', limit: String(limit) })
    if (text) p.set('q', text)
    if (cursor) p.set('cursor', cursor)
    return call<RepoChunk>('GET', '/api/projects?' + p.toString())
  },

  issues(q: IssueQuery) {
    const p = new URLSearchParams()
    if (q.source) p.set('source', q.source)
    if (q.repo) p.set('project', String(q.repo))
    if (q.state && q.state !== 'all') p.set('state', q.state)
    if (q.label) p.set('label', q.label)
    if (q.q) p.set('q', q.q)
    if (q.unread) p.set('unread', '1')
    if (q.cursor) p.set('cursor', q.cursor)
    if (q.after) p.set('after', q.after)
    if (q.ids?.length) p.set('ids', q.ids.join(','))
    if (q.limit) p.set('limit', String(q.limit))
    const qs = p.toString()
    return call<IssueChunk>('GET', '/api/items' + (qs ? '?' + qs : ''))
  },
  comments: (id: number, cursor: string, limit = 50) =>
    call<CommentChunk>('GET', `/api/items/${id}/comments?limit=${limit}` + (cursor ? '&cursor=' + encodeURIComponent(cursor) : '')),
  issue: (id: number | string) => call<IssueDetail>('GET', `/api/items/${encodeURIComponent(String(id))}`),
  markRead: (id: number) => call<void>('POST', `/api/items/${id}/read`),
  reply: (id: number, body: string) => call<Comment>('POST', `/api/items/${id}/comments`, { body }),

  stats(repo?: number, weeks = 26) {
    const p = new URLSearchParams({ weeks: String(weeks) })
    if (repo) p.set('project', String(repo))
    return call<Stats>('GET', '/api/stats?' + p.toString())
  },

  syncStatus: () => call<SyncStatus>('GET', '/api/sync'),
  syncNow: () => call<void>('POST', '/api/sync'),

  settingsDoc: () => call<SettingsDoc>('GET', '/api/settings'),
  patchSettings: (revision: number, patch: SettingsPatch) => settingsCall('PATCH', '/api/settings', { revision, patch }),
  resetSettings: (revision: number, section: string) => settingsCall('POST', '/api/settings/reset', { revision, section }),
  testNotification: () => call<void>('POST', '/api/notifications/test'),
  folders: () => call<FolderRow[]>('GET', '/api/folders'),
  setFolder: (id: number, path: string) => call<FolderRow>('PUT', `/api/projects/${id}/path`, { path }),
  discoverFolders: () => call<{ suggestions: FolderSuggestion[]; visited: number; roots: string[] }>('POST', '/api/folders/discover'),
}

/** A settings change: 409 carries the current document, 400 the field and a reason code. */
export type SettingsResult =
  | { ok: true; data: SettingsDoc; status: number }
  | { ok: false; status: number; error: string; current?: SettingsDoc; field?: string; code?: string }

/** Localised name of a settings field path (sync.providers.github.activeMinutes → "Active projects"). */
function fieldLabel(field: string): string {
  const last = field.split('.').filter((p) => !/^\d+$/.test(p)).pop() ?? field
  for (const k of ['settings.fields.' + last, 'settings.sync.fields.' + last]) if (te(k)) return t(k)
  return field
}

async function settingsCall(method: string, url: string, body: unknown): Promise<SettingsResult> {
  let res: Response
  try {
    res = await fetch(url, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), credentials: 'same-origin' })
  } catch {
    return { ok: false, status: 0, error: t('common.notRunning') }
  }
  let j: Record<string, unknown> = {}
  try {
    j = (await res.json()) as Record<string, unknown>
  } catch {
    /* empty body */
  }
  if (res.ok) return { ok: true, status: res.status, data: j as unknown as SettingsDoc }
  const code = typeof j.code === 'string' ? j.code : undefined
  const field = typeof j.field === 'string' ? j.field : undefined
  let error = typeof j.error === 'string' ? j.error : res.statusText || `HTTP ${res.status}`
  if (code && te('settings.errors.' + code)) {
    const params = (j.params as Record<string, unknown> | undefined) ?? {}
    error = (field ? fieldLabel(field) + ': ' : '') + t('settings.errors.' + code, params)
  }
  return { ok: false, status: res.status, error, current: j.current as SettingsDoc | undefined, field, code }
}
