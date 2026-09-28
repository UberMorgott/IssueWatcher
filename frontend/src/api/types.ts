// Shapes of the loopback JSON API (docs/ARCHITECTURE.md → Loopback JSON API).

export type ConnState = 'not_configured' | 'disconnected' | 'connecting' | 'connected' | 'error' | 'unavailable'

/** One platform connection as the UI sees it (normalised from /api/auth/status). */
export interface Provider {
  id: string
  name: string
  connected: boolean
  login?: string
  avatarUrl?: string
  setupNeeded?: boolean
  state: ConnState
  error?: string
  appUrl?: string
  installUrl?: string
  device?: DeviceState
}

export interface DeviceState {
  pending: boolean
  userCode?: string
  verificationUri?: string
  error?: string
}

export interface Repo {
  id: number
  name: string
  url: string
  platform: string
  open: number
  closed: number
  unread: number
  /** Mapped local working folder; "" = not mapped. */
  localPath?: string
  lastSync?: string
  /** Legacy alias of lastSync. */
  syncedAt?: string
}

export interface Issue {
  id: number
  repoId: number
  repo: string
  number: number
  title: string
  url: string
  author: string
  state: 'open' | 'closed' | string
  rawStatus: string
  labels: string[]
  comments: number
  unread: boolean
  createdAt: string
  updatedAt: string
  closedAt: string
}

/** A row of the virtual issues list: an issue or a loading placeholder. */
export type IssueRowData = Issue & { skeleton?: true }

/** Keyset chunk of GET /api/items (newest update first). */
export interface IssueChunk {
  items: Issue[]
  /** Cursor of the first row: `after` for a later head refresh. */
  headCursor: string
  /** Cursor of the last row: `cursor` for the next chunk. */
  nextCursor: string
  /** Older rows follow (cursor), or more newer rows than the limit (after). */
  more: boolean
  /** Rows matching the filter (first chunk and `after` only). */
  total?: number
}

export interface CommentChunk {
  items: Comment[]
  nextCursor: string
  more: boolean
}

export interface RepoChunk {
  items: Repo[]
  nextCursor: string
  more: boolean
  total: number
}

/** data.changed live event. */
export interface DataChange {
  reason: 'sync' | 'read' | 'reply'
  itemId?: number
  repo?: string
}

export interface Comment {
  id: string
  author: string
  body: string
  url: string
  createdAt: string
  updatedAt: string
}

export interface IssueDetail extends Issue {
  body: string
}

export interface Week {
  start: string
  opened: number
  closed: number
}

export interface Stats {
  open: number
  closed: number
  weekly: Week[]
  projects?: Repo[]
}

export interface SyncStatus {
  running: boolean
  signedIn: boolean
  lastSync: string
  lastError: string
  rateLimitedUntil: string
  interval: string
}

/** sync.status live event: one step of a sync cycle (internal/syncer Progress). */
export interface SyncProgress {
  state: 'started' | 'progress' | 'done' | 'error'
  repo?: string
  done: number
  total: number
  changed: number
  unread: number
  error?: string
}

export interface Health {
  version: string
  port: number
}

export interface IssueQuery {
  source?: string
  repo?: number
  state?: 'open' | 'closed' | 'all'
  label?: string
  q?: string
  unread?: boolean
  cursor?: string
  after?: string
  ids?: number[]
  limit?: number
}

/** Payload of item.new / comment.new / item.closed live events. */
export interface LiveItemEvent {
  id: number
  repo: string
  number: number
  title: string
  actor?: string
  body?: string
}

/** config.json (internal/config Settings). */
export interface Settings {
  schemaVersion: number
  revision: number
  general: { language: 'ru' | 'en'; startWithWindows: boolean; startMinimized: boolean }
  appearance: { mode: ThemeMode }
  notifications: {
    enabled: boolean
    newIssue: boolean
    newComment: boolean
    closed: boolean
    mutedProjects: string[]
    quiet: { enabled: boolean; from: string; to: string }
    group: boolean
    autoHideSeconds: number
    respectDnd: boolean
  }
  sync: { mode: SyncMode; activeDays: number; providers: Record<string, ProviderSync> }
  projects: { roots: string[]; exclude: string[]; scanDepth: number }
}

export type ThemeMode = 'dark' | 'light' | 'system'
export type SyncMode = 'balanced' | 'fast' | 'custom'

export interface ProviderSync {
  activeMinutes: number
  idleMinutes: number
  reconcileMinutes: number
  hourlyBudget: number
  concurrency: number
}

/** GET /api/settings, PATCH result and the settings.changed live event. */
export interface SettingsDoc {
  revision: number
  settings: Settings
  info: { dataDir: string; configFile: string; version: string; exe: string; syncPresets: Record<string, ProviderSync> }
}

type DeepPartial<T> = T extends unknown[] ? T : T extends object ? { [K in keyof T]?: DeepPartial<T[K]> } : T

/** A JSON merge patch of the settings (PATCH /api/settings). */
export type SettingsPatch = DeepPartial<Omit<Settings, 'schemaVersion' | 'revision'>>

/** GET /api/folders row. */
export interface FolderRow {
  projectId: number
  name: string
  url: string
  platform: string
  localPath: string
  status: 'none' | 'ok' | 'missing' | 'notGit' | 'mismatch'
}

export interface FolderSuggestion {
  projectId: number
  name: string
  path: string
  remote: string
}
