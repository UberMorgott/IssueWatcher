<script setup lang="ts">
import { safeUrl } from '../lib/safeUrl'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import Button from 'primevue/button'
import Skeleton from 'primevue/skeleton'
import Textarea from 'primevue/textarea'
import Message from 'primevue/message'
import MultiSelect from 'primevue/multiselect'
import { useRoute } from 'vue-router'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import EmptyState from '../components/EmptyState.vue'
import JobBadge from '../components/JobBadge.vue'
import JobLog from '../components/JobLog.vue'
import JobProgress from '../components/JobProgress.vue'
import DirectResult from '../components/DirectResult.vue'
import DiffViewer from '../components/DiffViewer.vue'
import LabelTag from '../components/LabelTag.vue'
import { api } from '../api/client'
import type { AgentResult, Job, JobAttempt, RepoLabel } from '../api/types'
import { absTime, elapsed, num, relTime, usd } from '../lib/format'
import { canPush, FLOW_ICON, isActive, isDirect, isFolderRun, jobCost, jobDuration, jobOutcome, jobRef, usePush } from '../lib/jobs'
import { useCrumbs } from '../lib/crumbs'
import { useAppStore } from '../stores/app'
import { useJobEvents, useJobsStore } from '../stores/jobs'

const props = defineProps<{ id: string }>()
const route = useRoute()
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
const busy = ref<'' | 'cancel' | 'retry' | 'dismiss' | 'pr' | 'reply' | 'labels'>('')
const draft = ref('')
const draftDirty = ref(false)
/** Label flow: the names «Добавить метки» sends (starts as the agent's picks). */
const picked = ref<string[]>([])
const pickedDirty = ref(false)

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
  if (!prev || prev.id !== j.id) attempt.value = queryAttempt(j) ?? j.attempt
  else if (prev.attempt !== j.attempt) attempt.value = j.attempt
  if (!draftDirty.value) draft.value = j.result.draft ?? j.result.agent?.reply ?? ''
  if (!pickedDirty.value) picked.value = [...(j.result.labels ?? [])]
}

/** ?attempt=N (attempt history links), when it names an attempt of j. */
function queryAttempt(j: Job): number | undefined {
  const n = Number(route.query.attempt)
  return Number.isInteger(n) && n >= 1 && n <= j.attempt ? n : undefined
}

watch(
  () => props.id,
  () => {
    job.value = null
    draftDirty.value = false
    pickedDirty.value = false
    void load()
  },
  { immediate: true },
)
watch(
  () => route.query.attempt,
  () => {
    if (job.value) attempt.value = queryAttempt(job.value) ?? job.value.attempt
  },
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
  job.value ? { label: jobRef(job.value), mono: true } : { label: '#' + props.id },
])

const res = computed(() => job.value?.result ?? {})
const current = computed(() => attempt.value === job.value?.attempt)

// Attempt history (earlier snapshots + the current one), newest first.
const attempts = ref<JobAttempt[]>([])
watch(
  () => (job.value && job.value.attempt > 1 ? `${job.value.id}:${job.value.attempt}:${job.value.state}` : ''),
  async (key) => {
    if (!key || !job.value) {
      attempts.value = []
      return
    }
    const r = await api.jobAttempts(job.value.id)
    if (r.ok) attempts.value = [...r.data].reverse()
  },
  { immediate: true },
)
function attemptNote(a: JobAttempt): string {
  const known = ['no_folder', 'no_profile', 'no_cli', 'timeout', 'agent_failed', 'git', 'interrupted']
  if (a.errorCode) return known.includes(a.errorCode) ? t('job.errorCode.' + a.errorCode) : a.errorCode
  return a.error
}

// Label flow: the repository's labels for the editor (loaded while under review).
const repoLabels = ref<RepoLabel[]>([])
const labelsError = ref('')
watch(
  () => (job.value?.flow === 'label' && job.value.state === 'needs_review' ? job.value.projectId : 0),
  async (pid) => {
    if (!pid) return
    const r = await api.projectLabels(pid)
    labelsError.value = r.ok ? '' : r.error
    if (r.ok) repoLabels.value = r.data
  },
  { immediate: true },
)
const labelOptions = computed(() => {
  const names = repoLabels.value.map((l) => l.name)
  for (const p of picked.value) if (!names.includes(p)) names.push(p)
  return names.map((n) => ({ label: n, value: n }))
})
const diffFiles = computed(() => res.value.diff?.files?.length ?? 0)
/** Re-fetch the diff when the attempt's result changes. */
const diffVersion = computed(() => `${job.value?.state}:${res.value.diff?.bytes ?? 0}:${res.value.diff?.files?.length ?? 0}`)
const showDiff = computed(() => job.value?.flow === 'fix' && !isFolderRun(job.value) && (!current.value || !!res.value.diff || !isActive(job.value.state)))

