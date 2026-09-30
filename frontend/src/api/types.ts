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

/**
 * Where a fix of a project's items runs (server-side, follows project_links):
 * the project's own folder, or a linked mod page's code project's.
 */
export interface FixTarget {
  /** The project whose folder is used (a linked mod page's code project). */
  fixProjectId: number
  /** That project's mapped folder; "" = none. */
  fixFolder: string
  /** fixFolder exists: a fix can run (in place when it is not a clone, see fixGit). */
  fixable: boolean
  /** fixFolder is a git clone of that project: the fix commits, push / PR available. */
  fixGit?: boolean
  /** A mod page not linked to a code project yet. */
  needsLink?: boolean
}

export interface Repo extends FixTarget {
  id: number
  name: string
  url: string
  platform: string
  /** Settings key platform:external_id (agents.projects keys, rule projects). */
  key: string
  /** A mod page's linked code project id; a code project's mod page ids (project_links). */
  linkedTo?: number
  links?: number[]
  /** Code projects an unlinked mod page may belong to (name match): one click links it. */
  suggest?: number[]
  open: number
  closed: number
  unread: number
  /** Open / unread items of kind comment (the rest of open / unread are issues and bug reports). */
  openComments?: number
  unreadComments?: number
  /** Mapped local working folder; "" = not mapped. */
  localPath?: string
  lastSync?: string
  /** Legacy alias of lastSync. */
  syncedAt?: string
  /**
   * Grouped list only (GET /api/projects?group=1): the row's own project first,
   * then its linked mod pages, each with its own counts. The row's open/closed/
   * unread are the sums; lastSync is the stalest member's.
   */
  integrations?: Integration[]
}

/** One channel of a grouped project row; Issues filter: ?repo=<row id>&source=<platform>. */
export interface Integration {
  id: number
  name: string
  url: string
  platform: string
  open: number
  closed: number
  unread: number
  openComments?: number
  unreadComments?: number
  lastSync: string
}

/** POST /api/projects/{id}/sync: platforms started, and those no connected account serves. */
export interface ProjectSyncResult {
  started: string[]
  missing: string[]
}

export interface Issue extends FixTarget {
  id: number
  repoId: number
  repo: string
  number: number
  /** issue (GitHub) | comment (a mod page's comment thread) | bug (a mod bug report). */
  kind: ItemKind
  /** The project's platform: github | nexus | curseforge | steam. */
  platform: string
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
  /** The item's newest agent job. */
  job?: JobBadge
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
  /** First chunk: open / closed / unread rows for every filter but state and unread (header counters). */
  counts?: { open: number; closed: number; unread: number }
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
  reason: 'sync' | 'read' | 'reply' | 'job' | 'folder' | 'labels'
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
  /** The last cycle failed with a relogin error (session expired). */
  relogin?: boolean
  rateLimitedUntil: string
  interval: string
  /** Tiered sync (internal/syncer Status). */
  lastCheck?: string
  mode?: string
  activeEvery?: string
  idleEvery?: string
  reconcileEvery?: string
  nextReconcile?: string
  projects?: number
  activeProjects?: number
  budget?: number
  budgetUsed?: number
  checks?: number
  notModified?: number
  rate?: { limit: number; remaining: number; reset: string }
  /** Every connected account's own status (internal/syncer GroupStatus); top-level fields = the primary (GitHub) one. */
  sources?: (Omit<SyncStatus, 'sources'> & { platform: string; account: string })[]
}

/** sync.status live event: one step of a sync cycle (internal/syncer Progress). */
export interface SyncProgress {
  state: 'started' | 'progress' | 'done' | 'error'
  /** platform[:account] of the reporting source. */
  source?: string
  repo?: string
  done: number
  total: number
  changed: number
  unread: number
  error?: string
  /** A cycle nobody asked for (scheduled/resumed reconcile, change checks): shown as a quiet hint, never blocks. */
  background?: boolean
}

export interface Health {
  version: string
  port: number
}

