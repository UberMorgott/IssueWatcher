<script lang="ts">
import { reactive } from 'vue'
import type { PublishTask } from '../api/types'

/** Running / last task per project, module-wide: closing (or re-mounting) the dialog never loses a running publish. */
const tasks = reactive<Record<number, PublishTask>>({})
</script>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Textarea from 'primevue/textarea'
import Select from 'primevue/select'
import RadioButton from 'primevue/radiobutton'
import Checkbox from 'primevue/checkbox'
import Message from 'primevue/message'
import ProgressBar from 'primevue/progressbar'
import Skeleton from 'primevue/skeleton'
import { useI18n } from 'vue-i18n'
import { api, type Result } from '../api/client'
import type { PublishCategory, PublishFile, PublishRequest, PublishResult, PublishTarget, PublishTargets } from '../api/types'
import { useAppStore } from '../stores/app'
import { absTime, relTime } from '../lib/format'
import { safeUrl } from '../lib/safeUrl'

// «Опубликовать версию» (Nexus, Phase 7): pick the target file (or a new
// file), the archive (native file dialog), version / name / category / flags /
// changelog → «Проверить» runs the dry run (POST …/publish {dryRun}: the planned
// requests, md5, size; nothing is written) → «Загрузить» starts the publish
// (202 task) and follows it live (SSE publish.progress, a slow poll as a net).
// A failed publish that kept its upload id retries with {uploadId} and no path:
// the archive is never uploaded twice. The version POST itself is sent once by
// the server and never re-sent behind the user's back.
const props = defineProps<{ target: PublishTarget | null }>()
const visible = defineModel<boolean>('visible', { required: true })
const { t, te } = useI18n()
const app = useAppStore()

type Step = 'form' | 'summary' | 'run'
const step = ref<Step>('form')
const targets = ref<PublishTargets | null>(null)
const loading = ref(false)
const busy = ref(false)
/** Error of the current screen: a code (own text + link) or a plain text. */
const error = ref<{ code?: string; text: string; detail?: string } | null>(null)
const plan = ref<PublishResult | null>(null)
const pickerAvailable = ref(true)
const picking = ref(false)

const NEW = '__new__'
const form = reactive({
  fileId: '',
  path: '',
  version: '',
  name: '',
  category: 'main' as PublishCategory,
  description: '',
  archivePrevious: true,
  updateModVersion: true,
  changelog: '',
})
/** The request of the last start (a retry resends it with uploadId instead of path). */
let lastReq: PublishRequest | null = null

const categories = computed(() =>
  (['main', 'optional', 'miscellaneous'] as PublishCategory[]).map((c) => ({ value: c, label: t('publish.category.' + c) })),
)
const newFile = computed(() => form.fileId === NEW)
const file = computed<PublishFile | undefined>(() => targets.value?.files.find((f) => f.id === form.fileId))
const task = computed(() => (props.target ? tasks[props.target.projectId] : undefined))

/** Newest first. */
const byDate = (vs: PublishFile['versions']) => [...vs].sort((a, b) => (b.uploadedAt ?? '').localeCompare(a.uploadedAt ?? ''))
/** The file's current version: its newest upload. */
const latestOf = (f?: PublishFile) => (f ? byDate(f.versions)[0] : undefined)
const latestAny = computed(() => byDate((targets.value?.files ?? []).flatMap((f) => f.versions))[0])
const current = computed(() => (newFile.value ? latestAny.value : latestOf(file.value)))

/** The version bumped by patch: 1.2.3 → 1.2.4; else the last number +1 (v0.9 → v0.10); none → "". */
function bumpPatch(v: string): string {
  const s = v.trim()
  const sem = /^(\d+)\.(\d+)\.(\d+)(.*)$/.exec(s)
  if (sem) return `${sem[1]}.${sem[2]}.${Number(sem[3]) + 1}`
  const m = /(\d+)(\D*)$/.exec(s)
  return m ? s.slice(0, m.index) + String(Number(m[1]) + 1) + m[2] : ''
}

