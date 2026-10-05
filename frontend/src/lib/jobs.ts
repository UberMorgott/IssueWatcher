import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useToast } from 'primevue/usetoast'
import { useConfirm } from 'primevue/useconfirm'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { api } from '../api/client'
import type { Agents, Job, JobErrorCode, JobFlow, JobState, LocalOutcome, QueuedJob } from '../api/types'
import { summarise } from '../stores/jobs'
import { useSettingsStore } from '../stores/settings'

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
  triage: 'pi pi-sort-amount-down',
}

export const JOB_FLOWS: JobFlow[] = ['fix', 'reply', 'label', 'triage']

/** owner/repo#N of an issue job; owner/repo of a project job (triage, no item). */
export function jobRef(j: Pick<Job, 'repo' | 'number' | 'itemId'>): string {
  return j.itemId ? `${j.repo}#${j.number}` : j.repo
}

/** Error codes the UI has its own text for (runner Code* constants). */
export const JOB_ERROR_CODES: JobErrorCode[] = ['no_folder', 'dirty_folder','no_profile', 'no_cli', 'timeout', 'agent_failed', 'agent_auth', 'git', 'interrupted', 'mod_item', 'reply_too_long']

/** The settings page that fixes an error code ('' = none). */
export function errorCodeLink(code: string): string {
  if (code === 'no_folder') return '/settings/projects'
  if (code === 'no_cli' || code === 'no_profile' || code === 'mod_item') return '/settings/agents'
  return ''
}

/** A known error code's text key ('' = unknown code). */
export function errorCodeKey(code: string | undefined): string {
  return code && (JOB_ERROR_CODES as string[]).includes(code) ? 'job.errorCode.' + code : ''
}

/** Push / PR of a mod-page fix are allowed (agents.modPush, overridable per mod page); always true for a code project's fix. */
export function modPushAllowed(j: Pick<Job, 'mod' | 'projectKey'>, agents: Agents | undefined): boolean {
  if (!j.mod) return true
  return agents?.projects?.[j.projectKey ?? '']?.modPush ?? agents?.modPush ?? false
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

/** A fix job that ran in the mapped folder itself (no worktree, no PR); also folder mode. */
export function isDirect(j: Job): boolean {
  return j.flow === 'fix' && (j.result.mode === 'direct' || j.result.mode === 'folder')
}

/** A fix job that ran in a folder that is not the project's git clone: no commit, push or PR. */
export function isFolderRun(j: Job): boolean {
  return j.flow === 'fix' && j.result.mode === 'folder'
}

/** Outcome of a direct fix job; "" = not a direct job or no outcome yet. */
export function jobOutcome(j: Job): LocalOutcome | '' {
  if (!isDirect(j)) return ''
  const l = j.result.local
  if (l?.closed) return 'closed'
  if (l?.outcome) return l.outcome
  return j.state === 'failed' ? 'failed' : ''
}

/** Fix outcomes that say the item is no bug: it wants a reply, not a fix. */
export const NON_BUG_OUTCOMES: readonly LocalOutcome[] = ['feedback', 'question', 'suggestion']

/** Badge colour of an outcome (a JobBadge state class). */
export const OUTCOME_TONE: Record<LocalOutcome, string> = {
  fixed_local: 'needs-review',
  pushed: 'running',
  closed: 'done',
  not_reproduced: 'cancelled',
  feedback: 'needs-review',
  question: 'needs-review',
  suggestion: 'needs-review',
  needs_info: 'needs-review',
  no_commit: 'needs-review',
  changed_folder: 'done',
  no_changes: 'needs-review',
  failed: 'failed',
}

/** A direct fix job under review that made commits: Push applies to it (see canPush). */
export function pushEligible(j: Job): boolean {
  return isDirect(j) && !isFolderRun(j) && j.state === 'needs_review' && !!j.result.local?.commits?.length
}

/** Push / PR of this job are off: a mod-page fix while agents.modPush is off for that mod page. */
export function modPushOff(j: Job): boolean {
  return !!j.mod && !modPushAllowed(j, useSettingsStore().doc?.settings.agents)
}

/** Push is offered for a direct fix job under review that made commits (a mod-page fix only with modPush). */
export function canPush(j: Job): boolean {
  return pushEligible(j) && !modPushOff(j)
}

/** The user-facing text of a failed job action: a known error code's text, else the server's message. */
export function actionError(t: (key: string) => string, r: { status: number; error: string; body?: unknown }): string {
  const key = errorCodeKey((r.body as { code?: string } | undefined)?.code)
  if (key) return t(key)
  return r.status === 409 ? t('job.notAllowed') : r.error
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
        message: t('job.confirm.push', { ref: jobRef(j), path: j.localPath || j.result.local?.dir || '—' }),
        icon: 'pi pi-question-circle',
        rejectProps: { label: t('common.cancel'), severity: 'secondary', text: true },
        acceptProps: { label: t('job.actions.push') },
        reject: () => resolve(null),
        accept: async () => {
          busy.value = true
          const r = await api.jobAction(j.id, 'push')
          busy.value = false
          if (!r.ok) {
            toast.add({ severity: 'error', summary: t('job.actionFailed.push'), detail: actionError(t, r), life: 8000 })
            resolve(null)
            return
          }
          toast.add({ severity: 'success', summary: t('job.pushed'), detail: jobRef(j), life: 6000 })
          resolve(r.data)
        },
      })
    })
  }
  return { push, busy }
}