export interface IssueQuery {
  source?: string
  kind?: ItemKind | ''
  /** Several kinds (Issues page: issue + bug); ignored when kind is set. */
  kinds?: ItemKind[]
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
  appearance: Appearance
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
  updates: { channel: UpdateChannel; autoCheck: boolean; intervalHours: number }
  agents: Agents
  providers: Providers
}

/** settings.providers: the mod platforms (internal/config Providers); apply live. */
export interface Providers {
  nexus: ModPlatform
  curseforge: ModPlatform
  factorio: NativePlatform
}

/** A platform with only the built-in engine (Factorio). */
export interface NativePlatform {
  enabled: boolean
  /** Portal username whose mods are listed; empty = the signed-in one. */
  author?: string
}

export interface ModPlatform {
  enabled: boolean
  mcp: { command: string; args: string[] }
  /** 'mcp' = the owner's MCP server, 'native' = built-in (installed browser + plain HTTP). */
  engine?: 'mcp' | 'native'
  /** Nexus: the exact uploader account name or member id (required); CurseForge: CFWidget author override. */
  author?: string
}

export type AgentCLI = 'claude' | 'codex'

/** One configured agent (internal/config AgentProfile). */
export interface AgentProfile {
  id: string
  name: string
  cli: AgentCLI
  /** "" = found on PATH. */
  path: string
  /** "" = the CLI's default. */
  model: string
  args: string[]
  timeoutMinutes: number
  maxParallel: number
  /** 0 = no cap (claude only). */
  maxBudgetUsd: number
}

/** How a fix job runs: in the mapped folder itself, or in a worktree published as a PR. */
export type RunMode = 'direct' | 'worktree-pr'

export interface ProjectAgent {
  prompt: string
  verify: string
  noAegis: boolean
  /** Overrides Agents.jobMcp; missing = inherit. */
  jobMcp?: boolean
  /** Mod pages: overrides Agents.modPush; missing = inherit. */
  modPush?: boolean
  /** Missing or "" = 'direct'. */
  mode?: RunMode | ''
  /** Overrides Agents.triageTopN; missing = inherit. */
  triageTopN?: number
  /** Appended to the triage task for this project (its criticality criteria). */
  triagePrompt?: string
  /** Overrides of the global automation defaults; a missing field = inherit. */
  automation?: ProjectAutomation
}

export type RuleEvent = 'new_issue' | 'new_comment' | 'new_item'
/** Item kinds: GitHub issues, mod-page comment threads, mod bug reports. */
export type ItemKind = 'issue' | 'comment' | 'bug'
export type RuleFlow = 'fix' | 'reply' | 'label'

/** One automation rule (internal/config Rule); the first matching enabled rule wins. */
export interface AutomationRule {
  id: string
  enabled: boolean
  /** Project key platform:external_id (e.g. github:owner/repo). */
  project: string
  event: RuleEvent
  /** Empty = any item. */
  labelsAny: string[]
  /** Item kinds the rule fires on; absent = issue only. */
  kinds?: ItemKind[]
  flow: RuleFlow
  /** "" = the flow's role. */
  profileId: string
  /** 0 = only the global/project cap. */
  maxPerDay: number
}

/** settings.agents.automation: global defaults + rules (internal/config Automation). */
export interface Automation {
  enabled: boolean
  maxPerDay: number
  maxAttempts: number
  allowAutoFix: boolean
  autoApplyLabels: boolean
  rules: AutomationRule[]
}

/** Per-project automation overrides (internal/config ProjectAutomation). */
export interface ProjectAutomation {
  enabled?: boolean
  maxPerDay?: number
  maxAttempts?: number
  allowAutoFix?: boolean
  autoApplyLabels?: boolean
}

/** One rule decision (GET /api/automation/log, internal/store AutomationEntry). */
export interface AutomationEntry {
  id: number
  at: string
  itemId: number
  event: RuleEvent
  ruleId: string
  flow: RuleFlow
  decision: 'queued' | 'skipped'
  /** Skipped: no_profile | unavailable | auto_fix_off | no_folder | exists | max_attempts | total_cap | day_cap | rule_cap. */
  reason: string
  jobId: number | null
  repo: string
  number: number
  title: string
}

export interface AutomationChunk {
  items: AutomationEntry[]
  nextCursor: string
  more: boolean
}

