<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import Button from 'primevue/button'
import Select from 'primevue/select'
import Skeleton from 'primevue/skeleton'
import Textarea from 'primevue/textarea'
import Message from 'primevue/message'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import EmptyState from '../components/EmptyState.vue'
import JobBadge from '../components/JobBadge.vue'
import JobLog from '../components/JobLog.vue'
import DiffViewer from '../components/DiffViewer.vue'
import { api } from '../api/client'
import type { AgentResult, Job } from '../api/types'
import { absTime, elapsed, num, relTime, usd } from '../lib/format'
import { FLOW_ICON, isActive, jobCost, jobDuration } from '../lib/jobs'
import { useCrumbs } from '../lib/crumbs'
import { useAppStore } from '../stores/app'
import { useJobEvents, useJobsStore } from '../stores/jobs'

const props = defineProps<{ id: string }>()
const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const app = useAppStore()
const jobs = useJobsStore()

const MAX_REPLY = 65536

const job = ref<Job | null>(null)
const state = ref<'loading' | 'ok' | 'missing' | 'error'>('loading')
const errorText = ref('')
const attempt = ref(1)
const busy = ref<'' | 'cancel' | 'retry' | 'dismiss' | 'pr' | 'reply'>('')
const draft = ref('')
const draftDirty = ref(false)

async function load(quiet = false) {
  if (!quiet) state.value = 'loading'
  const r = await api.job(props.id)
  if (!r.ok) {
    state.value = r.status === 404 || r.status === 400 ? 'missing' : 'error'
    errorText.value = r.error
    return
  }
  setJob(r.data)
  state.value = 'ok'
}

function setJob(j: Job) {
  const prev = job.value
  job.value = j
  if (!prev || prev.id !== j.id || prev.attempt !== j.attempt) attempt.value = j.attempt
  if (!draftDirty.value) draft.value = j.result.draft ?? j.result.agent?.reply ?? ''
}

watch(
  () => props.id,
  () => {
    job.value = null
    draftDirty.value = false
    void load()
  },
  { immediate: true },
)
// SSE reconnect: reload what may have been missed.
watch(
  () => app.dataVersion,
  () => {
    if (app.lastChanges === null) void load(true)
  },
)
useJobEvents({
  job: (j) => {
    if (job.value && j.id === job.value.id) setJob(j)
  },
})

// Running time ticks while the job runs.
const now = ref(Date.now())
let tick: number | undefined
onMounted(() => (tick = window.setInterval(() => (now.value = Date.now()), 1000)))
onBeforeUnmount(() => window.clearInterval(tick))

useCrumbs(() => [
  { label: t('nav.jobs'), to: '/jobs' },
  job.value ? { label: `${job.value.repo}#${job.value.number}`, mono: true } : { label: '#' + props.id },
])

const res = computed(() => job.value?.result ?? {})
const current = computed(() => attempt.value === job.value?.attempt)
const attemptOptions = computed(() => Array.from({ length: job.value?.attempt ?? 1 }, (_, i) => ({ label: t('job.attemptN', { n: i + 1 }), value: i + 1 })).reverse())
const diffFiles = computed(() => res.value.diff?.files?.length ?? 0)
/** Re-fetch the diff when the attempt's result changes. */
const diffVersion = computed(() => `${job.value?.state}:${res.value.diff?.bytes ?? 0}:${res.value.diff?.files?.length ?? 0}`)
const showDiff = computed(() => job.value?.flow === 'fix' && (!current.value || !!res.value.diff || !isActive(job.value.state)))

const can = computed(() => {
  const j = job.value
  if (!j) return { cancel: false, retry: false, dismiss: false, pr: false, reply: false }
  return {
    cancel: j.state === 'queued' || j.state === 'running',
    retry: j.state === 'failed' || j.state === 'cancelled' || j.state === 'needs_review',
    dismiss: j.state === 'needs_review' || j.state === 'failed',
    pr: j.flow === 'fix' && j.state === 'needs_review',
    reply: j.flow === 'reply' && j.state === 'needs_review',
  }
})