const can = computed(() => {
  const j = job.value
  if (!j) return { cancel: false, retry: false, dismiss: false, pr: false, push: false, reply: false, labels: false }
  // A folder run that ended «Изменено в папке» is done but unpublished: it can be rerun or dropped.
  const folderDone = j.state === 'done' && isFolderRun(j)
  return {
    labels: j.flow === 'label' && j.state === 'needs_review',
    cancel: j.state === 'queued' || j.state === 'running',
    retry: j.state === 'failed' || j.state === 'cancelled' || j.state === 'needs_review' || folderDone,
    dismiss: j.state === 'needs_review' || j.state === 'failed' || folderDone,
    pr: j.flow === 'fix' && j.state === 'needs_review' && !isDirect(j),
    push: canPush(j),
    reply: j.flow === 'reply' && j.state === 'needs_review',
  }
})
const direct = computed(() => !!job.value && isDirect(job.value))
const logOpen = ref(false)

const pusher = usePush()
async function push() {
  const j = job.value
  if (!j || busy.value) return
  const r = await pusher.push(j)
  if (r) setJob(r)
  else void load(true) // a failed push puts the job back to needs_review with publishError
}

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

function ask(action: 'dismiss' | 'pr' | 'reply' | 'labels') {
  const j = job.value
  if (!j) return
  confirm.require({
    header: t('job.confirm.' + action + 'Title'),
    message: t('job.confirm.' + (action !== 'dismiss' ? action : isFolderRun(j) ? 'dismissFolder' : isDirect(j) ? 'dismissDirect' : action), { ref: `${j.repo}#${j.number}`, labels: picked.value.join(', ') }),
    icon: action === 'dismiss' ? 'pi pi-exclamation-triangle' : 'pi pi-question-circle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', text: true },
    acceptProps: { label: t('job.actions.' + action), severity: action === 'dismiss' ? 'danger' : undefined },
    accept: () => void (action === 'reply' ? sendReply() : action === 'labels' ? applyLabels() : run(action)),
  })
}