/** settings.agents (internal/config Agents). */
export interface Agents {
  maxParallel: number
  profiles: AgentProfile[]
  roles: { coder: string; responder: string; verifier: string }
  /** fixDirect: the direct fix flow; "" = built-in default (like fix). */
  prompts: { system: string; fix: string; fixDirect: string; reply: string; review: string; label: string; triage: string }
  /** Keyed by project key platform:external_id (e.g. github:owner/repo). */
  projects: Record<string, ProjectAgent>
  automation: Automation
  /** Each job's agent run gets IssueWatcher's MCP server for its issue (per run only). */
  jobMcp: boolean
  /** Allow Push / PR of mod-page fixes (they run in the linked code project); off = commit stays local. */
  modPush: boolean
  /** Project triage: how many ranked picks get a fix job (1–20). */
  triageTopN: number
}

// --- agent jobs (internal/store Job, internal/runner Result) -----------------

export type JobState = 'queued' | 'running' | 'needs_review' | 'done' | 'failed' | 'cancelled'
/** triage: a project job (itemId 0) that ranks the open issues and queues fix jobs. */
export type JobFlow = 'fix' | 'reply' | 'label' | 'triage'
export type JobOrigin = 'manual' | 'rule'
export type JobPhase = '' | 'prepare' | 'agent' | 'check' | 'verify' | 'review' | 'publish'

/** Issue row badge: the item's newest job. */
export interface JobBadge {
  id: number
  flow: JobFlow
  state: JobState
  startedAt?: string
  /** Direct fix outcome (result.local.outcome); '' otherwise. */
  outcome?: LocalOutcome | ''
}

export interface AgentResult {
  profile: string
  cli: string
  model?: string
  status?: 'fixed' | 'partial' | 'cannot_fix' | 'needs_info' | 'not_reproduced' | 'failed'
  verdict?: 'ok' | 'concerns'
  summary?: string
  notes?: string
  /** Commits the agent reports (direct mode). */
  commits?: string[]
  /** The agent's own verify note. */
  verify?: string
  reply?: string
  /** Label flow: the agent's picks as it wrote them. */
  labels?: string[]
  final?: string
  costUsd?: number
  turns?: number
  tokens?: number
  exitCode: number
  durationMs: number
  error?: string
}

export interface FileChange {
  path: string
  /** A, M, D, R, … */
  status: string
  /** -1 = binary. */
  added: number
  deleted: number
}

export interface DiffSummary {
  files: FileChange[]
  added: number
  deleted: number
  bytes: number
  truncated: boolean
  commits: number
}

export interface VerifyResult {
  command: string
  ok: boolean
  exitCode: number
  output: string
  durationMs: number
  timedOut?: boolean
}

export type JobErrorCode = 'no_folder' | 'no_profile' | 'no_cli' | 'timeout' | 'agent_failed' | 'git' | 'interrupted'

export interface LocalCommit {
  sha: string
  subject: string
  /** The message carries "Fixes #N" for this issue. */
  fixes: boolean
}

export type LocalOutcome =
  | 'fixed_local'
  | 'pushed'
  | 'closed'
  | 'not_reproduced'
  | 'needs_info'
  | 'no_commit'
  | 'changed_folder'
  | 'no_changes'
  | 'failed'

/** Git facts of a direct fix job, checked after the agent (phase check). */
export interface LocalResult {
  dir: string
  branch: string
  startSha: string
  headSha: string
  commits: LocalCommit[]
  fixesRef: boolean
  dirtyBefore?: string[]
  dirtyAfter?: string[]
  outcome: LocalOutcome
  pushed: boolean
  pushedAt?: string
  closed: boolean
  /** Folder mode: files changed in place, "A|M|D path" (a file snapshot, not git). */
  changed?: string[]
}