async function run(action: 'cancel' | 'retry' | 'dismiss' | 'pr') {
  const j = job.value
  if (!j || busy.value) return
  busy.value = action
  const r = await api.jobAction(j.id, action)
  busy.value = ''
  if (!r.ok) {
    const detail = r.status === 409 ? t('job.notAllowed', { error: r.error }) : r.error
    toast.add({ severity: 'error', summary: t('job.actionFailed.' + action), detail, life: 8000 })
    void load(true) // a failed PR puts the job back to needs_review with publishError
    return
  }
  setJob(r.data)
  if (action === 'retry') draftDirty.value = false
  if (action === 'pr' && r.data.result.pr) {
    toast.add({ severity: 'success', summary: t('job.prCreated', { n: r.data.result.pr.number }), detail: r.data.result.pr.url, life: 8000 })
  }
}

function ask(action: 'dismiss' | 'pr' | 'reply') {
  const j = job.value
  if (!j) return
  confirm.require({
    header: t('job.confirm.' + action + 'Title'),
    message: t('job.confirm.' + action, { ref: `${j.repo}#${j.number}` }),
    icon: action === 'dismiss' ? 'pi pi-exclamation-triangle' : 'pi pi-question-circle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', text: true },
    acceptProps: { label: t('job.actions.' + action), severity: action === 'dismiss' ? 'danger' : undefined },
    accept: () => void (action === 'reply' ? sendReply() : run(action)),
  })
}

async function sendReply() {
  const j = job.value
  const body = draft.value.trim()
  if (!j || !body || busy.value) return
  busy.value = 'reply'
  const r = await api.jobReply(j.id, body)
  busy.value = ''
  if (!r.ok) {
    const detail = r.status === 409 ? t('item.notSignedIn') : r.error
    toast.add({ severity: 'error', summary: t('job.actionFailed.reply'), detail, life: 8000 })
    void load(true)
    return
  }
  draftDirty.value = false
  setJob(r.data)
  toast.add({ severity: 'success', summary: t('item.replyPosted'), detail: `${j.repo}#${j.number}`, life: 4000 })
}

const errorHelp = computed(() => {
  const c = res.value.errorCode
  if (!c) return null
  const to = c === 'no_folder' ? '/settings/projects' : c === 'no_cli' || c === 'no_profile' ? '/settings/agents' : ''
  const known = ['no_folder', 'no_profile', 'no_cli', 'timeout', 'agent_failed', 'git', 'interrupted'].includes(c)
  return { text: known ? t('job.errorCode.' + c) : c, to, link: to === '/settings/projects' ? t('settings.sections.projects') : to ? t('settings.sections.agents') : '' }
})

function agentStats(a: AgentResult): string[] {
  const out: string[] = []
  if (a.model) out.push(a.model)
  if (a.costUsd) out.push(usd(a.costUsd))
  if (a.turns) out.push(t('job.turns', { n: a.turns }))
  if (a.tokens) out.push(t('job.tokens', { n: num(a.tokens) }))
  if (a.durationMs) out.push(elapsed(a.durationMs))
  if (a.exitCode) out.push(t('job.exit', { code: a.exitCode }))
  return out
}
</script>

