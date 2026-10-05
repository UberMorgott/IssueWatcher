import { t, te } from '../i18n'
import type { PlanTarget, Refusal, RunState, RunStepState } from '../api/types'
import type { Result } from '../api/client'
import { platformName } from './platforms'

// Autopilot release runs (docs/AUTOPILOT.md): human text for refusal codes,
// held reasons and step names, shared by the release dialog and the Runs pages.

/** Targets that name a key (manifest targets, plan targets). */
export type NamedTargets = readonly { key: string; name: string; platform: string }[] | null

export const RUN_STATES: RunState[] = ['pending', 'running', 'held', 'pushed', 'released', 'duplicate', 'answered', 'ignored', 'done', 'cancelled', 'failed']

/** Run kind as text: «Релиз» / «Исправление»; unknown kinds as is. */
export function runKindText(kind: string): string {
  return te('release.kind.' + kind) ? t('release.kind.' + kind) : kind
}

/** Unfinished runs (cancel allowed). */
export const runActive = (s: RunState) => s === 'pending' || s === 'running' || s === 'held'

/** Badge tone of a run state (JobBadge colour classes). */
export const RUN_TONE: Record<RunState, string> = {
  pending: '',
  running: 'running',
  held: 'needs-review',
  done: 'done',
  cancelled: 'cancelled',
  failed: 'failed',
  pushed: 'running',
  released: 'done',
  duplicate: 'done',
  answered: 'done',
  ignored: 'cancelled',
}

export const RUN_ICON: Record<RunState, string> = {
  pending: 'pi pi-clock',
  running: 'pi pi-spin pi-spinner',
  held: 'pi pi-pause-circle',
  done: 'pi pi-check-circle',
  cancelled: 'pi pi-ban',
  failed: 'pi pi-times-circle',
  pushed: 'pi pi-upload',
  released: 'pi pi-check-circle',
  duplicate: 'pi pi-clone',
  answered: 'pi pi-comment',
  ignored: 'pi pi-minus-circle',
}

export const STEP_ICON: Record<RunStepState, string> = {
  pending: 'pi pi-circle',
  sending: 'pi pi-spin pi-spinner',
  sent: 'pi pi-check-circle',
  failed: 'pi pi-times-circle',
  unknown: 'pi pi-question-circle',
  skipped: 'pi pi-minus-circle',
}

/** A refusal as text: our wording for a known code, the server's message otherwise. */
export function refusalText(r: Refusal): string {
  return te('release.refusal.' + r.code) ? t('release.refusal.' + r.code) : r.message || r.code
}

/** Release steps whose target is an item id (the reply to / closing of an issue). */
export const ITEM_STEPS = ['reply', 'close']

/** Name of a step (bump, build, publish, …) without its target. */
export function stepName(step: string): string {
  return te('release.step.' + step) ? t('release.step.' + step) : step
}

/** A target key (platform:external_id) as «Nexus Mods «name»», named from the manifest when known. */
export function targetLabel(key: string, targets?: NamedTargets): string {
  const tg = targets?.find((x) => x.key === key)
  const platform = tg?.platform || key.slice(0, Math.max(0, key.indexOf(':')))
  const name = platformName(platform)
  return tg?.name ? `${name} «${tg.name}»` : name || key
}

/** A step with its target (publish · Nexus Mods «X»; gh_asset · file name; reply / close · #item). */
export function stepLabel(s: { step: string; target?: string }, targets?: NamedTargets): string {
  if (!s.target) return stepName(s.step)
  if (ITEM_STEPS.includes(s.step)) return `${stepName(s.step)} · #${s.target}`
  return `${stepName(s.step)} · ${s.step === 'gh_asset' ? s.target : targetLabel(s.target, targets)}`
}

/**
 * Held reason as text: check:<step>[:<target>] / failed:<step>[:<target>] /
 * auth:<platform> name the step or platform; the rest are fixed codes.
 */
export function heldText(reason: string, targets?: NamedTargets): string {
  if (!reason) return ''
  const [kind, step = '', ...rest] = reason.split(':')
  const target = rest.join(':')
  if (kind === 'check' || kind === 'failed') {
    return t('release.held.' + kind, { step: stepLabel({ step, target }, targets) })
  }
  if (kind === 'auth') return t('release.held.auth', { platform: platformName(step) })
  return te('release.held.' + reason) ? t('release.held.' + reason) : reason
}

/** A step note: a known code (no_verify, smoke_missing, …) as text, else the server's wording. */
export function noteText(note: string): string {
  if (!note) return ''
  return /^[a-z_]+$/.test(note) && te('release.held.' + note) ? t('release.held.' + note) : note
}

/** Name of a smoke kind (factorio / command / none), unknown kinds as is. */
export function smokeKindText(kind: string): string {
  return te('release.profile.smokeKind.' + kind) ? t('release.profile.smokeKind.' + kind) : kind
}

/**
 * Why a target cannot be published to ("" = it can): the platform has no
 * uploader, no / bad API key, or the profile does not configure it yet.
 */
export function targetBlock(tg: PlanTarget): string {
  const platform = platformName(tg.platform)
  if (!tg.publishable) return t('release.target.notPublishable', { platform })
  if (tg.platform === 'steam' && tg.auth === 'no_api_key') return t('release.target.noSteamLogin')
  if (tg.platform === 'steam' && tg.auth === 'bad_api_key') return t('release.target.steamRelogin')
  if (tg.auth === 'no_api_key') return t('release.target.noKey', { platform })
  if (tg.auth === 'bad_api_key') return t('release.target.badKey', { platform })
  if (!tg.configured) return t(tg.platform === 'nexus' ? 'release.target.noFileId' : 'release.target.notConfigured')
  return ''
}

/** A failed release call as text: known codes (agent_caller, bad_state, …) localised, else the server's reason. */
export function releaseError(r: Extract<Result<unknown>, { ok: false }>): string {
  const code = (r.body as { code?: string } | undefined)?.code ?? ''
  if (code && te('release.errors.' + code)) return t('release.errors.' + code)
  if (code && te('release.refusal.' + code)) return t('release.refusal.' + code)
  return r.error
}

/** A step's raw request (JSON string or object) as display text. */
export function requestText(v: unknown): string {
  if (v === null || v === undefined || v === '') return ''
  if (typeof v === 'string') return v
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

/** "abcdef1234…" → "abcdef1". */
export const shortSha = (s: string) => (/^[0-9a-f]{12,}$/i.test(s) ? s.slice(0, 7) : s)

/** Bytes → "1.2 MB". */
export function bytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return ''
  const u = ['B', 'KB', 'MB', 'GB']
  let i = 0
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`
}
