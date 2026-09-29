import { defineStore } from 'pinia'
import { computed, markRaw, onBeforeUnmount, shallowReactive } from 'vue'
import { api, normJob } from '../api/client'
import type { AgentProfile, Job, JobFlow, JobLogEvent, JobStep, QueuedJob } from '../api/types'
import { useSettingsStore } from './settings'

type JobListener = (j: Job) => void
type LogListener = (e: JobLogEvent) => void

/** Step kinds that describe what the agent is doing right now. */
const STEP_KINDS = new Set(['tool', 'text', 'info'])

/** Latest meaningful step of a batch as one clipped line; "" = none. */
export function stepLine(steps: JobStep[]): string {
  for (let i = steps.length - 1; i >= 0; i--) {
    const s = steps[i]
    if (!STEP_KINDS.has(s.kind)) continue
    const line = s.text.split('\n').find((l) => l.trim())?.trim() ?? ''
    if (line) return line.length > 200 ? line.slice(0, 200) + '…' : line
  }
  return ''
}

/**
 * Agent jobs: fan-out of the job.changed / job.log live events to the views
 * that show jobs (every event reaches every listener, none is coalesced), the
 * configured profiles and the dispatch call shared by Issues and the item page.
 * It also keeps the latest known state of unfinished and recent jobs (Issues
 * rows read run time and outcome from it) and the current step of each run.
 */
export const useJobsStore = defineStore('jobs', () => {
  const jobListeners = markRaw(new Set<JobListener>())
  const logListeners = markRaw(new Set<LogListener>())

  /** Latest known job by id: seeded from the API, then kept by job.changed. */
  const byId = shallowReactive(new Map<number, Job>())
  /** Current step line of a job's attempt (from job.log, or the log tail on load). */
  const steps = shallowReactive(new Map<number, { attempt: number; text: string }>())
  const tailLoading = new Set<number>()
  /** Unfinished jobs (queued / running / needs_review): the sidebar badge. */
  const activeCount = computed(() => {
    let n = 0
    for (const j of byId.values()) if (j.state === 'queued' || j.state === 'running' || j.state === 'needs_review') n++
    return n
  })

  /**
   * Applies a job.changed payload. Returns the job when this event ends an
   * agent run (running → needs_review / failed / done, not a publish step).
   */
  function emitJob(data: unknown): Job | null {
    const j = data as Job | null
    if (!j || typeof j.id !== 'number') return null
    const job = normJob(j)
    const prev = byId.get(job.id)
    byId.set(job.id, job)
    jobListeners.forEach((f) => f(job))
    const finished =
      prev?.state === 'running' &&
      prev.phase !== 'publish' &&
      job.phase !== 'publish' &&
      (job.state === 'needs_review' || job.state === 'failed' || job.state === 'done')
    return finished ? job : null
  }
  function emitLog(data: unknown) {
    const e = data as JobLogEvent | null
    if (!e || typeof e.id !== 'number' || !Array.isArray(e.steps)) return
    const text = stepLine(e.steps)
    if (text) steps.set(e.id, { attempt: e.attempt, text })
    logListeners.forEach((f) => f(e))
  }

  /** Current step of a running attempt: live, else loaded once from the log tail. */
  function stepOf(id: number, attempt: number): string {
    const s = steps.get(id)
    return s && s.attempt === attempt ? s.text : ''
  }
  async function loadStep(id: number, attempt: number) {
    if (steps.get(id)?.attempt === attempt || tailLoading.has(id)) return
    tailLoading.add(id)
    const r = await api.jobLog(id, attempt)
    tailLoading.delete(id)
    if (!r.ok || steps.get(id)?.attempt === attempt) return
    steps.set(id, { attempt, text: stepLine(r.data.steps ?? []) })
  }

  /** Loads unfinished and recent fix jobs (app start, SSE reconnect). */
  async function seed() {
    const at = Date.now()
    const [active, done] = await Promise.all([api.jobs({ state: 'active', limit: 100 }), api.jobs({ state: 'done', flow: 'fix', limit: 100 })])
    if (!active.ok) return
    const fresh = new Set(active.data.items.map((j) => j.id))
    // A job that finished while the stream was down must not look running any more.
    for (const [id, j] of byId) {
      if (!fresh.has(id) && Date.parse(j.updatedAt) < at && (j.state === 'queued' || j.state === 'running' || j.state === 'needs_review')) byId.delete(id)
    }
    for (const j of [...(done.ok ? done.data.items : []), ...active.data.items]) {
      const cur = byId.get(j.id) // a live event newer than this response wins
      if (!cur || !(Date.parse(cur.updatedAt) > Date.parse(j.updatedAt))) byId.set(j.id, j)
    }
  }

  const settings = useSettingsStore()
  const agents = computed(() => settings.doc?.settings.agents)
  const profiles = computed<AgentProfile[]>(() => agents.value?.profiles ?? [])
  /** Profile id the server uses for a flow when none is picked (role coder / responder; label → responder). */
  function roleProfile(flow: JobFlow): string {
    const r = agents.value?.roles
    return (flow === 'fix' ? r?.coder : r?.responder) ?? ''
  }
  function profileName(id: string): string {
    return profiles.value.find((p) => p.id === id)?.name ?? id
  }

  /** Queues one job per item; per-item outcome (queued, exists, error). */
  async function dispatch(itemIds: number[], flow: JobFlow, profileId?: string) {
    return api.createJobs(itemIds, flow, profileId)
  }

  return { jobListeners, logListeners, emitJob, emitLog, byId, activeCount, stepOf, loadStep, seed, profiles, roleProfile, profileName, dispatch }
})

/** Subscribes the calling component to job.changed / job.log while it is mounted. */
export function useJobEvents(on: { job?: JobListener; log?: LogListener }) {
  const s = useJobsStore()
  if (on.job) s.jobListeners.add(on.job)
  if (on.log) s.logListeners.add(on.log)
  onBeforeUnmount(() => {
    if (on.job) s.jobListeners.delete(on.job)
    if (on.log) s.logListeners.delete(on.log)
  })
}

/** Splits POST /api/jobs results for the summary toast. */
export function summarise(jobs: QueuedJob[]) {
  const queued = jobs.filter((q) => !q.error && q.job).map((q) => q.job as Job)
  const existing = jobs.filter((q) => q.error === 'exists' && q.job).map((q) => q.job as Job)
  const failed = jobs.filter((q) => q.error && q.error !== 'exists')
  return { queued, existing, failed }
}