/** Fills version / name / category from the picked target (only fields the user did not type in). */
function applyDefaults() {
  const cur = current.value
  form.version = cur ? bumpPatch(cur.version) : ''
  if (newFile.value) {
    form.name = ''
    form.category = 'optional'
  } else {
    form.name = cur?.name || file.value?.name || ''
    const c = cur?.category as PublishCategory | undefined
    form.category = c && ['main', 'optional', 'miscellaneous'].includes(c) ? c : 'main'
  }
}
watch(() => form.fileId, () => {
  if (step.value === 'form') applyDefaults()
})

watch(visible, (v) => {
  if (!v || !props.target) return
  error.value = null
  plan.value = null
  // A publish of this project is running or just ended: show it.
  step.value = task.value ? 'run' : 'form'
  if (step.value === 'form') resetForm()
  void loadTargets()
})

function resetForm() {
  form.fileId = ''
  form.path = ''
  form.description = ''
  form.changelog = ''
  form.archivePrevious = true
  form.updateModVersion = true
}

async function loadTargets() {
  const p = props.target
  if (!p) return
  loading.value = true
  const r = await api.publishTargets(p.projectId)
  loading.value = false
  if (props.target?.projectId !== p.projectId) return
  if (!r.ok) {
    targets.value = null
    error.value = failure(r)
    return
  }
  targets.value = r.data
  if (step.value === 'form' && !form.fileId) {
    // Default target: the active file with the newest upload; no file yet → a new file.
    const files = [...r.data.files].sort((a, b) => Number(b.active) - Number(a.active) || (b.lastUploadedAt ?? '').localeCompare(a.lastUploadedAt ?? ''))
    form.fileId = files[0]?.id ?? NEW
    applyDefaults()
  }
}

/** The error of a failed call: a known code gets its own text (and the settings link for the key). */
function failure(r: Extract<Result<unknown>, { ok: false }>) {
  const code = (r.body as { code?: unknown } | undefined)?.code
  if (typeof code === 'string' && code !== 'bad_request' && te('publish.errors.' + code)) return { code, text: t('publish.errors.' + code), detail: r.error }
  if (code === 'bad_request') return { code, text: t('publish.errors.bad_request', { reason: r.error }) }
  return { text: r.status === 0 ? r.error : t('publish.errors.failed'), detail: r.error }
}
const keyProblem = computed(() => error.value?.code === 'no_api_key' || error.value?.code === 'bad_api_key')

async function browse() {
  if (picking.value) return
  picking.value = true
  error.value = null
  const initial = form.path.trim() || props.target?.folder || undefined
  const r = await api.pickFile({ title: t('publish.pickTitle', { name: props.target?.name ?? '' }), initial })
  picking.value = false
  if (!r.ok) {
    if ((r.body as { code?: string } | undefined)?.code === 'unavailable') pickerAvailable.value = false
    else error.value = { text: r.error }
    return
  }
  if (!r.data.cancelled && r.data.path) form.path = r.data.path
}

const fileName = (p: string) => p.split(/[\\/]/).pop() ?? p
function request(): PublishRequest {
  const req: PublishRequest = {
    path: form.path.trim(),
    name: form.name.trim(),
    version: form.version.trim(),
    category: form.category,
    updateModVersion: form.updateModVersion,
  }
  if (newFile.value) req.newFile = true
  else {
    req.fileId = form.fileId
    req.archivePrevious = form.archivePrevious
  }
  if (form.description.trim()) req.description = form.description.trim()
  if (form.changelog.trim()) req.changelog = form.changelog.trim()
  return req
}
const formReady = computed(() => !!form.fileId && !!form.path.trim() && !!form.name.trim() && !!form.version.trim() && !loading.value)

/** «Проверить»: the dry run (no writes) → the summary screen. */
async function review() {
  const p = props.target
  if (!p || !formReady.value || busy.value) return
  busy.value = true
  error.value = null
  const r = await api.publishPlan(p.projectId, request())
  busy.value = false
  if (!r.ok) {
    error.value = failure(r)
    return
  }
  plan.value = r.data
  step.value = 'summary'
}

