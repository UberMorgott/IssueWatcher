import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import type { Job, JobFlow, JobState, QueuedJob } from '../api/types'
import { summarise } from '../stores/jobs'

export const JOB_STATES: JobState[] = ['queued', 'running', 'needs_review', 'done', 'failed', 'cancelled']
export const ACTIVE_STATES: JobState[] = ['queued', 'running', 'needs_review']

/** Icon per state (running spins). */
export const STATE_ICON: Record<JobState, string> = {
  queued: 'pi pi-clock',
  running: 'pi pi-spin pi-spinner',
  needs_review: 'pi pi-eye',
  done: 'pi pi-check-circle',
  failed: 'pi pi-times-circle',
  cancelled: 'pi pi-ban',
}

export const FLOW_ICON: Record<JobFlow, string> = {
  fix: 'pi pi-wrench',
  reply: 'pi pi-comment',
}

export function isActive(s: JobState): boolean {
  return ACTIVE_STATES.includes(s)
}

/** Agent + reviewer spend of the current attempt. */
export function jobCost(j: Job): number {
  return (j.result.agent?.costUsd ?? 0) + (j.result.review?.costUsd ?? 0)
}

/** Run time: start → finish, or → now while running. */
export function jobDuration(j: Job, now = Date.now()): number | undefined {
  if (!j.startedAt) return undefined
  const start = Date.parse(j.startedAt)
  const end = j.finishedAt ? Date.parse(j.finishedAt) : j.state === 'running' ? now : NaN
  return Number.isFinite(start) && Number.isFinite(end) ? Math.max(0, end - start) : undefined
}

/** Does a live job row belong to a list filtered by state / flow / project? */
export function matches(j: Job, f: { state: string; flow: string; project: number }): boolean {
  if (f.flow && j.flow !== f.flow) return false
  if (f.project && j.projectId !== f.project) return false
  if (f.state === 'active') return isActive(j.state)
  return !f.state || j.state === f.state
}

/** Summary toast of a dispatch (queued N, skipped M with links to their jobs). */
export function useDispatchToast() {
  const toast = useToast()
  const { t } = useI18n()
  return (res: { ok: true; data: { jobs: QueuedJob[] } } | { ok: false; error: string }) => {
    if (!res.ok) {
      toast.add({ severity: 'error', summary: t('jobs.dispatchFailed'), detail: res.error, life: 6000 })
      return
    }
    const { queued, existing, failed } = summarise(res.data.jobs)
    const links = [
      ...queued.map((j) => ({ label: `${j.repo}#${j.number}`, to: `/jobs/${j.id}`, note: '' })),
      ...existing.map((j) => ({ label: `${j.repo}#${j.number}`, to: `/jobs/${j.id}`, note: t('jobs.alreadyQueued') })),
    ].slice(0, 8)
    const parts = [t('jobs.queuedN', queued.length)]
    if (existing.length) parts.push(t('jobs.skippedN', existing.length))
    if (failed.length) parts.push(t('jobs.failedN', failed.length))
    toast.add({
      group: 'jobs',
      severity: failed.length && !queued.length ? 'error' : existing.length || failed.length ? 'warn' : 'success',
      summary: parts.join(' · '),
      detail: failed.map((f) => (f.error === 'not_found' ? t('jobs.itemMissing', { id: f.itemId }) : f.error)).join('; '),
      life: 8000,
      data: { links },
    } as never)
  }
}
