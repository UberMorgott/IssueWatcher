import type {
  AppSettings,
  Comment,
  Health,
  IssueDetail,
  IssuePage,
  IssueQuery,
  Provider,
  Repo,
  Stats,
  SyncStatus,
} from './types'
import { t } from '../i18n'

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

  issues(q: IssueQuery) {
    const p = new URLSearchParams()
    if (q.source) p.set('source', q.source)
    if (q.repo) p.set('project', String(q.repo))
    if (q.state && q.state !== 'all') p.set('state', q.state)
    if (q.label) p.set('label', q.label)
    if (q.q) p.set('q', q.q)
    if (q.unread) p.set('unread', '1')
    if (q.page) p.set('page', String(q.page))
    if (q.perPage) p.set('per', String(q.perPage))
    const qs = p.toString()
    return call<IssuePage>('GET', '/api/items' + (qs ? '?' + qs : ''))
  },
  issue: (id: number | string) => call<IssueDetail>('GET', `/api/items/${encodeURIComponent(String(id))}`),
  markRead: (id: number) => call<void>('POST', `/api/items/${id}/read`),
  reply: (id: number, body: string) => call<Comment>('POST', `/api/items/${id}/comments`, { body }),

  stats(repo?: number, weeks = 26) {
    const p = new URLSearchParams({ weeks: String(weeks) })
    if (repo) p.set('project', String(repo))
    return call<Stats>('GET', '/api/stats?' + p.toString())
  },

  settings: () => call<AppSettings>('GET', '/api/settings'),
  saveSettings: (patch: Partial<Pick<AppSettings, 'startWithWindows' | 'startMinimized'>>) => call<AppSettings>('PUT', '/api/settings', patch),

  syncStatus: () => call<SyncStatus>('GET', '/api/sync'),
  syncNow: () => call<void>('POST', '/api/sync'),
}
