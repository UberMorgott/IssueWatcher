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
  /** Open / unread items of kind comment (the rest of open / unread are issues and bug reports); open = waiting for an answer. */
  openComments?: number
  /** Comment threads answered or resolved (flat project list only). */
  closedComments?: number
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
  /** Written by the synced account (the owner): shown as «вы». */
  mine: boolean
  /** Comment threads: open = waiting for the owner's answer, closed = answered or resolved. */
  state: 'open' | 'closed' | string
  /** Comment threads: the local «Решено» flag (never sent to the platform). */
  resolved: boolean
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
  /** First chunk: open / closed / unread / resolved rows for every filter but state and unread (header counters). */
  counts?: IssueCounts
}

export interface IssueCounts {
  open: number
  closed: number
  unread: number
  /** Comment threads marked «Решено» (part of closed). */
  resolved: number
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
  reason: 'sync' | 'read' | 'unread' | 'resolve' | 'reply' | 'job' | 'folder' | 'labels'
  /** 0 / absent: several or unknown items. */
  itemId?: number
  repo?: string
}

export interface Comment {
  id: string
  author: string
  /** Written by the synced account (the owner). */
  mine: boolean
  body: string
  url: string
  createdAt: string
  updatedAt: string
}

/** What an item is when it is not a bug: the fix agent's outcome or the autopilot triage verdict. */
export type NonBugKind = 'feedback' | 'question' | 'suggestion'

export interface IssueDetail extends Issue {
  body: string
  /** Not a bug (store ItemNonBug): the item page leads with «Черновик ответа агентом». */
  nonBug?: NonBugKind
}

/** Weekly bucket: issues and bug reports opened / closed; new comment threads apart. */
export interface Week {
  start: string
  opened: number
  closed: number
  comments: number
}

/** open / closed: issues and bug reports only; comment threads counted apart. */
export interface Stats {
  open: number
  closed: number
  /** Comment threads waiting for an answer. */
  openComments: number
  /** All comment threads (own-only threads excluded). */
  comments: number
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
  /** resolved: comment threads marked «Решено». */
  state?: 'open' | 'closed' | 'resolved' | 'all'
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
  /** Item kind (issue | comment | bug); absent on old servers. */
  kind?: ItemKind
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

/** Factorio: portal settings. */
export interface NativePlatform {
  enabled: boolean
  /** Portal username whose mods are listed; empty = the signed-in one. */
  author?: string
}

export interface ModPlatform {
  enabled: boolean
  /** Nexus: the exact uploader account name or member id (default: the signed-in member); CurseForge: CFWidget author override. */
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
  status?: 'fixed' | 'partial' | 'cannot_fix' | 'needs_info' | 'not_reproduced' | 'feedback' | 'question' | 'suggestion' | 'failed'
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

export type JobErrorCode = 'no_folder' | 'dirty_folder' |'no_profile' | 'no_cli' | 'timeout' | 'agent_failed' | 'agent_auth' | 'git' | 'interrupted' | 'mod_item' | 'reply_too_long'

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
  | 'feedback'
  | 'question'
  | 'suggestion'
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
  /** queued (jobId = the new fix job) | exists (jobId = the unfinished one) | closed | no_folder | failed | '' (below top N); older jobs: a text. */
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
  /** "exists" (job = the unfinished one), "not_found", "no_folder", "dirty_folder" or a message. */
  error?: string
  /** dirty_folder: `git status --porcelain` lines of the folder the direct fix would commit in (no job). */
  dirty?: string[]
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
  /** Longest reply the platform accepts, in characters (Steam 999); absent = DEFAULT_MAX_REPLY. */
  maxReply?: number
  /** The provider publishes new mod file versions from the app (Nexus with the API key store). */
  publish?: boolean
  /** The provider edits mod pages (name, summary, description, version) from the app. */
  editPage?: boolean
}

export type PlatformState = 'disabled' | 'unknown' | 'connected' | 'signed_out' | 'relogin' | 'error'

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
  /** Server error code (internal/api Code*): the card shows its own text for it, error goes to a tooltip. */
  errorCode?: string
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
  /** internal/api Code* of error. */
  errorCode?: string
  /** state window: the sign-in window of the installed browser. */
  via?: 'window'
  /** state window: which browser. */
  browser?: string
}

export interface SteamUpdate {
  steamId?: string
  appId?: number
  apiKey?: string
  steamLoginSecure?: string
  sessionid?: string
}