export interface JobResult {
  /** Absent = an older worktree-pr job; 'folder' = ran in a folder that is not the project's git clone. */
  mode?: RunMode | 'folder'
  local?: LocalResult
  errorCode?: JobErrorCode | string
  agent?: AgentResult
  diff?: DiffSummary
  verify?: VerifyResult
  review?: AgentResult
  draft?: string
  pr?: { number: number; url: string; commit: string }
  comment?: { id: string; author: string; body: string; url: string; createdAt: string }
  publishError?: string
  cleanupError?: string
  baseBranch?: string
  /** Label flow: picks that exist in the repository (canonical names). */
  labels?: string[]
  /** Label flow: picks the repository does not have (dropped). */
  droppedLabels?: string[]
  /** Label flow: names actually added to the issue. */
  appliedLabels?: string[]
  /** Triage flow: the checked ranking and the fix jobs it queued. */
  triage?: TriageResult
}

export type TriageSeverity = 'critical' | 'high' | 'medium' | 'low' | ''

/** One ranked issue of a triage (runner TriagePick). */
export interface TriagePick {
  number: number
  severity: TriageSeverity
  reason: string
  itemId?: number
  title?: string
  /** queued (jobId = the new fix job) | exists (jobId = the unfinished one) | '' (below top N) | an error. */
  queue?: string
  jobId?: number
}

export interface TriageResult {
  open: number
  more?: boolean
  topN: number
  picks: TriagePick[]
  dropped?: TriagePick[]
  summary?: string
}

/** GET /api/projects/{id}/labels row (provider.Label). */
/** GET /api/projects/{id}/labels: the labels stored in SQLite; a stale list refreshes in the background (data.changed reason 'labels'). */
export interface ProjectLabels {
  labels: RepoLabel[]
  /** '' = never fetched. */
  fetchedAt: string
  refreshing: boolean
  error?: string
}

export interface RepoLabel {
  name: string
  /** Hex without '#', '' when unknown. */
  color: string
  description: string
}

/** GET /api/jobs/{id}/attempts row: an earlier attempt snapshot or the current one. */
export interface JobAttempt {
  attempt: number
  state: JobState
  error: string
  errorCode: string
  result: JobResult
  startedAt: string
  finishedAt: string
}

export interface Job {
  id: number
  itemId: number
  projectId: number
  flow: JobFlow
  state: JobState
  /** Queued by a click or by an automation rule (ruleId). */
  origin: JobOrigin
  ruleId: string
  profileId: string
  attempt: number
  phase: JobPhase
  branch: string
  worktree: string
  /** The folder fixes run in: the project's mapped folder, or a mod page's linked code project's. */
  localPath: string
  /** The item is on a mod page; codeProject = its linked code project (Push/PR need agents.modPush). */
  mod?: boolean
  codeProject?: string
  /** Settings key platform:external_id. */
  projectKey?: string
  baseSha: string
  error: string
  result: JobResult
  createdAt: string
  startedAt: string
  finishedAt: string
  updatedAt: string
  repo: string
  number: number
  title: string
  itemUrl: string
}

/** A row of the virtual jobs list: a job or a loading placeholder. */
export type JobRowData = Job & { skeleton?: true }

export interface JobChunk {
  items: Job[]
  nextCursor: string
  more: boolean
  total?: number
}

export interface JobQuery {
  /** A job state or "active" (queued/running/needs_review). */
  state?: string
  flow?: string
  origin?: JobOrigin
  project?: number
  item?: number
  cursor?: string
  limit?: number
}

export type StepKind = 'info' | 'text' | 'tool' | 'output' | 'error' | 'stderr' | 'result'

export interface JobStep {
  t: string
  kind: StepKind
  text: string
}

/** job.log live event: new steps of the running attempt. */
export interface JobLogEvent {
  id: number
  attempt: number
  steps: JobStep[]
}

/** POST /api/jobs result per item. */
export interface QueuedJob {
  itemId: number
  job?: Job
  /** "exists" (job = the unfinished one), "not_found" or a message. */
  error?: string
}

/** GET /api/agents/detect row. */
export interface DetectedCLI {
  cli: AgentCLI
  /** "" = not on PATH. */
  path: string
  version: string
}

export type UpdateChannel = 'stable' | 'preview'