/** «Загрузить» (or a retry with the kept upload id): starts the publish task. */
async function start(req: PublishRequest) {
  const p = props.target
  if (!p || busy.value) return
  busy.value = true
  error.value = null
  lastReq = req
  const r = await api.publish(p.projectId, req)
  busy.value = false
  if (!r.ok) {
    const body = r.body as { code?: string; task?: PublishTask } | undefined
    if (body?.code === 'busy' && body.task?.id) {
      // Another publish of this project runs: follow it instead.
      tasks[p.projectId] = body.task
      step.value = 'run'
      follow(body.task.id)
      return
    }
    error.value = failure(r)
    return
  }
  tasks[p.projectId] = r.data
  step.value = 'run'
  follow(r.data.id)
}
const upload = () => void start(request())
function retryUpload() {
  const id = task.value?.uploadId
  if (!id || !lastReq) return
  const req: PublishRequest = { ...lastReq, uploadId: id }
  delete req.path // the archive is on Nexus already: never uploaded twice
  void start(req)
}

// Live progress: SSE publish.progress for this task; a slow poll covers a lost event / reconnect.
watch(
  () => app.lastPublish,
  (e) => {
    if (!e) return
    const cur = tasks[e.projectId]
    if (cur && cur.id === e.id) tasks[e.projectId] = e
  },
  { flush: 'sync' },
)
let pollTimer: number | undefined
function follow(id: string) {
  window.clearTimeout(pollTimer)
  pollTimer = window.setTimeout(async () => {
    const r = await api.publishTask(id)
    if (r.ok) tasks[r.data.projectId] = r.data
    if (!r.ok || r.data.state === 'running') follow(id)
  }, 3000)
}
watch(
  () => task.value?.state,
  (s) => {
    if (s && s !== 'running') window.clearTimeout(pollTimer)
  },
)

async function cancelRun() {
  const id = task.value?.id
  if (id) await api.cancelPublish(id)
}
/** Back to the form after an ended run (the finished task is forgotten server-side). */
function again() {
  const p = props.target
  const tk = task.value
  if (!p || !tk || tk.state === 'running') return
  void api.cancelPublish(tk.id)
  delete tasks[p.projectId]
  error.value = null
  plan.value = null
  step.value = 'form'
  if (tk.state === 'done') {
    resetForm()
    void loadTargets()
  }
}

const STAGES = ['resolve', 'hash', 'upload', 'wait', 'publish', 'changelog'] as const
const stageIndex = computed(() => STAGES.indexOf((task.value?.stage ?? 'resolve') as (typeof STAGES)[number]))
const percent = computed(() => {
  const tk = task.value
  if (!tk?.total) return 0
  return Math.min(100, Math.round(((tk.sent ?? 0) / tk.total) * 100))
})
/** Cancel is offered until the publish call itself (the server never aborts it once sent). */
const cancellable = computed(() => task.value?.state === 'running' && stageIndex.value < STAGES.indexOf('publish'))