/** GET/PUT /api/providers/nexus: the Nexus API key for publishing (write-only; never returned). */
export interface NexusKeyStatus {
  hasApiKey: boolean
  /** Account name v1 users/validate returned for the key. */
  user?: string
  userId?: number
  checkedAt?: string
}

/** GET/PUT /api/providers/curseforge/upload: the CurseForge upload API token (write-only; never returned). */
export interface CurseForgeUploadStatus {
  hasToken: boolean
  checkedAt?: string
}

/** idle | starting | updating | logging_in | need_code | confirm_mobile | ok | failed | cancelled. */
export type SteamLoginStep =
  | 'idle'
  | 'starting'
  | 'updating'
  | 'logging_in'
  | 'need_code'
  | 'confirm_mobile'
  | 'ok'
  | 'failed'
  | 'cancelled'
  | ''

export interface SteamLoginState {
  state: SteamLoginStep
  /** a failure's code (platforms.steamUpload.loginErrors.*); error is the English detail. */
  code?: string
  error?: string
  at?: string
}

/** GET /api/providers/steam/upload: steamcmd and its cached sign-in for Workshop uploads (no password stored). */
export interface SteamUploadStatus {
  /** the app's own steamcmd.exe in data\\tools\\steamcmd (absent = not set up yet). */
  steamcmd?: string
  tool: ToolStatus
  user?: string
  loggedIn: boolean
  expired?: boolean
  loggedInAt?: string
  checkedAt?: string
  login: SteamLoginState
}

/** A helper the app provisions next to its binary (data\\tools): GET /api/tools. */
export interface ToolStatus {
  name: 'steamcmd' | 'steamworks' | string
  state: 'missing' | 'working' | 'ready' | 'error'
  path?: string
  /** where it came from: the download URL or the game folder it was copied from */
  source?: string
  error?: string
  at?: string
}

// --- Publishing a mod file version (Nexus, Phase 7): internal/provider/publish.go, internal/api/publish.go.

/** One version of a mod file. */
export interface PublishVersion {
  id: string
  name: string
  version: string
  /** main | optional | miscellaneous | archived | … */
  category: string
  uploadedAt: string
  primary?: boolean
}

export interface PublishFile {
  id: string
  name: string
  active: boolean
  versionsCount: number
  archivedCount: number
  lastUploadedAt?: string
  versions: PublishVersion[]
}

/** GET /api/projects/{id}/publish/targets. */
export interface PublishTargets {
  modUid: string
  modName: string
  filesUrl: string
  files: PublishFile[]
}

export type PublishCategory = 'main' | 'optional' | 'miscellaneous'

/** POST /api/projects/{id}/publish: fileId (new version) or newFile; path (upload) or uploadId (retry, no re-upload). */
export interface PublishRequest {
  fileId?: string
  newFile?: boolean
  path?: string
  uploadId?: string
  name: string
  version: string
  description?: string
  category?: PublishCategory
  archivePrevious?: boolean
  previousVersionId?: string
  updateModVersion?: boolean
  primaryModManagerDownload?: boolean
  allowModManagerDownload?: boolean
  showRequirementsPopUp?: boolean
  changelog?: string
  dryRun?: boolean
}

/** One planned request of a dry run. */
export interface PublishStep {
  method: string
  url: string
  body?: unknown
  note?: string
}

export interface PublishResult {
  dryRun?: boolean
  plan?: PublishStep[]
  uploadId?: string
  md5?: string
  size?: number
  fileId?: string
  fileName?: string
  versionId?: string
  filesUrl?: string
  /** "" none asked, "added", or "failed: <reason>" (the version is published regardless). */
  changelog?: string
}

export type PublishStage = 'resolve' | 'hash' | 'upload' | 'wait' | 'publish' | 'changelog'
export type PublishState = 'running' | 'done' | 'failed' | 'cancelled'

/** A publish run (202 of POST …/publish, GET /api/publish/{id}, SSE publish.progress). */
export interface PublishTask {
  id: string
  projectId: number
  state: PublishState
  stage?: PublishStage
  sent?: number
  total?: number
  version: string
  /** Set once the archive is uploaded: a failed publish retries with it (no re-upload). */
  uploadId?: string
  result?: PublishResult
  error?: string
  errorCode?: 'no_api_key' | 'bad_api_key' | 'upload_failed' | 'publish_failed' | 'cancelled'
  startedAt: string
  finishedAt?: string
}

/** GET /api/projects/{id}/page: the mod editor's General tab (internal/provider/modpage.go). */
export interface ModPage {
  name: string
  /** Plain text, line breaks as \n. */
  summary: string
  /** BBCode. */
  description: string
  version: string
  category?: string
  author?: string
  tags: string[]
  url: string
  maxSummary?: number
}

