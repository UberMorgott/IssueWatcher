<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import Button from 'primevue/button'
import Message from 'primevue/message'
import Skeleton from 'primevue/skeleton'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import EmptyState from '../components/EmptyState.vue'
import PlatformIcon from '../components/PlatformIcon.vue'
import RunBadge from '../components/RunBadge.vue'
import { api } from '../api/client'
import type { RunStep, RunView } from '../api/types'
import { useCrumbs } from '../lib/crumbs'
import { absTime, elapsed, relTime } from '../lib/format'
import { STEP_ICON, bytes, heldText, noteText, releaseError, requestText, runActive, shortSha, stepLabel, targetLabel } from '../lib/release'
import { safeUrl } from '../lib/safeUrl'
import { useAppStore } from '../stores/app'

// One release run: header (state, version, held reason, actions «Продолжить» /
// «Отменить»), then the step timeline with each step's request, external
// result (commit, release, platform version) and error; «Пропустить платформу»
// on a target's publish / availability step. Live from SSE autopilot.run.
const props = defineProps<{ id: string }>()
const app = useAppStore()
const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

const view = ref<RunView | null>(null)
const state = ref<'loading' | 'ok' | 'missing' | 'error'>('loading')
const errorText = ref('')
const busy = ref('')

async function load(quiet = false) {
  if (!quiet) state.value = 'loading'
  const r = await api.run(props.id)
  if (!r.ok) {
    if (!quiet || !view.value) {
      state.value = r.status === 404 ? 'missing' : 'error'
      errorText.value = r.error
    }
    return
  }
  view.value = r.data
  state.value = 'ok'
}
onMounted(() => void load())
watch(() => props.id, () => void load())
watch(
  () => app.dataVersion,
  () => {
    if (app.lastChanges === null) void load(true)
  },
)
watch(
  () => app.lastRun,
  (v) => {
    if (v && String(v.run.id) === String(props.id)) view.value = v
  },
  { flush: 'sync' },
)

const run = computed(() => view.value?.run ?? null)
const m = computed(() => run.value?.manifest ?? null)
const steps = computed(() => [...(view.value?.steps ?? [])].sort((a, b) => a.seq - b.seq))
const targets = computed(() => m.value?.targets ?? null)
const projectName = computed(() => m.value?.repo || app.repos.find((x) => x.id === run.value?.projectId)?.name || '')

useCrumbs(() => [
  { label: t('nav.releases'), to: '/runs' },
  { label: run.value ? `#${run.value.id} · ${projectName.value} ${run.value.version || ''}`.trim() : '#' + props.id, mono: true },
])

const canResume = computed(() => run.value?.state === 'held')
const canCancel = computed(() => !!run.value && runActive(run.value.state))
/** «Пропустить платформу»: a target's publish / availability step not sent yet, run held or pending. */
function canSkip(s: RunStep): boolean {
  const st = run.value?.state
  return (st === 'held' || st === 'pending') && (s.step === 'publish' || s.step === 'available') && !!s.target && !['sent', 'skipped', 'sending'].includes(s.state)
}

async function resume() {
  const r = run.value
  if (!r || busy.value) return
  busy.value = 'resume'
  const res = await api.resumeRun(r.id)
  busy.value = ''
  if (!res.ok) {
    toast.add({ severity: 'error', summary: t('release.actionFailed.resume'), detail: releaseError(res), life: 8000 })
    void load(true)
    return
  }
  toast.add({ severity: 'success', summary: t('release.resumed'), life: 3000 })
  void load(true)
}

function cancel() {
  const r = run.value
  if (!r) return
  confirm.require({
    header: t('release.confirm.cancelTitle'),
    message: t('release.confirm.cancel'),
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', text: true },
    acceptProps: { label: t('release.actions.cancel'), severity: 'danger' },
    accept: async () => {
      busy.value = 'cancel'
      const res = await api.cancelRun(r.id)
      busy.value = ''
      if (!res.ok) {
        toast.add({ severity: 'error', summary: t('release.actionFailed.cancel'), detail: releaseError(res), life: 8000 })
        void load(true)
        return
      }
      toast.add({
        severity: 'info',
        summary: t('release.cancelled'),
        detail: [res.data.bumpDropped ? t('release.bumpDropped') : '', res.data.note].filter(Boolean).join(' · ') || undefined,
        life: 8000,
      })
      void load(true)
    },
  })
}

