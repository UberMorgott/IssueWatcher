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

export interface IssuePage {
  total: number
  page: number
  perPage: number
  items: Issue[]
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
  commentsList: Comment[]
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
  page?: number
  perPage?: number
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