<template>
  <div class="page">
    <template v-if="state === 'loading'">
      <Skeleton height="120px" />
      <Skeleton height="320px" />
    </template>

    <EmptyState
      v-else-if="state !== 'ok' || !job"
      :icon="state === 'missing' ? 'pi pi-search' : 'pi pi-exclamation-triangle'"
      :title="state === 'missing' ? t('job.notFound') : t('job.loadError')"
      :text="state === 'missing' ? '' : errorText"
    >
      <Button
        as="router-link"
        to="/jobs"
        :label="t('job.back')"
        icon="pi pi-arrow-left"
        severity="secondary"
        size="small"
      />
    </EmptyState>

    <template v-else>
      <!-- header -->
      <section class="panel head">
        <div class="head-top">
          <div class="head-main">
            <div class="head-line">
              <JobBadge :state="job.state" />
              <span
                v-if="job.phase && isActive(job.state)"
                class="phase"
              ><i class="pi pi-spin pi-cog" /> {{ t('jobs.phase.' + job.phase) }}</span>
              <span class="flow"><i :class="FLOW_ICON[job.flow]" /> {{ t('jobs.flow.' + job.flow) }}</span>
            </div>
            <RouterLink
              :to="'/item/' + job.itemId"
              class="title"
            >
              <span class="mono ref">{{ job.repo }}#{{ job.number }}</span> {{ job.title }}
            </RouterLink>
          </div>
          <div class="actions">
            <Button
              v-if="can.cancel"
              :label="t('job.actions.cancel')"
              icon="pi pi-stop-circle"
              severity="secondary"
              :loading="busy === 'cancel'"
              :disabled="!!busy"
              @click="run('cancel')"
            />
            <span
              v-if="can.pr"
              v-tooltip.bottom="!diffFiles ? t('job.noDiffTip') : undefined"
            >
              <Button
                :label="busy === 'pr' ? t('job.creatingPr') : t('job.actions.pr')"
                icon="pi pi-github"
                :loading="busy === 'pr'"
                :disabled="!!busy || !diffFiles"
                @click="ask('pr')"
              />
            </span>
            <Button
              v-if="can.retry"
              :label="t('job.actions.retry')"
              icon="pi pi-replay"
              severity="secondary"
              :loading="busy === 'retry'"
              :disabled="!!busy"
              @click="run('retry')"
            />
            <Button
              v-if="can.dismiss"
              :label="t('job.actions.dismiss')"
              icon="pi pi-times"
              severity="danger"
              outlined
              :loading="busy === 'dismiss'"
              :disabled="!!busy"
              @click="ask('dismiss')"
            />
            <Button
              as="a"
              :href="job.itemUrl"
              target="_blank"
              rel="noopener noreferrer"
              icon="pi pi-external-link"
              severity="secondary"
              text
              :aria-label="t('item.openOnGithub')"
            />
          </div>
        </div>
        <dl class="facts">
          <div>
            <dt>{{ t('job.profile') }}</dt>
            <dd>{{ jobs.profileName(job.profileId) || '—' }}</dd>
          </div>
          <div>
            <dt>{{ t('jobs.attempt') }}</dt>
            <dd class="mono">
              {{ job.attempt }}
            </dd>
          </div>
          <div v-if="job.branch">
            <dt>{{ t('job.branch') }}</dt>
            <dd class="mono small">
              {{ job.branch }}<span
                v-if="res.baseBranch"
                class="muted"
              > ← {{ res.baseBranch }}</span>
            </dd>
          </div>
          <div>
            <dt>{{ t('job.created') }}</dt>
            <dd :title="absTime(job.createdAt)">
              {{ relTime(job.createdAt) }}
            </dd>
          </div>
          <div v-if="job.startedAt">
            <dt>{{ t('job.started') }}</dt>
            <dd :title="absTime(job.startedAt)">
              {{ relTime(job.startedAt) }}
            </dd>
          </div>
          <div v-if="job.finishedAt">
            <dt>{{ t('job.finished') }}</dt>
            <dd :title="absTime(job.finishedAt)">
              {{ relTime(job.finishedAt) }}
            </dd>
          </div>
          <div v-if="jobDuration(job, now) !== undefined">
            <dt>{{ t('job.duration') }}</dt>
            <dd>{{ elapsed(jobDuration(job, now)) }}</dd>
          </div>
          <div v-if="jobCost(job) || res.agent?.turns">
            <dt>{{ t('job.cost') }}</dt>
            <dd class="mono">
              {{ usd(jobCost(job)) || '—' }}<span
                v-if="res.agent?.turns"
                class="muted"
              > · {{ t('job.turns', { n: res.agent.turns }) }}</span>
            </dd>
          </div>
        </dl>
      </section>

      <!-- errors and publish results -->
      <Message
        v-if="job.error || errorHelp"
        severity="error"
      >
        <div class="err">
          <b v-if="errorHelp">{{ errorHelp.text }}</b>
          <span v-if="job.error">{{ job.error }}</span>
          <RouterLink
            v-if="errorHelp?.to"
            :to="errorHelp.to"
          >
            {{ errorHelp.link }} <i class="pi pi-arrow-right" />
          </RouterLink>
        </div>
      </Message>
      <Message
        v-if="res.publishError"
        severity="error"
      >
        {{ t('job.publishError', { error: res.publishError }) }}
      </Message>
      <Message
        v-if="res.cleanupError"
        severity="warn"
      >
        {{ t('job.cleanupError', { error: res.cleanupError }) }}
      </Message>
      <Message
        v-if="res.pr"
        severity="success"
      >
        {{ t('job.prOpened', { n: res.pr.number }) }}
        <a
          :href="res.pr.url"
          target="_blank"
          rel="noopener noreferrer"
        >{{ res.pr.url }}</a>
      </Message>
      <Message
        v-if="res.comment"
        severity="success"
      >
        {{ t('job.replyPosted') }}
        <a
          :href="res.comment.url"
          target="_blank"
          rel="noopener noreferrer"
        >{{ t('item.openComment') }}</a>
      </Message>

      <!-- reply draft -->
      <section
        v-if="can.reply"
        class="panel card"
      >
        <div class="card-head">
          <span class="panel-title"><i class="pi pi-comment" /> {{ t('job.draftTitle') }}</span>
          <span class="muted small">{{ t('job.draftText') }}</span>
        </div>
        <Textarea
          v-model="draft"
          auto-resize
          rows="8"
          :maxlength="MAX_REPLY"
          :aria-label="t('job.draftTitle')"
          fluid
          @input="draftDirty = true"
        />
        <div class="card-foot">
          <span class="muted mono small">{{ num(draft.length) }} / {{ num(MAX_REPLY) }}</span>
          <Button
            :label="t('job.actions.reply')"
            icon="pi pi-send"
            :loading="busy === 'reply'"
            :disabled="!draft.trim() || !!busy || !app.githubConnected"
            @click="ask('reply')"
          />
        </div>
      </section>

      <div class="grid">
        <!-- agent result -->
        <section
          v-if="res.agent"
          class="panel card"
        >
          <div class="card-head">
            <span class="panel-title"><i class="pi pi-microchip-ai" /> {{ t('job.agentTitle') }}</span>
            <span
              v-if="res.agent.status"
              class="tag"
              :class="res.agent.status === 'fixed' ? 'ok' : res.agent.status === 'partial' ? 'warn' : 'bad'"
            >{{ t('job.status.' + res.agent.status) }}</span>
          </div>
          <div class="stats muted small">
            {{ agentStats(res.agent).join(' · ') }}
          </div>
          <p
            v-if="res.agent.summary"
            class="prose"
          >
            {{ res.agent.summary }}
          </p>
          <p
            v-else-if="res.agent.final && job.flow === 'fix'"
            class="prose"
          >
            {{ res.agent.final }}
          </p>
          <details v-if="res.agent.notes">
            <summary>{{ t('job.notes') }}</summary>
            <p class="prose">
              {{ res.agent.notes }}
            </p>
          </details>
          <Message
            v-if="res.agent.error"
            severity="error"
            size="small"
          >
            {{ res.agent.error }}
          </Message>
        </section>

        <!-- verify -->
        <section
          v-if="res.verify"
          class="panel card"
        >
          <div class="card-head">
            <span class="panel-title"><i class="pi pi-verified" /> {{ t('job.verifyTitle') }}</span>
            <span
              class="tag"
              :class="res.verify.ok ? 'ok' : 'bad'"
            >{{ res.verify.timedOut ? t('job.verifyTimeout') : res.verify.ok ? t('job.verifyOk') : t('job.verifyFailed') }}</span>
          </div>
          <div class="mono small cmd">
            $ {{ res.verify.command }}
          </div>
          <div class="muted small">
            {{ t('job.exit', { code: res.verify.exitCode }) }} · {{ elapsed(res.verify.durationMs) }}
          </div>
          <details
            v-if="res.verify.output"
            :open="!res.verify.ok"
          >
            <summary>{{ t('job.output') }}</summary>
            <pre class="out mono">{{ res.verify.output }}</pre>
          </details>
        </section>

        <!-- review -->
        <section
          v-if="res.review"
          class="panel card"
        >
          <div class="card-head">
            <span class="panel-title"><i class="pi pi-eye" /> {{ t('job.reviewTitle') }}</span>
            <span
              v-if="res.review.verdict"
              class="tag"
              :class="res.review.verdict === 'ok' ? 'ok' : 'warn'"
            >{{ t('job.verdict.' + res.review.verdict) }}</span>
          </div>
          <div class="stats muted small">
            {{ agentStats(res.review).join(' · ') }}
          </div>
          <p
            v-if="res.review.summary || res.review.final"
            class="prose"
          >
            {{ res.review.summary || res.review.final }}
          </p>
          <Message
            v-if="res.review.error"
            severity="error"
            size="small"
          >
            {{ res.review.error }}
          </Message>
        </section>
      </div>

      <!-- attempt switcher for log + diff -->
      <div
        v-if="job.attempt > 1"
        class="attempts"
      >
        <span class="muted">{{ t('job.attemptShown') }}</span>
        <Select
          v-model="attempt"
          :options="attemptOptions"
          option-label="label"
          option-value="value"
          :aria-label="t('job.attemptShown')"
        />
        <span
          v-if="!current"
          class="muted small"
        >{{ t('job.oldAttempt') }}</span>
      </div>

      <section
        v-if="showDiff"
        class="panel card"
      >
        <div class="card-head">
          <span class="panel-title"><i class="pi pi-file-edit" /> {{ t('job.diff.title') }}</span>
          <span
            v-if="current && res.diff?.commits"
            class="muted small"
          >{{ t('job.diff.commits', { n: res.diff.commits }) }}</span>
        </div>
        <DiffViewer
          :job-id="job.id"
          :attempt="attempt"
          :version="current ? diffVersion : 'old'"
          :truncated="current && res.diff?.truncated"
        />
      </section>

      <section class="panel card">
        <div class="card-head">
          <span class="panel-title"><i class="pi pi-list" /> {{ t('job.log.title') }}</span>
          <span
            v-if="current && job.state === 'running'"
            class="live muted small"
          ><span class="dot" /> {{ t('common.live') }}</span>
        </div>
        <JobLog
          :job-id="job.id"
          :attempt="attempt"
        />
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
}