function skip(s: RunStep) {
  const r = run.value
  if (!r) return
  const target = targetLabel(s.target, targets.value)
  confirm.require({
    header: t('release.confirm.skipTitle'),
    message: t('release.confirm.skip', { target }),
    icon: 'pi pi-question-circle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', text: true },
    acceptProps: { label: t('release.actions.skip') },
    accept: async () => {
      busy.value = 'skip:' + s.id
      const res = await api.skipStep(r.id, s.step, s.target)
      busy.value = ''
      if (!res.ok) {
        toast.add({ severity: 'error', summary: t('release.actionFailed.skip'), detail: releaseError(res), life: 8000 })
      }
      void load(true)
    },
  })
}

// --- a step's result
type Ref = { text: string; href?: string; mono?: boolean }
function stepRef(s: RunStep): Ref | null {
  const x = s.externalRef
  if (!x) return null
  const repoUrl = m.value?.repoUrl ?? ''
  if (/^https?:\/\//.test(x)) return { text: x, href: x }
  if (s.step === 'bump' || s.step === 'push' || s.step === 'tag') {
    return { text: shortSha(x), href: repoUrl && /^[0-9a-f]{7,}$/i.test(x) ? `${repoUrl}/commit/${x}` : undefined, mono: true }
  }
  if (s.step === 'build') {
    try {
      const a = JSON.parse(x) as { name?: string; size?: number; sha256?: string }
      return { text: [a.name, bytes(a.size ?? 0), a.sha256 ? 'sha256 ' + shortSha(a.sha256) : ''].filter(Boolean).join(' · '), mono: true }
    } catch {
      return { text: x, mono: true }
    }
  }
  if (s.step === 'gate') return null // shown as details below
  if (s.step === 'smoke') return x.startsWith('{') ? null : { text: noteText(x) } // a JSON log → details below
  if (s.step === 'publish') return { text: t('release.publishedVersion', { id: x }), mono: true }
  return { text: x, mono: true }
}
function gateDetails(s: RunStep): string {
  if (!s.externalRef || !(s.step === 'gate' || (s.step === 'smoke' && s.externalRef.startsWith('{')))) return ''
  try {
    return JSON.stringify(JSON.parse(s.externalRef), null, 2)
  } catch {
    return s.externalRef
  }
}
const targetUrl = (s: RunStep) => (s.target ? targets.value?.find((x) => x.key === s.target)?.url ?? '' : '')
const targetPlatform = (s: RunStep) => (s.target && s.step !== 'gh_asset' ? s.target.slice(0, Math.max(0, s.target.indexOf(':'))) : '')
function took(s: RunStep): string {
  if (!s.startedAt || !s.finishedAt) return ''
  return elapsed(new Date(s.finishedAt).getTime() - new Date(s.startedAt).getTime())
}
</script>

<template>
  <div class="page">
    <template v-if="state === 'loading'">
      <Skeleton height="120px" />
      <Skeleton height="320px" />
    </template>

    <EmptyState
      v-else-if="state !== 'ok' || !run"
      :icon="state === 'missing' ? 'pi pi-search' : 'pi pi-exclamation-triangle'"
      :title="state === 'missing' ? t('release.notFound') : t('release.loadError')"
      :text="state === 'missing' ? '' : errorText"
    >
      <Button
        as="router-link"
        to="/runs"
        :label="t('release.back')"
        icon="pi pi-arrow-left"
        severity="secondary"
        size="small"
      />
    </EmptyState>

    <template v-else>
      <section class="panel head">
        <div class="head-top">
          <div class="head-main">
            <div class="head-line">
              <RunBadge :state="run.state" />
              <span class="muted">{{ t('release.origin.' + run.origin) }}</span>
              <span
                v-tooltip.top="absTime(run.createdAt)"
                class="muted"
              >{{ relTime(run.createdAt) }}</span>
            </div>
            <RouterLink
              :to="{ name: 'runs', query: { project: String(run.projectId) } }"
              class="title"
            >
              <PlatformIcon
                platform="github"
                :size="16"
              />
              {{ projectName }} <span class="mono">{{ m?.fromVersion ? m.fromVersion + ' → ' : '' }}{{ run.version || m?.version }}</span>
            </RouterLink>
          </div>
          <div class="actions">
            <Button
              v-if="canResume"
              :label="t('release.actions.resume')"
              icon="pi pi-play"
              :loading="busy === 'resume'"
              :disabled="!!busy"
              @click="resume"
            />
            <Button
              v-if="canCancel"
              :label="t('release.actions.cancel')"
              icon="pi pi-stop-circle"
              severity="secondary"
              :loading="busy === 'cancel'"
              :disabled="!!busy"
              @click="cancel"
            />
          </div>
        </div>
        <Message
          v-if="run.state === 'held' && run.heldReason"
          severity="warn"
          :closable="false"
        >
          <b>{{ t('release.heldTitle') }}</b> {{ heldText(run.heldReason, targets) }}
          <div class="muted small">
            {{ t('release.heldHint') }}
          </div>
        </Message>
        <dl class="facts">
          <template v-if="m?.tag">
            <dt>{{ t('release.plan.tag') }}</dt>
            <dd class="mono">
              <a
                v-if="m.repoUrl && (run.state === 'done' || steps.some((s) => s.step === 'tag' && s.state === 'sent'))"
                :href="safeUrl(m.repoUrl + '/releases/tag/' + m.tag)"
                target="_blank"
                rel="noopener noreferrer"
              >{{ m.tag }}</a>
              <template v-else>
                {{ m.tag }}
              </template>
            </dd>
          </template>
          <template v-if="m?.folder">
            <dt>{{ t('release.plan.folder') }}</dt>
            <dd class="mono path">
              {{ m.folder }}<span
                v-if="m.branch"
                class="muted"
              > · {{ m.branch }}</span>
            </dd>
          </template>
          <template v-if="m?.head">
            <dt>{{ t('release.plan.head') }}</dt>
            <dd class="mono">
              {{ shortSha(m.head) }}<template v-if="m.bumpSha">
                → {{ shortSha(m.bumpSha) }}
              </template>
            </dd>
          </template>
          <template v-if="m?.asset">
            <dt>{{ t('release.plan.asset') }}</dt>
            <dd class="mono">
              {{ m.asset }}<span
                v-if="run.artifactSha256"
                class="muted"
              > · sha256 {{ shortSha(run.artifactSha256) }}</span>
            </dd>
          </template>
          <template v-if="targets?.length">
            <dt>{{ t('release.plan.targets') }}</dt>
            <dd class="tgts">
              <a
                v-for="tg in targets"
                :key="tg.key"
                :href="safeUrl(tg.url)"
                target="_blank"
                rel="noopener noreferrer"
                class="tgt"
              ><PlatformIcon
                :platform="tg.platform"
                :size="12"
              />{{ tg.name }}</a>
            </dd>
          </template>
          <template v-if="view?.items?.length">
            <dt>{{ t('release.items') }}</dt>
            <dd>
              <RouterLink
                v-for="it in view.items"
                :key="it.itemId"
                :to="'/item/' + it.itemId"
                class="item-link mono"
              >
                #{{ it.itemId }}
              </RouterLink>
            </dd>
          </template>
        </dl>
      </section>

      <section class="panel card">
        <div class="card-head">
          <span class="panel-title"><i class="pi pi-list-check" /> {{ t('release.timeline') }}</span>
        </div>
        <p
          v-if="!steps.length"
          class="muted"
        >
          {{ t('release.noSteps') }}
        </p>
        <ol class="timeline">
          <li
            v-for="s in steps"
            :key="s.id"
            class="tl-step"
            :class="s.state"
          >
            <i
              class="tl-icon"
              :class="STEP_ICON[s.state] ?? 'pi pi-circle'"
            />
            <div class="tl-main">
              <div class="tl-line">
                <PlatformIcon
                  v-if="targetPlatform(s)"
                  :platform="targetPlatform(s)"
                  :size="14"
                />
                <a
                  v-if="targetUrl(s)"
                  :href="safeUrl(targetUrl(s))"
                  target="_blank"
                  rel="noopener noreferrer"
                  class="tl-name"
                >{{ stepLabel(s, targets) }}</a>
                <span
                  v-else
                  class="tl-name"
                >{{ stepLabel(s, targets) }}</span>
                <span class="tl-state">{{ t('release.stepState.' + s.state) }}</span>
                <span
                  v-if="s.attempt > 1"
                  class="muted small"
                >{{ t('release.attempt', { n: s.attempt }) }}</span>
                <span
                  v-if="took(s)"
                  v-tooltip.top="absTime(s.finishedAt)"
                  class="muted small"
                >{{ took(s) }}</span>
                <Button
                  v-if="canSkip(s)"
                  :label="t('release.actions.skip')"
                  icon="pi pi-forward"
                  size="small"
                  severity="secondary"
                  text
                  class="tl-skip"
                  :loading="busy === 'skip:' + s.id"
                  :disabled="!!busy"
                  @click="skip(s)"
                />
              </div>
              <template v-if="stepRef(s)">
                <a
                  v-if="stepRef(s)?.href"
                  :href="safeUrl(stepRef(s)?.href ?? '')"
                  target="_blank"
                  rel="noopener noreferrer"
                  class="tl-ref"
                  :class="{ mono: stepRef(s)?.mono }"
                >{{ stepRef(s)?.text }}</a>
                <span
                  v-else
                  class="tl-ref"
                  :class="{ mono: stepRef(s)?.mono }"
                >{{ stepRef(s)?.text }}</span>
              </template>
              <div
                v-if="s.error"
                class="tl-error"
              >
                {{ noteText(s.error) }}
              </div>
              <details
                v-if="requestText(s.request) || gateDetails(s)"
                class="tl-details"
              >
                <summary class="muted small">
                  {{ t('release.details') }}<template v-if="s.idemKey">
                    · <span class="mono">{{ s.idemKey }}</span>
                  </template>
                </summary>
                <pre
                  v-if="requestText(s.request)"
                  class="pre"
                >{{ requestText(s.request) }}</pre>
                <pre
                  v-if="gateDetails(s)"
                  class="pre"
                >{{ gateDetails(s) }}</pre>
              </details>
            </div>
          </li>
        </ol>
      </section>

      <section
        v-if="m?.changelog"
        class="panel card"
      >
        <div class="card-head">
          <span class="panel-title"><i class="pi pi-file" /> {{ t('release.plan.changelog') }}</span>
        </div>
        <pre class="pre">{{ m.changelog }}</pre>
      </section>
    </template>
  </div>
</template>

<style scoped>
.head {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 18px 20px;
}

.head-top {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.head-main {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
}

.head-line {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  font-size: calc(13px * var(--iw-fs, 1));
}

.title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: calc(18px * var(--iw-fs, 1));
  font-weight: 600;
  color: var(--iw-text);
}

.title:hover {
  color: var(--iw-primary);
}

.actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.facts {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
  gap: 4px 14px;
  margin: 0;
  font-size: calc(13px * var(--iw-fs, 1));
}

.facts dt {
  color: var(--iw-muted);
}

.facts dd {
  margin: 0;
  min-width: 0;
}

.path {
  overflow-wrap: anywhere;
}

.tgts {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 12px;
}

.tgt {
  display: inline-flex;
  align-items: center;
  gap: 5px;
}

.item-link {
  margin-right: 8px;
}

.small {
  font-size: calc(12px * var(--iw-fs, 1));
}

.card {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 16px 20px 18px;
  min-width: 0;
}

.card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.card-head .panel-title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.timeline {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}

.tl-step {
  position: relative;
  display: flex;
  gap: 12px;
  padding: 0 0 14px;
}

.tl-step:not(:last-child)::before {
  content: '';
  position: absolute;
  left: 7px;
  top: 20px;
  bottom: 0;
  width: 2px;
  background: var(--iw-border);
}

.tl-icon {
  flex: none;
  margin-top: 3px;
  width: 16px;
  font-size: calc(15px * var(--iw-fs, 1));
  color: var(--iw-dimmed);
}

.tl-step.sent .tl-icon {
  color: var(--iw-success);
}

.tl-step.sending .tl-icon {
  color: var(--iw-primary);
}

.tl-step.failed .tl-icon {
  color: var(--iw-danger);
}

.tl-step.unknown .tl-icon {
  color: var(--iw-warn);
}

.tl-main {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
  flex: 1;
}

.tl-line {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  min-height: 24px;
}

.tl-name {
  font-weight: 500;
  color: var(--iw-text);
}

.tl-state {
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.tl-skip {
  margin-left: auto;
}

.tl-ref {
  font-size: calc(12.5px * var(--iw-fs, 1));
  overflow-wrap: anywhere;
}

.tl-error {
  font-size: calc(12.5px * var(--iw-fs, 1));
  color: var(--iw-danger);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.tl-details summary {
  cursor: pointer;
}

.pre {
  margin: 6px 0 0;
  max-height: 260px;
  overflow: auto;
  padding: 8px 10px;
  border-radius: var(--iw-radius-sm);
  background: var(--iw-elevated);
  font-family: var(--iw-mono);
  font-size: calc(12px * var(--iw-fs, 1));
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
