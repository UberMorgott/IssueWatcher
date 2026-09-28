import { defineStore } from 'pinia'
import { computed, markRaw, onBeforeUnmount } from 'vue'
import { api, normJob } from '../api/client'
import type { AgentProfile, Job, JobFlow, JobLogEvent, QueuedJob } from '../api/types'
import { useSettingsStore } from './settings'

type JobListener = (j: Job) => void
type LogListener = (e: JobLogEvent) => void

/**
 * Agent jobs: fan-out of the job.changed / job.log live events to the views
 * that show jobs (every event reaches every listener, none is coalesced), the
 * configured profiles and the dispatch call shared by Issues and the item page.
 */
export const useJobsStore = defineStore('jobs', () => {
  const jobListeners = markRaw(new Set<JobListener>())
  const logListeners = markRaw(new Set<LogListener>())

  function emitJob(data: unknown) {
    const j = data as Job | null
    if (!j || typeof j.id !== 'number') return
    const job = normJob(j)
    jobListeners.forEach((f) => f(job))
  }
  function emitLog(data: unknown) {
    const e = data as JobLogEvent | null
    if (!e || typeof e.id !== 'number' || !Array.isArray(e.steps)) return
    logListeners.forEach((f) => f(e))
  }

  const settings = useSettingsStore()
  const agents = computed(() => settings.doc?.settings.agents)
  const profiles = computed<AgentProfile[]>(() => agents.value?.profiles ?? [])
  /** Profile id the server uses for a flow when none is picked (role coder / responder). */
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

  return { jobListeners, logListeners, emitJob, emitLog, profiles, roleProfile, profileName, dispatch }
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
