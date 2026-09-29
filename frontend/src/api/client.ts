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
  UpdateStatus,
  DetectedCLI,
  Job,
  JobChunk,
  JobAttempt,
  JobFlow,
  JobQuery,
  RepoLabel,
  JobStep,
  QueuedJob,
  AutomationChunk,
} from './types'
import { t, te } from '../i18n'

/**
 * Result of an API call. Never throws: pages render empty/error states from
 * `ok` + `status` instead (404 = endpoint not built yet, 401 = session gone).
 */
export type Result<T> = { ok: true; data: T; status: number } | { ok: false; status: number; error: string }

export type JobAction = 'cancel' | 'retry' | 'dismiss' | 'pr' | 'push'

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
  updateStatus: () => call<UpdateStatus>('GET', '/api/update'),
  updateCheck: () => call<UpdateStatus>('POST', '/api/update/check'),
  updateInstall: () => call<void>('POST', '/api/update/install'),
  folders: () => call<FolderRow[]>('GET', '/api/folders'),
  setFolder: (id: number, path: string) => call<FolderRow>('PUT', `/api/projects/${id}/path`, { path }),
  discoverFolders: () => call<{ suggestions: FolderSuggestion[]; visited: number; roots: string[] }>('POST', '/api/folders/discover'),

  // --- agent jobs
  async jobs(q: JobQuery): Promise<Result<JobChunk>> {
    const p = new URLSearchParams()
    if (q.state) p.set('state', q.state)
    if (q.flow) p.set('flow', q.flow)
    if (q.origin) p.set('origin', q.origin)
    if (q.project) p.set('project', String(q.project))
    if (q.item) p.set('item', String(q.item))
    if (q.cursor) p.set('cursor', q.cursor)
    if (q.limit) p.set('limit', String(q.limit))
    const qs = p.toString()
    const r = await call<JobChunk>('GET', '/api/jobs' + (qs ? '?' + qs : ''))
    return r.ok ? { ...r, data: { ...r.data, items: (r.data.items ?? []).map(normJob) } } : r
  },
  async createJobs(itemIds: number[], flow: JobFlow, profileId?: string): Promise<Result<{ jobs: QueuedJob[] }>> {
    const r = await call<{ jobs: QueuedJob[] }>('POST', '/api/jobs', { itemIds, flow, profileId: profileId || undefined })
    return r.ok ? { ...r, data: { jobs: (r.data.jobs ?? []).map((q) => (q.job ? { ...q, job: normJob(q.job) } : q)) } } : r
  },
  job: (id: number | string) => jobCall('GET', `/api/jobs/${encodeURIComponent(String(id))}`),
  jobLog: (id: number, attempt?: number) => call<{ attempt: number; steps: JobStep[] }>('GET', `/api/jobs/${id}/log` + (attempt ? `?attempt=${attempt}` : '')),
  jobDiff: (id: number, attempt?: number) => textCall(`/api/jobs/${id}/diff` + (attempt ? `?attempt=${attempt}` : '')),
  /**
   * cancel | retry | dismiss (Отклонить) | pr (Создать PR, worktree jobs) | push (direct fix jobs);
   * pr/push: 502 = publish failed, job back to needs_review with result.publishError.
   */
  jobAction: (id: number, action: JobAction) => jobCall('POST', `/api/jobs/${id}/${action}`),
  jobReply: (id: number, body: string) => jobCall('POST', `/api/jobs/${id}/reply`, { body }),
  /** Добавить метки: add labels to a label job's issue (checked against the repo, add only) → done. */
  jobLabels: (id: number, labels: string[]) => jobCall('POST', `/api/jobs/${id}/labels`, { labels }),
  async jobAttempts(id: number): Promise<Result<JobAttempt[]>> {
    const r = await call<{ attempts: JobAttempt[] }>('GET', `/api/jobs/${id}/attempts`)
    return r.ok ? { ...r, data: (r.data.attempts ?? []).map((a) => ({ ...a, result: a.result && typeof a.result === 'object' ? a.result : {} })) } : r
  },
  projectLabels: (id: number) => call<RepoLabel[]>('GET', `/api/projects/${id}/labels`),
  detectAgents: () => call<DetectedCLI[]>('GET', '/api/agents/detect'),
  automationLog: (cursor = '', limit = 50) =>
    call<AutomationChunk>('GET', `/api/automation/log?limit=${limit}${cursor ? '&cursor=' + encodeURIComponent(cursor) : ''}`),
}

/** Job rows always carry a result object (the column may hold null). */
export function normJob(j: Job): Job {
  return j.result && typeof j.result === 'object' ? j : { ...j, result: {} }
}

async function jobCall(method: string, url: string, body?: unknown): Promise<Result<Job>> {
  const r = await call<Job>(method, url, body)
  return r.ok ? { ...r, data: normJob(r.data) } : r
}

/** GET returning text/plain (job diff). */
async function textCall(url: string): Promise<Result<string>> {
  let res: Response
  try {
    res = await fetch(url, { credentials: 'same-origin' })
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
  return { ok: true, status: res.status, data: await res.text() }
}

/** A settings change: 409 carries the current document, 400 the field and a reason code. */
export type SettingsResult =
  | { ok: true; data: SettingsDoc; status: number }
  | { ok: false; status: number; error: string; current?: SettingsDoc; field?: string; code?: string }

/** Localised name of a settings field path (sync.providers.github.activeMinutes → "Active projects"). */
function fieldLabel(field: string): string {
  const parts = field.split('.').filter((p) => !/^\d+$/.test(p))
  const last = parts.pop() ?? field
  // Section-specific name first (agents.projects.<name>.mode → fields.agents_mode).
  for (const k of ['settings.fields.' + parts[0] + '_' + last, 'settings.fields.' + last, 'settings.sync.fields.' + last]) if (te(k)) return t(k)
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