/**
 * Does a live job row belong to a list filtered by state / flow / project?
 * projects: the project's scope (itself + its linked mod pages); without it only the exact id.
 */
export function matches(j: Job, f: { state: string; flow: string; project: number; origin?: string; projects?: Set<number> }): boolean {
  if (f.flow && j.flow !== f.flow) return false
  if (f.origin && j.origin !== f.origin) return false
  if (f.project && !(f.projects ? f.projects.has(j.projectId) : j.projectId === f.project)) return false
  if (f.state === 'active') return isActive(j.state)
  return !f.state || j.state === f.state
}

/** Summary toast of a dispatch (queued N, skipped M with links to their jobs). */
export function useDispatchToast() {
  const toast = useToast()
  const { t } = useI18n()
  return (res: { ok: true; data: { jobs: QueuedJob[] } } | { ok: false; error: string; body?: unknown }) => {
    if (!res.ok) {
      const body = res.body as { code?: string; hint?: string; dirty?: string[] } | undefined
      const noFolder = body?.code === 'no_folder' // 409: nothing queued
      const detail =
        body?.code === 'dirty_folder' // 409: nothing queued, the folder has uncommitted changes
          ? [t('folder.dirty'), ...(body.dirty ?? []).slice(0, 5)].join('\n')
          : noFolder
            ? body?.hint === 'link_mod'
              ? t('folder.neededMod')
              : t('folder.needed')
            : actionError(t, { status: 0, ...res })
      toast.add({ severity: 'error', summary: t('jobs.dispatchFailed'), detail, life: 6000 })
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
      detail: [
        ...(failed.some((f) => f.error === 'dirty_folder') ? [t('folder.dirtySkipped', { n: failed.filter((f) => f.error === 'dirty_folder').length })] : []),
        ...failed.filter((f) => f.error !== 'no_folder' && f.error !== 'dirty_folder').map((f) => (f.error === 'not_found' ? t('jobs.itemMissing', { id: f.itemId }) : f.error)),
        ...(failed.some((f) => f.error === 'no_folder') ? [t('folder.skipped', { n: failed.filter((f) => f.error === 'no_folder').length })] : []),
      ].join('; '),
      life: 8000,
      data: { links },
    } as never)
  }
}

/**
 * «Разобрать проект»: POST /api/projects/{id}/triage, then open the triage job
 * (the unfinished one when the project already has it). busy = the projects
 * whose start is in flight (each row spins on its own).
 */
export function useTriage() {
  const toast = useToast()
  const router = useRouter()
  const { t } = useI18n()
  const busy = ref(new Set<number>())
  async function run(projectId: number, repo: string) {
    if (busy.value.has(projectId)) return
    busy.value = new Set(busy.value).add(projectId)
    const r = await api.triage(projectId)
    const rest = new Set(busy.value)
    rest.delete(projectId)
    busy.value = rest
    if (r.ok) {
      toast.add({ severity: 'success', summary: t('jobs.triage.queued'), detail: repo, life: 5000 })
      void router.push(`/jobs/${r.data.id}`)
      return
    }
    if (r.job) {
      toast.add({ severity: 'info', summary: t('jobs.triage.exists'), detail: repo, life: 5000 })
      void router.push(`/jobs/${r.job.id}`)
      return
    }
    const code = (r.body as { code?: string } | undefined)?.code ?? ''
    const to = errorCodeLink(code)
    toast.add({
      group: 'jobs',
      severity: 'error',
      summary: t('jobs.triage.failed'),
      detail: `${repo}: ${actionError(t, r)}`,
      life: 8000,
      data: { links: to ? [{ label: t(to === '/settings/projects' ? 'settings.sections.projects' : 'settings.sections.agents'), to, note: '' }] : [] },
    } as never)
  }
  return { run, busy }
}