/** PUT /api/projects/{id}/page; an omitted field stays as loaded. */
export interface ModPageEdit {
  name?: string
  summary?: string
  description?: string
  version?: string
  dryRun?: boolean
}

export interface ModPageSave {
  dryRun?: boolean
  changed: string[]
  request: PublishStep
  saved?: boolean
}

/** A Nexus mod page the publish / page dialogs act on (ProjectsView row menu). */
export interface PublishTarget {
  /** The mod page project (platform nexus). */
  projectId: number
  name: string
  url: string
  /** The linked code project's folder: the file dialog starts there. */
  folder?: string
}

/** GET/PUT /api/projects/{id}/links. */
export interface ProjectLinks {
  linkedTo?: number
  links?: number[]
}
// --- autopilot release runs (internal/release, internal/store autopilot; docs/AUTOPILOT.md) ---

/** Why a plan cannot start (refusal codes: no_profile, no_folder, dirty_folder, … remote_error). */
export interface Refusal {
  code: string
  message: string
}

/** POST /api/projects/{id}/release (and release/plan, publish-profile/check). */
export interface ReleaseRequest {
  /** Explicit version (minor/major allowed); missing = next patch. */
  version?: string
  head?: string
  items?: number[]
  /** Subset of the enabled targets (platform:external_id); missing = all. */
  targets?: string[]
  dryRun?: boolean
}

/** ok | no_api_key | bad_api_key | error | unavailable | unchecked. */
export type TargetAuth = 'ok' | 'no_api_key' | 'bad_api_key' | 'error' | 'unavailable' | 'unchecked' | ''

/** A linked mod page as the plan sees it. */
export interface PlanTarget {
  key: string
  projectId: number
  platform: string
  name: string
  url: string
  /** autopilot.publish[key]. */
  enabled: boolean
  /** publishProfile.targets[key] (Nexus: with a fileId). */
  configured: boolean
  /** The platform has an uploader. */
  publishable: boolean
  /** Part of this release. */
  selected: boolean
  latestVersion: string
  auth: TargetAuth
  error?: string
}

export interface PlanStep {
  step: string
  target?: string
  idemKey?: string
  request: string
  note?: string
}

export interface ReleaseCaps {
  projectReleasesToday: number
  projectMaxReleases: number
  releasesToday: number
  maxReleases: number
  publishesToday: number
  maxPublishes: number
}

/** The dry run of a release (and the resolved publish profile). */
export interface ReleasePlan {
  ok: boolean
  refusals: Refusal[] | null
  projectId: number
  project: string
  folder: string
  branch: string
  head: string
  remoteHead: string
  baseTag: string
  currentVersion: string
  version: string
  tag: string
  writesVersion: string[] | null
  writesChangelog: string[] | null
  changelog: string
  asset: string
  githubRelease: boolean
  smokeKind: string
  targets: PlanTarget[] | null
  steps: PlanStep[] | null
  caps: ReleaseCaps
  paused: boolean
  enabled: boolean
  /** Planned without network calls (publish profile GET): no remote head; targets' auth / latest version from the last platform answer, else unchecked. */
  local?: boolean
}

/** The build of a profile check: HEAD built in a temp worktree, nothing committed or sent. */
export interface CheckBuild {
  ok: boolean
  /** Why the build was skipped (no git folder / version / build profile). */
  skipped: string
  head: string
  writes: string[] | null
  command: { command: string; ok: boolean; exitCode: number; output: string; durationMs: number; timedOut: boolean } | null
  artifact: { path: string; name: string; size: number; sha256: string; sha1: string; md5: string } | null
  error: string
}

/** One target's planned upload requests (dry run). */
export interface CheckTargetPlan {
  key: string
  ok: boolean
  plan: { method: string; url: string; body?: unknown; note?: string }[] | null
  error: string
  /** bad_request | no_api_key | bad_api_key | platform_error | unavailable. */
  code?: string
}

/** POST …/publish-profile/check: the plan plus the build, archive check and per-target dry runs. */
export interface CheckResult extends ReleasePlan {
  build?: CheckBuild | null
  archiveCheck?: { ok: boolean; skipped: string; note: string; error: string } | null
  targetPlans?: CheckTargetPlan[] | null
}

