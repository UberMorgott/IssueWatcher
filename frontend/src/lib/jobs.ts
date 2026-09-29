import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useToast } from 'primevue/usetoast'
import { useConfirm } from 'primevue/useconfirm'
import { useI18n } from 'vue-i18n'
import { api } from '../api/client'
import type { Job, JobFlow, JobState, LocalOutcome, QueuedJob } from '../api/types'
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
  label: 'pi pi-tags',
}

export const JOB_FLOWS: JobFlow[] = ['fix', 'reply', 'label']

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

/** A fix job that ran in the mapped folder itself (no worktree, no PR). */
export function isDirect(j: Job): boolean {
  return j.flow === 'fix' && j.result.mode === 'direct'
}

/** Outcome of a direct fix job; "" = not a direct job or no outcome yet. */
export function jobOutcome(j: Job): LocalOutcome | '' {
  if (!isDirect(j)) return ''
  const l = j.result.local
  if (l?.closed) return 'closed'
  if (l?.outcome) return l.outcome
  return j.state === 'failed' ? 'failed' : ''
}

/** Badge colour of an outcome (a JobBadge state class). */
export const OUTCOME_TONE: Record<LocalOutcome, string> = {
  fixed_local: 'needs-review',
  pushed: 'running',
  closed: 'done',
  not_reproduced: 'cancelled',
  needs_info: 'needs-review',
  no_commit: 'needs-review',
  failed: 'failed',
}

/** Push is offered for a direct fix job under review that made commits. */
export function canPush(j: Job): boolean {
  return isDirect(j) && j.state === 'needs_review' && !!j.result.local?.commits?.length
}

/** A ticking clock (1 s) shared by every running-job view while one is mounted. */
const now = ref(Date.now())
let clockUsers = 0
let clock: number | undefined
export function useNow() {
  onMounted(() => {
    if (clockUsers++ === 0) {
      now.value = Date.now()
      clock = window.setInterval(() => (now.value = Date.now()), 1000)
    }
  })
  onBeforeUnmount(() => {
    if (--clockUsers === 0) window.clearInterval(clock)
  })
  return now
}

/**
 * Push of a direct fix job: confirm, POST /api/jobs/{id}/push, toast the result.
 * Resolves with the updated job, or null (declined, failed: the caller reloads).
 */
export function usePush() {
  const confirm = useConfirm()
  const toast = useToast()
  const { t } = useI18n()
  const busy = ref(false)
  function push(j: Job): Promise<Job | null> {
    return new Promise((resolve) => {
      confirm.require({
        header: t('job.confirm.pushTitle'),
        message: t('job.confirm.push', { ref: `${j.repo}#${j.number}`, path: j.localPath || j.result.local?.dir || '—' }),
        icon: 'pi pi-question-circle',
        rejectProps: { label: t('common.cancel'), severity: 'secondary', text: true },
        acceptProps: { label: t('job.actions.push') },
        reject: () => resolve(null),
        accept: async () => {
          busy.value = true
          const r = await api.jobAction(j.id, 'push')
          busy.value = false
          if (!r.ok) {
            const detail = r.status === 409 ? t('job.notAllowed', { error: r.error }) : r.error
            toast.add({ severity: 'error', summary: t('job.actionFailed.push'), detail, life: 8000 })
            resolve(null)
            return
          }
          toast.add({ severity: 'success', summary: t('job.pushed'), detail: `${j.repo}#${j.number}`, life: 6000 })
          resolve(r.data)
        },
      })
    })
  }
  return { push, busy }
}

/** Does a live job row belong to a list filtered by state / flow / project? */
export function matches(j: Job, f: { state: string; flow: string; project: number; origin?: string }): boolean {
  if (f.flow && j.flow !== f.flow) return false
  if (f.origin && j.origin !== f.origin) return false
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