/** GET /api/update and the update.status live event (internal/selfupdate Status). */
export interface UpdateStatus {
  current: string
  channel: UpdateChannel
  /** Not a release build: checks only, never installs. */
  devBuild: boolean
  state: 'idle' | 'checking' | 'downloading' | 'installing' | 'restarting'
  error?: string
  done?: number
  total?: number
  available?: { version: string; name?: string; notes?: string; publishedAt: string; url?: string; prerelease: boolean }
  updateAvailable: boolean
  checkedAt?: string
  lastResult?: { ok: boolean; from?: string; to?: string; error?: string; at: string }
}

export type ThemeMode = 'dark' | 'light' | 'system'

/** Four base colours of one theme mode (#rrggbb); the UI derives the rest. */
export interface Colors {
  accent: string
  background: string
  surface: string
  text: string
}

/** A colour preset: a dark and a light variant (GET /api/settings info.palettes). */
export interface Palette {
  id: string
  dark: Colors
  light: Colors
}

export interface Appearance {
  mode: ThemeMode
  paletteId: string
  /** Per-mode overrides of the palette; empty = palette colour. */
  custom: { dark: Colors; light: Colors }
  fontFamily: 'inter' | 'segoe' | 'system' | 'mono'
  fontScale: number
  density: 'compact' | 'comfortable' | 'spacious'
}
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
  info: { dataDir: string; configFile: string; version: string; exe: string; syncPresets: Record<string, ProviderSync>; palettes: Palette[] }
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
  /** Settings key platform:external_id. */
  key: string
  localPath: string
  status: 'none' | 'ok' | 'missing' | 'notGit' | 'mismatch'
}

export interface FolderSuggestion {
  projectId: number
  name: string
  path: string
  remote: string
}

/** Provider capabilities (internal/provider Capabilities). */
export interface Capabilities {
  listProjects: boolean
  syncItems: boolean
  listComments: boolean
  reply: boolean
  setLabels: boolean
  setStatus: boolean
  createPR: boolean
  auth: string
  kinds: ItemKind[]
  /** false: a reply is a new top-level comment (Steam «@author …»). */
  replyThreaded: boolean
}

export type PlatformState = 'disabled' | 'unknown' | 'connected' | 'signed_out' | 'relogin' | 'unavailable' | 'error'

/** none = public reads only; qr = Steam QR sign-in; stored = origin unknown. */
export type PlatformSession = 'none' | 'browser' | 'window' | 'manual' | 'qr' | 'stored' | 'profile'

/** GET /api/platforms row (internal/api PlatformStatus). */
export interface PlatformStatus {
  id: string
  name: string
  enabled: boolean
  state: PlatformState
  account?: string
  /** Display name when account is an id (Steam persona). */
  accountName?: string
  /** Where the web session came from; missing = not known yet. */
  session?: PlatformSession
  /** session 'browser': which browser. */
  browser?: string
  error?: string
  /** The platform's MCP server child is running. */
  running: boolean
  projects: number
  lastSync?: string
  checkedAt?: string
  capabilities: Capabilities
}

/** GET/PUT /api/providers/steam (secrets are write-only). */
export interface SteamStatus {
  configured: boolean
  steamId: string
  /** Public profile name, when known. */
  persona?: string
  appId: number
  hasApiKey: boolean
  hasCookies: boolean
  session: 'none' | 'stored' | 'verified' | 'expired'
  signedIn: boolean // signed in by QR: the session renews itself
  checkedAt?: string
}

// «Подключить»: POST/GET/DELETE /api/platforms/{id}/login.
export type LoginState = 'idle' | 'qr' | 'scanned' | 'window' | 'connected' | 'expired' | 'failed'
export interface LoginStatus {
  platform: string
  state: LoginState
  challengeUrl?: string // Steam: the QR code's content
  account?: string
  error?: string
  /** state window: the login page opened in the default browser, or the server's own window. */
  via?: 'default-browser' | 'window'
  /** via default-browser: which browser. */
  browser?: string
}

export interface SteamUpdate {
  steamId?: string
  appId?: number
  apiKey?: string
  steamLoginSecure?: string
  sessionid?: string
}

/** GET/PUT /api/projects/{id}/links. */
export interface ProjectLinks {
  linkedTo?: number
  links?: number[]
}