function bytes(n?: number): string {
  if (!n) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i ? 1 : 0)} ${u[i]}`
}
const filesUrl = computed(() => task.value?.result?.filesUrl || targets.value?.filesUrl || '')
const changelogFailed = computed(() => task.value?.result?.changelog?.startsWith('failed') ?? false)
const pretty = (b: unknown) => JSON.stringify(b, null, 2)
const runError = computed(() => {
  const tk = task.value
  if (!tk || (tk.state !== 'failed' && tk.state !== 'cancelled')) return ''
  return t(te('publish.errors.' + (tk.errorCode ?? '')) ? 'publish.errors.' + tk.errorCode : 'publish.errors.failed')
})
</script>

<template>
  <Dialog
    v-model:visible="visible"
    :header="t('publish.title', { name: target?.name ?? '' })"
    modal
    :dismissable-mask="step !== 'run'"
    :style="{ width: '680px' }"
    :breakpoints="{ '720px': '94vw' }"
  >
    <!-- 1. The form -->
    <div
      v-if="step === 'form'"
      class="form"
    >
      <span class="sub">{{ t('publish.target') }}</span>
      <div
        v-if="loading && !targets"
        class="list"
      >
        <Skeleton
          v-for="i in 2"
          :key="i"
          height="44px"
        />
      </div>
      <div
        v-else-if="targets"
        class="list"
        role="radiogroup"
        :aria-label="t('publish.target')"
      >
        <label
          v-for="f in targets.files"
          :key="f.id"
          class="opt"
          :class="{ on: form.fileId === f.id }"
        >
          <RadioButton
            v-model="form.fileId"
            :value="f.id"
            name="file"
          />
          <span class="o-main">
            <span class="o-name">{{ f.name }}<span
              v-if="!f.active"
              class="tag muted"
            > · {{ t('publish.inactive') }}</span></span>
            <span class="o-meta muted">
              <template v-if="latestOf(f)">{{ t('publish.current', { version: latestOf(f)?.version }) }} · </template>
              {{ t('publish.versions', f.versionsCount) }}<template v-if="f.lastUploadedAt">
                · <span v-tooltip.top="absTime(f.lastUploadedAt)">{{ relTime(f.lastUploadedAt) }}</span></template>
            </span>
          </span>
        </label>
        <label
          class="opt"
          :class="{ on: newFile }"
        >
          <RadioButton
            v-model="form.fileId"
            :value="NEW"
            name="file"
          />
          <span class="o-main">
            <span class="o-name"><i class="pi pi-plus" /> {{ t('publish.newFile') }}</span>
            <span class="o-meta muted">{{ t('publish.newFileHint') }}</span>
          </span>
        </label>
      </div>

      <label class="field">
        <span class="label">{{ t('publish.archive') }}</span>
        <div class="row">
          <InputText
            v-model="form.path"
            class="mono"
            :placeholder="t('publish.archivePlaceholder')"
            fluid
          />
          <Button
            v-if="pickerAvailable"
            :label="t('publish.browse')"
            icon="pi pi-file"
            severity="secondary"
            outlined
            :loading="picking"
            @click="browse"
          />
        </div>
        <small
          v-if="!pickerAvailable"
          class="muted"
        >{{ t('publish.pickerUnavailable') }}</small>
      </label>

      <div class="grid">
        <label class="field">
          <span class="label">{{ t('publish.version') }}</span>
          <InputText
            v-model="form.version"
            class="mono"
            maxlength="50"
            fluid
          />
          <small
            v-if="current"
            class="muted"
          >{{ t('publish.versionHint', { version: current.version }) }}</small>
        </label>
        <label class="field">
          <span class="label">{{ t('publish.category.label') }}</span>
          <Select
            v-model="form.category"
            :options="categories"
            option-label="label"
            option-value="value"
            fluid
          />
        </label>
      </div>
      <label class="field">
        <span class="label">{{ t('publish.name') }}</span>
        <InputText
          v-model="form.name"
          maxlength="50"
          fluid
        />
        <small class="muted">{{ t('publish.nameHint') }}</small>
      </label>
      <label class="field">
        <span class="label">{{ t('publish.description') }}</span>
        <InputText
          v-model="form.description"
          fluid
        />
      </label>
      <label class="field">
        <span class="label">{{ t('publish.changelog') }}</span>
        <Textarea
          v-model="form.changelog"
          rows="4"
          auto-resize
          fluid
          :placeholder="t('publish.changelogPlaceholder')"
        />
      </label>
      <div class="checks">
        <label
          v-if="!newFile"
          class="check"
        >
          <Checkbox
            v-model="form.archivePrevious"
            binary
          />
          {{ t('publish.archivePrevious') }}
        </label>
        <label class="check">
          <Checkbox
            v-model="form.updateModVersion"
            binary
          />
          {{ t('publish.updateModVersion') }}
        </label>
      </div>
    </div>

    <!-- 2. The summary: what the dry run plans (nothing written yet) -->
    <div
      v-else-if="step === 'summary' && plan"
      class="form"
    >
      <p class="hint">
        {{ t('publish.summaryText') }}
      </p>
      <dl class="facts">
        <dt>{{ t('publish.target') }}</dt>
        <dd>{{ newFile ? t('publish.newFile') : file?.name }}</dd>
        <dt>{{ t('publish.version') }}</dt>
        <dd class="mono">
          {{ current ? current.version + ' → ' : '' }}<b>{{ form.version }}</b>
        </dd>
        <dt>{{ t('publish.name') }}</dt>
        <dd>{{ form.name }} · {{ t('publish.category.' + form.category) }}</dd>
        <dt>{{ t('publish.archive') }}</dt>
        <dd>
          <span
            v-tooltip.top="form.path"
            class="mono"
          >{{ fileName(form.path) }}</span> · {{ bytes(plan.size) }}
        </dd>
        <dt>MD5</dt>
        <dd class="mono small">
          {{ plan.md5 }}
        </dd>
        <dt>{{ t('publish.options') }}</dt>
        <dd>
          <span v-if="!newFile">{{ t(form.archivePrevious ? 'publish.archivePreviousOn' : 'publish.archivePreviousOff') }} · </span>
          {{ t(form.updateModVersion ? 'publish.updateModVersionOn' : 'publish.updateModVersionOff') }}
          <template v-if="form.changelog.trim()">
            · {{ t('publish.withChangelog') }}
          </template>
        </dd>
      </dl>
      <details class="plan">
        <summary>{{ t('publish.plan', plan.plan?.length ?? 0) }}</summary>
        <ol>
          <li
            v-for="(s, i) in plan.plan ?? []"
            :key="i"
          >
            <span class="mono"><b>{{ s.method }}</b> {{ s.url }}</span>
            <span
              v-if="s.note"
              class="muted"
            > — {{ s.note }}</span>
            <pre
              v-if="s.body !== undefined"
              class="mono"
            >{{ pretty(s.body) }}</pre>
          </li>
        </ol>
      </details>
      <Message
        severity="warn"
        size="small"
        variant="simple"
      >
        {{ t('publish.liveWarning') }}
      </Message>
    </div>

    <!-- 3. The run: live progress → success / failure -->
    <div
      v-else-if="step === 'run' && task"
      class="form"
    >
      <template v-if="task.state === 'running'">
        <ol class="stages">
          <li
            v-for="(s, i) in STAGES"
            :key="s"
            :class="{ done: i < stageIndex, now: i === stageIndex }"
          >
            <i :class="i < stageIndex ? 'pi pi-check' : i === stageIndex ? 'pi pi-spin pi-spinner' : 'pi pi-circle'" />
            {{ t('publish.stage.' + s) }}
          </li>
        </ol>
        <div
          v-if="task.stage === 'upload' && task.total"
          class="bytes"
        >
          <ProgressBar
            :value="percent"
            :show-value="false"
            style="height: 8px"
          />
          <span class="muted mono small">{{ bytes(task.sent) }} / {{ bytes(task.total) }} · {{ percent }}%</span>
        </div>
        <p class="hint">
          {{ t('publish.runningHint', { version: task.version }) }}
        </p>
      </template>
      <template v-else-if="task.state === 'done'">
        <Message
          severity="success"
          size="small"
        >
          {{ t('publish.done', { version: task.version }) }}
        </Message>
        <Message
          v-if="changelogFailed"
          v-tooltip.top="task.result?.changelog"
          severity="warn"
          size="small"
          variant="simple"
        >
          {{ t('publish.changelogFailed') }}
        </Message>
        <Button
          v-if="filesUrl"
          as="a"
          :href="safeUrl(filesUrl)"
          target="_blank"
          rel="noopener noreferrer"
          :label="t('publish.openFiles')"
          icon="pi pi-external-link"
          severity="secondary"
          outlined
          class="self-start"
        />
      </template>
      <template v-else>
        <Message
          v-tooltip.top="task.error"
          :severity="task.state === 'cancelled' ? 'warn' : 'error'"
          size="small"
        >
          {{ runError }}
          <RouterLink
            v-if="task.errorCode === 'no_api_key' || task.errorCode === 'bad_api_key'"
            to="/connections"
            @click="visible = false"
          >
            {{ t('publish.toSettings') }}
          </RouterLink>
        </Message>
        <p
          v-if="task.uploadId"
          class="hint"
        >
          {{ t('publish.uploadKept') }}
        </p>
        <p
          v-if="task.state === 'failed' && task.errorCode === 'publish_failed'"
          class="hint"
        >
          {{ t('publish.checkFirst') }}
          <a
            v-if="filesUrl"
            :href="safeUrl(filesUrl)"
            target="_blank"
            rel="noopener noreferrer"
          >{{ t('publish.openFiles') }} <i class="pi pi-external-link" /></a>
        </p>
      </template>
    </div>

    <Message
      v-if="error"
      v-tooltip.top="error.detail || undefined"
      severity="error"
      size="small"
      variant="simple"
      class="err"
    >
      {{ error.text }}
      <RouterLink
        v-if="keyProblem"
        to="/connections"
        @click="visible = false"
      >
        {{ t('publish.toSettings') }}
      </RouterLink>
    </Message>

    <template #footer>
      <template v-if="step === 'form'">
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          text
          @click="visible = false"
        />
        <Button
          :label="t('publish.review')"
          icon="pi pi-list-check"
          :loading="busy"
          :disabled="!formReady"
          @click="review"
        />
      </template>
      <template v-else-if="step === 'summary'">
        <Button
          :label="t('publish.back')"
          icon="pi pi-arrow-left"
          severity="secondary"
          text
          :disabled="busy"
          @click="step = 'form'"
        />
        <Button
          :label="t('publish.upload')"
          icon="pi pi-upload"
          :loading="busy"
          @click="upload"
        />
      </template>
      <template v-else-if="task">
        <Button
          v-if="cancellable"
          :label="t('common.cancel')"
          icon="pi pi-times"
          severity="secondary"
          outlined
          @click="cancelRun"
        />
        <Button
          v-if="task.state !== 'running' && task.state !== 'done' && task.uploadId && lastReq"
          :label="t('publish.retryNoUpload')"
          icon="pi pi-replay"
          :loading="busy"
          @click="retryUpload"
        />
        <Button
          v-if="task.state !== 'running'"
          :label="task.state === 'done' ? t('publish.another') : t('publish.backToForm')"
          severity="secondary"
          :outlined="task.state !== 'done'"
          :text="task.state === 'done'"
          @click="again"
        />
        <Button
          :label="task.state === 'running' ? t('publish.hide') : t('common.close')"
          severity="secondary"
          :text="task.state !== 'done'"
          @click="visible = false"
        />
      </template>
    </template>
  </Dialog>
</template>

<style scoped>
.form {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.hint {
  margin: 0;
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.sub,
.label {
  font-size: calc(12.5px * var(--iw-fs, 1));
  font-weight: 600;
  color: var(--iw-muted);
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.row {
  display: flex;
  gap: 8px;
}

.grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}

.list {
  display: flex;
  flex-direction: column;
  max-height: 260px;
  overflow-y: auto;
  border: 1px solid var(--iw-border);
  border-radius: var(--iw-radius);
}

.opt {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 12px;
  border-bottom: 1px solid var(--iw-border);
  cursor: pointer;
}

.opt:last-child {
  border-bottom: 0;
}

.opt:hover {
  background: var(--iw-hover);
}

.opt.on {
  background: var(--iw-primary-soft);
}

.o-main {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-width: 0;
}

.o-name {
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.o-meta,
.small {
  font-size: calc(12px * var(--iw-fs, 1));
}

.checks {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 20px;
}

.check {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  cursor: pointer;
}

.facts {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
  gap: 6px 16px;
  margin: 0;
}

.facts dt {
  color: var(--iw-muted);
}

.facts dd {
  margin: 0;
  overflow-wrap: anywhere;
}

.plan summary {
  cursor: pointer;
  color: var(--iw-muted);
}

.plan ol {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin: 8px 0 0;
  padding-left: 20px;
  font-size: calc(12.5px * var(--iw-fs, 1));
  overflow-wrap: anywhere;
}

.plan pre {
  margin: 4px 0 0;
  padding: 8px;
  max-height: 200px;
  overflow: auto;
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  border: 1px solid var(--iw-border);
  white-space: pre-wrap;
}

.stages {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin: 0;
  padding: 0;
  list-style: none;
  color: var(--iw-dimmed);
}

.stages li {
  display: flex;
  align-items: center;
  gap: 8px;
}

.stages li.done {
  color: var(--iw-success);
}

.stages li.now {
  color: var(--iw-text);
  font-weight: 600;
}

.bytes {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.self-start {
  align-self: flex-start;
}

.err {
  margin-top: 12px;
}

@media (width <= 600px) {
  .grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