async function applyLabels() {
  const j = job.value
  if (!j || !picked.value.length || busy.value) return
  busy.value = 'labels'
  const r = await api.jobLabels(j.id, picked.value)
  busy.value = ''
  if (!r.ok) {
    const detail = r.status === 409 ? t('job.notAllowed', { error: r.error }) : r.error
    toast.add({ severity: 'error', summary: t('job.actionFailed.labels'), detail, life: 8000 })
    void load(true)
    return
  }
  pickedDirty.value = false
  setJob(r.data)
  const n = r.data.result.appliedLabels?.length ?? 0
  toast.add({ severity: 'success', summary: n ? t('job.labels.added', n) : t('job.labels.nothingNew'), detail: `${j.repo}#${j.number}`, life: 5000 })
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
              <JobBadge
                :state="job.state"
                :outcome="jobOutcome(job)"
              />
              <span
                v-if="job.phase && isActive(job.state) && job.state !== 'running'"
                class="phase"
              ><i class="pi pi-spin pi-cog" /> {{ t('jobs.phase.' + job.phase) }}</span>
              <span class="flow"><i :class="FLOW_ICON[job.flow]" /> {{ t('jobs.flow.' + job.flow) }}<template v-if="direct"> · {{ t(isFolderRun(job) ? 'jobs.folderMode' : 'jobs.directMode') }}</template></span>
              <span
                v-if="job.origin === 'rule'"
                class="flow"
                :title="job.ruleId"
              ><i class="pi pi-bolt" /> {{ t('jobs.origin.rule') }}</span>
            </div>
            <JobProgress
              v-if="job.state === 'running'"
              :id="job.id"
              :attempt="job.attempt"
              :started-at="job.startedAt"
              :phase="job.phase"
              class="progress"
            />
            <RouterLink
              v-if="job.itemId"
              :to="'/item/' + job.itemId"
              class="title"
            >
              <span class="mono ref">{{ job.repo }}#{{ job.number }}</span> {{ job.title }}
            </RouterLink>
            <RouterLink
              v-else
              :to="'/issues?repo=' + job.projectId"
              class="title"
            >
              <span class="mono ref">{{ job.repo }}</span> {{ t('job.triage.project') }}
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
            <Button
              v-if="can.push"
              :label="t('job.actions.push')"
              icon="pi pi-upload"
              :loading="pusher.busy.value"
              :disabled="!!busy || pusher.busy.value"
              @click="push"
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
              :href="safeUrl(job.itemUrl)"
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
            <dt>{{ t('jobs.originLabel') }}</dt>
            <dd>
              {{ t('jobs.origin.' + job.origin) }}<span
                v-if="job.ruleId"
                class="mono muted small"
              > · {{ job.ruleId }}</span>
            </dd>
          </div>
          <div>
            <dt>{{ t('jobs.attempt') }}</dt>
            <dd class="mono">
              {{ job.attempt }}
            </dd>
          </div>
          <div v-if="direct && job.localPath">
            <dt>{{ t('job.folder') }}</dt>
            <dd class="mono small">
              {{ job.localPath }}
            </dd>
          </div>
          <div v-if="job.branch || res.local?.branch">
            <dt>{{ t('job.branch') }}</dt>
            <dd class="mono small">
              {{ job.branch || res.local?.branch }}<span
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
          :href="safeUrl(res.pr.url)"
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
          :href="safeUrl(res.comment.url)"
          target="_blank"
          rel="noopener noreferrer"
        >{{ t('item.openComment') }}</a>
      </Message>

      <!-- direct fix: outcome, commits in the mapped folder, warnings -->
      <section
        v-if="direct && (res.local || job.state === 'failed')"
        class="panel card"
      >
        <div class="card-head">
          <span class="panel-title"><i class="pi pi-folder" /> {{ t('job.direct.title') }}</span>
          <span
            v-if="res.local?.startSha && res.local.headSha"
            class="muted small mono"
          >{{ res.local.startSha.slice(0, 7) }}..{{ res.local.headSha.slice(0, 7) }}</span>
        </div>
        <DirectResult :job="job" />
      </section>

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

      <!-- triage: the checked ranking and the fix jobs it queued -->
      <section
        v-if="job.flow === 'triage' && res.triage"
        class="panel card"
      >
        <div class="card-head">
          <span class="panel-title"><i :class="FLOW_ICON.triage" /> {{ t('job.triage.title') }}</span>
          <span class="muted small">{{ t('job.triage.text', { open: res.triage.open, n: res.triage.topN }) }}</span>
        </div>
        <p
          v-if="res.triage.summary"
          class="triage-summary"
        >
          {{ res.triage.summary }}
        </p>
        <span
          v-if="!res.triage.picks.length"
          class="muted"
        >{{ t('job.triage.none') }}</span>
        <ol
          v-else
          class="triage-picks"
        >
          <li
            v-for="p in res.triage.picks"
            :key="p.number"
          >
            <span
              class="sev"
              :class="p.severity || 'unknown'"
            >{{ t('job.triage.severity.' + (p.severity || 'unknown')) }}</span>
            <RouterLink
              :to="'/item/' + p.itemId"
              class="pick-title"
            >
              <span class="mono">#{{ p.number }}</span> {{ p.title }}
            </RouterLink>
            <RouterLink
              v-if="p.jobId"
              :to="'/jobs/' + p.jobId"
              class="small pick-queue"
              :class="p.queue"
            >
              {{ t('job.triage.' + (p.queue === 'queued' ? 'queued' : 'exists')) }} · #{{ p.jobId }}
            </RouterLink>
            <span
              v-else-if="p.queue"
              class="small pick-queue err"
            >{{ p.queue }}</span>
            <span
              v-else
              class="muted small pick-queue"
            >{{ t('job.triage.below', { n: res.triage.topN }) }}</span>
            <span class="muted small pick-reason">{{ p.reason }}</span>
          </li>
        </ol>
        <div
          v-if="res.triage.more"
          class="muted small"
        >
          {{ t('job.triage.more') }}
        </div>
        <div
          v-if="res.triage.dropped?.length"
          class="muted small"
        >
          {{ t('job.triage.dropped', { list: res.triage.dropped.map((d) => '#' + d.number).join(', ') }) }}
        </div>
      </section>

      <!-- label flow: suggestions → «Добавить метки» (add only), or what was added -->
      <section
        v-if="job.flow === 'label' && (res.labels || res.appliedLabels)"
        class="panel card"
      >
        <div class="card-head">
          <span class="panel-title"><i :class="FLOW_ICON.label" /> {{ t('job.labels.title') }}</span>
          <span class="muted small">{{ can.labels ? t('job.labels.text') : '' }}</span>
        </div>
        <template v-if="can.labels">
          <MultiSelect
            v-model="picked"
            :options="labelOptions"
            option-label="label"
            option-value="value"
            filter
            display="chip"
            :placeholder="t('job.labels.none')"
            :aria-label="t('job.labels.title')"
            fluid
            @update:model-value="pickedDirty = true"
          />
          <Message
            v-if="labelsError"
            severity="warn"
            size="small"
          >
            {{ t('job.labels.loadError', { error: labelsError }) }}
          </Message>
          <div
            v-if="res.droppedLabels?.length"
            class="muted small"
          >
            {{ t('job.labels.dropped', { labels: res.droppedLabels.join(', ') }) }}
          </div>
          <div class="card-foot">
            <span class="muted small">{{ t('job.labels.addOnly') }}</span>
            <Button
              :label="t('job.actions.labels')"
              :icon="FLOW_ICON.label"
              :loading="busy === 'labels'"
              :disabled="!picked.length || !!busy || !app.githubConnected"
              @click="ask('labels')"
            />
          </div>
        </template>
        <template v-else>
          <div
            v-if="res.labels?.length"
            class="chips"
          >
            <span class="muted small">{{ t('job.labels.suggested') }}</span>
            <LabelTag
              v-for="l in res.labels"
              :key="l"
              :name="l"
            />
          </div>
          <span
            v-else
            class="muted"
          >{{ t('job.labels.none') }}</span>
          <div
            v-if="job.state === 'done'"
            class="chips"
          >
            <span class="muted small">{{ t('job.labels.applied') }}</span>
            <LabelTag
              v-for="l in res.appliedLabels ?? []"
              :key="l"
              :name="l"
            />
            <span
              v-if="!res.appliedLabels?.length"
              class="muted small"
            >{{ t('job.labels.nothingNew') }}</span>
          </div>
        </template>
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
            v-else-if="res.agent.final && job.flow !== 'reply'"
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
          <details v-if="res.agent.verify">
            <summary>{{ t('job.agentVerify') }}</summary>
            <p class="prose">
              {{ res.agent.verify }}
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

      <!-- attempt history: each links its log + diff (?attempt=) -->
      <section
        v-if="job.attempt > 1"
        class="panel card"
      >
        <div class="card-head">
          <span class="panel-title"><i class="pi pi-history" /> {{ t('job.attemptsTitle') }}</span>
          <span
            v-if="!current"
            class="muted small"
          >{{ t('job.oldAttempt') }}</span>
        </div>
        <ul class="attempts">
          <li
            v-for="a in attempts"
            :key="a.attempt"
          >
            <RouterLink
              :to="{ query: { ...route.query, attempt: String(a.attempt) } }"
              class="attempt"
              :class="{ on: a.attempt === attempt }"
              :aria-current="a.attempt === attempt ? 'true' : undefined"
            >
              <JobBadge
                :state="a.state"
                compact
              />
              <span>{{ t('job.attemptN', { n: a.attempt }) }}</span>
              <span
                v-if="a.finishedAt || a.startedAt"
                class="muted small"
                :title="absTime(a.finishedAt || a.startedAt)"
              >{{ relTime(a.finishedAt || a.startedAt) }}</span>
              <span
                v-if="attemptNote(a)"
                class="muted small note"
              >{{ attemptNote(a) }}</span>
            </RouterLink>
          </li>
        </ul>
      </section>

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
          <span class="log-head">
            <span
              v-if="current && job.state === 'running'"
              class="live muted small"
            ><span class="dot" /> {{ t('common.live') }}</span>
            <Button
              :label="logOpen ? t('job.log.hide') : t('job.log.show')"
              :icon="logOpen ? 'pi pi-chevron-up' : 'pi pi-chevron-down'"
              size="small"
              severity="secondary"
              text
              :aria-expanded="logOpen"
              @click="logOpen = !logOpen"
            />
          </span>
        </div>
        <JobLog
          v-if="logOpen"
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
  flex-direction: column;
  gap: 2px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.attempt {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 8px;
  border-radius: 6px;
  color: var(--iw-text);
  min-width: 0;
}

.attempt:hover,
.attempt.on {
  background: var(--iw-elevated);
}

.attempt .note {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}

.chips {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.triage-summary {
  margin: 0;
}

.triage-picks {
  margin: 0;
  padding-left: 22px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.triage-picks li {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 4px 10px;
}

.pick-title {
  color: var(--iw-text);
  text-decoration: none;
  min-width: 0;
}

.pick-title:hover {
  color: var(--iw-primary);
}

.pick-reason {
  flex-basis: 100%;
}

.pick-queue {
  text-decoration: none;
}

.pick-queue.queued {
  color: var(--iw-success);
}

.pick-queue.exists {
  color: var(--iw-warn);
}

.sev {
  font-size: calc(11.5px * var(--iw-fs, 1));
  padding: 1px 7px;
  border-radius: 999px;
  color: var(--iw-muted);
  background: var(--iw-elevated);
}

.sev.critical {
  color: var(--iw-danger);
  background: var(--iw-danger-soft);
}

.sev.high {
  color: var(--iw-warn);
  background: var(--iw-warn-soft);
}

.sev.medium {
  color: var(--iw-primary);
}

.progress {
  font-size: calc(13px * var(--iw-fs, 1));
}

.log-head {
  display: inline-flex;
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