.phase,
.flow {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--iw-muted);
  font-size: calc(13px * var(--iw-fs, 1));
}

.title {
  font-size: calc(18px * var(--iw-fs, 1));
  font-weight: 600;
  color: var(--iw-text);
}

.title:hover {
  color: var(--iw-primary);
}

.ref {
  color: var(--iw-muted);
  font-weight: 500;
}

.actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.facts {
  display: flex;
  flex-wrap: wrap;
  gap: 10px 28px;
  margin: 0;
}

.facts dt {
  font-size: calc(11.5px * var(--iw-fs, 1));
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--iw-dimmed);
}

.facts dd {
  margin: 0;
}

.small {
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.err {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
  gap: 20px;
  align-items: start;
}

.grid:empty {
  display: none;
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
  flex-wrap: wrap;
}

.card-head .panel-title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.card-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.tag {
  padding: 1px 10px;
  border-radius: 999px;
  font-size: calc(12px * var(--iw-fs, 1));
  font-weight: 500;
}

.tag.ok {
  color: var(--iw-success);
  background: var(--iw-success-soft);
}

.tag.warn {
  color: var(--iw-warn);
  background: var(--iw-warn-soft);
}

.tag.bad {
  color: var(--iw-danger);
  background: var(--iw-danger-soft);
}

.prose {
  margin: 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  line-height: 1.6;
}

details summary {
  cursor: pointer;
  color: var(--iw-muted);
}

.cmd {
  padding: 4px 8px;
  border-radius: 6px;
  background: var(--iw-elevated);
  overflow-wrap: anywhere;
}

.out {
  max-height: 320px;
  overflow: auto;
  margin: 8px 0 0;
  padding: 8px 10px;
  border-radius: 6px;
  background: var(--iw-bg);
  border: 1px solid var(--iw-border);
  font-size: calc(12px * var(--iw-fs, 1));
  white-space: pre-wrap;
}

.attempts {
  display: flex;
  align-items: center;
  gap: 10px;
}

.live {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--iw-success);
  animation: pulse 1.4s ease-in-out infinite;
}

@keyframes pulse {
  50% {
    opacity: 0.3;
  }
}
</style>