export interface BuildProfile {
  command?: string
  output?: string
  path?: string
}
export interface VersionProfile {
  kind?: string
  path?: string
  key?: string
  pattern?: string
}
export interface ChangelogProfile {
  kind?: string
  path?: string
}
export interface SmokeProfile {
  kind?: string
  save?: string
  ticks?: number
  /** Factorio install folder or factorio.exe; empty = auto-detect (Steam / standalone). */
  install?: string
  command?: string
}
/** One publish target's settings; each platform reads its own fields. */
export interface TargetProfile {
  fileId?: string
  category?: string
  archivePrevious?: boolean
  appId?: number
  gameVersions?: string[]
  releaseType?: string
}
/** agents.projects.<key>.publishProfile (no secrets). */
export interface PublishProfile {
  build?: BuildProfile
  steamContent?: string
  version?: VersionProfile
  changelog?: ChangelogProfile
  smoke?: SmokeProfile
  targets?: Record<string, TargetProfile>
}

/** agents.projects.<key>.autopilot. */
export interface ProjectAutopilot {
  enabled: boolean
  autoTriage: boolean
  autoFix: boolean
  autoPush: boolean
  autoRelease: boolean
  githubRelease: boolean
  publish?: Record<string, boolean>
  autoReply: boolean
  autoClose: boolean
  coalesceMinutes: number
  maxBatchAgeHours: number
  maxReleasesPerDay: number
  maxDiffLines: number
  publishWithoutSmoke: boolean
  regressionWindowHours: number
}

/** GET/PUT /api/projects/{id}/publish-profile. */
export interface ProfileDoc {
  projectId: number
  project: string
  revision: number
  publishProfile: PublishProfile
  autopilot: ProjectAutopilot
  global: { paused: boolean; maxReleasesPerDay: number; maxPublishesPerDay: number }
  kinds: { version: string[]; changelog: string[]; smoke: string[] }
  resolved: ReleasePlan
}

/** A release run's frozen input (Run.manifest). */
export interface ReleaseManifest {
  projectId: number
  project: string
  repo: string
  repoUrl: string
  folder: string
  branch: string
  head: string
  baseTag: string
  fromVersion: string
  version: string
  tag: string
  modName: string
  asset: string
  githubRelease: boolean
  targets: { key: string; projectId: number; platform: string; externalId: string; name: string; url: string }[] | null
  items: number[] | null
  bumpSha?: string
  changelog?: string
}

/**
 * pushed / released: a fix run (kind fix) on the default branch, then claimed by a release run;
 * duplicate / answered / ignored: fix runs that auto-triage ended without a fix.
 */
export type RunState =
  | 'pending'
  | 'running'
  | 'held'
  | 'done'
  | 'cancelled'
  | 'failed'
  | 'pushed'
  | 'released'
  | 'duplicate'
  | 'answered'
  | 'ignored'
export type RunStepState = 'pending' | 'sending' | 'sent' | 'failed' | 'unknown' | 'skipped'

export interface ReleaseRun {
  id: number
  /** release | fix */
  kind: string
  projectId: number
  state: RunState
  origin: 'manual' | 'mcp' | 'auto' | string
  releaseId?: number
  manifest: Partial<ReleaseManifest> | null
  version: string
  artifactSha256: string
  /** check:<step>[:<target>] | failed:<step>[:<target>] | auth:<platform> | tag_conflict | … */
  heldReason: string
  createdAt: string
  updatedAt: string
}

export interface RunStep {
  id: number
  runId: number
  seq: number
  step: string
  target: string
  state: RunStepState
  attempt: number
  idemKey: string
  /** Raw JSON: a string or an object. */
  request: unknown
  externalRef: string
  error: string
  startedAt: string
  finishedAt: string
}

/** GET /api/runs/{id}, SSE autopilot.run. */
export interface RunView {
  run: ReleaseRun
  steps: RunStep[] | null
  items: { itemId: number; role: string }[] | null
}

export interface CancelResult {
  run: ReleaseRun
  bumpDropped: boolean
  note: string
}

/** One autopilot activity log entry (GET /api/autopilot/events, SSE autopilot.event). */
export interface AutopilotEvent {
  id: number
  at: string
  runId?: number
  projectId?: number
  itemId?: number
  /** release.held | release.done | release.cancelled | … (unknown kinds show the title only). */
  kind: string
  severity: 'info' | 'attention' | string
  title: string
  /** Free-form JSON object: reason (held reason code), detail (text), version, targets, … */
  detail: Record<string, unknown> | null
  /** "" = unread. */
  readAt: string
}

/** Unread counters (also the SSE autopilot.unread payload). */
export interface AutopilotUnread {
  unread: number
  attention: number
}

/** GET /api/autopilot/events. */
export interface AutopilotEvents extends AutopilotUnread {
  events: AutopilotEvent[] | null
}
