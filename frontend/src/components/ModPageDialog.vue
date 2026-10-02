<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Textarea from 'primevue/textarea'
import Message from 'primevue/message'
import Skeleton from 'primevue/skeleton'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { api, type Result } from '../api/client'
import type { ModPage, ModPageEdit, ModPageSave, PublishTarget } from '../api/types'
import { foldContext, lineDiff } from '../lib/lineDiff'
import { safeUrl } from '../lib/safeUrl'

// «Изменить страницу мода» (Nexus, Phase 7): the mod editor's General tab —
// name, summary, description (BBCode) and version. Load (GET …/page) → edit →
// «Проверить изменения»: the diff against the loaded page plus the server's dry
// run (the exact save request; nothing sent) → «Сохранить» (PUT …/page, sent
// once). Only changed fields are sent; category, author, tags and translation
// are resent by the server exactly as loaded.
const props = defineProps<{ target: PublishTarget | null }>()
const visible = defineModel<boolean>('visible', { required: true })
const { t, te } = useI18n()
const toast = useToast()

type Field = 'name' | 'summary' | 'description' | 'version'
const FIELDS: Field[] = ['name', 'summary', 'description', 'version']

const page = ref<ModPage | null>(null)
const loading = ref(false)
const busy = ref(false)
const step = ref<'edit' | 'review'>('edit')
const plan = ref<ModPageSave | null>(null)
const error = ref<{ code?: string; text: string; detail?: string } | null>(null)
const form = reactive<Record<Field, string>>({ name: '', summary: '', description: '', version: '' })

watch(visible, (v) => {
  if (v && props.target) void load()
})

async function load() {
  const p = props.target
  if (!p) return
  loading.value = true
  error.value = null
  page.value = null
  plan.value = null
  step.value = 'edit'
  const r = await api.modPage(p.projectId)
  loading.value = false
  if (props.target?.projectId !== p.projectId) return
  if (!r.ok) {
    error.value = failure(r)
    return
  }
  page.value = r.data
  for (const f of FIELDS) form[f] = r.data[f]
}

function failure(r: Extract<Result<unknown>, { ok: false }>) {
  const code = (r.body as { code?: unknown } | undefined)?.code
  if (code === 'bad_request') return { code, text: t('modPage.errors.bad_request', { reason: r.error }) }
  if (typeof code === 'string' && te('modPage.errors.' + code)) return { code, text: t('modPage.errors.' + code), detail: r.error }
  return { text: r.status === 0 ? r.error : t('modPage.errors.failed'), detail: r.error }
}
const signInProblem = computed(() => error.value?.code === 'not_signed_in' || error.value?.code === 'relogin')

const maxSummary = computed(() => page.value?.maxSummary ?? 350)
const summaryLeft = computed(() => maxSummary.value - [...form.summary].length)
const changed = computed(() => (page.value ? FIELDS.filter((f) => form[f] !== page.value?.[f]) : []))
const versionOk = computed(() => /^[a-zA-Z0-9.-]+$/.test(form.version) && form.version.length <= 255)
const valid = computed(
  () => !!form.name.trim() && [...form.name].length <= 250 && !!form.summary.trim() && summaryLeft.value >= 0 && !!form.description.trim() && versionOk.value,
)

function edit(dryRun: boolean): ModPageEdit {
  const e: ModPageEdit = { dryRun }
  for (const f of changed.value) e[f] = form[f]
  return e
}

/** «Проверить изменения»: the server's dry run (what would be sent) + the local diff. */
async function review() {
  const p = props.target
  if (!p || !changed.value.length || !valid.value || busy.value) return
  busy.value = true
  error.value = null
  const r = await api.saveModPage(p.projectId, edit(true))
  busy.value = false
  if (!r.ok) {
    error.value = failure(r)
    return
  }
  plan.value = r.data
  step.value = 'review'
}

async function save() {
  const p = props.target
  if (!p || busy.value) return
  busy.value = true
  error.value = null
  const r = await api.saveModPage(p.projectId, edit(false))
  busy.value = false
  if (!r.ok) {
    error.value = failure(r)
    return
  }
  toast.add({ severity: 'success', summary: t('modPage.saved'), detail: p.name, life: 3000 })
  visible.value = false
}

const diffs = computed(() => {
  const pg = page.value
  if (!pg) return []
  return changed.value.map((f) => ({
    field: f,
    multi: f === 'summary' || f === 'description',
    before: pg[f],
    after: form[f],
    lines: f === 'summary' || f === 'description' ? foldContext(lineDiff(pg[f], form[f])) : [],
  }))
})
const pretty = (b: unknown) => JSON.stringify(b, null, 2)
</script>

<template>
  <Dialog
    v-model:visible="visible"
    :header="t('modPage.title', { name: target?.name ?? '' })"
    modal
    :style="{ width: '820px' }"
    :breakpoints="{ '860px': '96vw' }"
  >
    <div
      v-if="loading"
      class="form"
    >
      <Skeleton height="36px" />
      <Skeleton height="80px" />
      <Skeleton height="240px" />
    </div>

    <!-- Edit -->
    <div
      v-else-if="page && step === 'edit'"
      class="form"
    >
      <p class="hint">
        {{ t('modPage.hint') }}
        <a
          :href="safeUrl(page.url)"
          target="_blank"
          rel="noopener noreferrer"
        >{{ t('projects.openModPage') }} <i class="pi pi-external-link" /></a>
      </p>
      <div class="grid">
        <label class="field">
          <span class="label">{{ t('modPage.name') }}</span>
          <InputText
            v-model="form.name"
            maxlength="250"
            fluid
          />
        </label>
        <label class="field">
          <span class="label">{{ t('modPage.version') }}</span>
          <InputText
            v-model="form.version"
            class="mono"
            maxlength="255"
            :invalid="!versionOk"
            fluid
          />
        </label>
      </div>
      <label class="field">
        <span class="label">{{ t('modPage.summary') }}
          <span
            class="count"
            :class="{ over: summaryLeft < 0 }"
          >{{ t('modPage.left', { n: summaryLeft }) }}</span>
        </span>
        <Textarea
          v-model="form.summary"
          rows="3"
          auto-resize
          :invalid="summaryLeft < 0"
          fluid
        />
      </label>
      <label class="field">
        <span class="label">{{ t('modPage.description') }}</span>
        <Textarea
          v-model="form.description"
          class="mono bbcode"
          rows="16"
          spellcheck="false"
          fluid
        />
      </label>
      <p class="meta muted">
        <template v-if="page.category">
          {{ t('modPage.category', { name: page.category }) }} ·
        </template>
        <template v-if="page.author">
          {{ t('modPage.author', { name: page.author }) }} ·
        </template>
        {{ page.tags.length ? t('modPage.tags', { list: page.tags.join(', ') }) : t('modPage.noTags') }} — {{ t('modPage.kept') }}
      </p>
    </div>

    <!-- Review: the diff against the loaded page + the exact request -->
    <div
      v-else-if="page && step === 'review' && plan"
      class="form"
    >
      <p class="hint">
        {{ t('modPage.reviewText') }}
      </p>
      <section
        v-for="d in diffs"
        :key="d.field"
        class="change"
      >
        <span class="label">{{ t('modPage.' + d.field) }}</span>
        <div
          v-if="!d.multi"
          class="inline mono"
        >
          <del>{{ d.before || '—' }}</del> → <ins>{{ d.after }}</ins>
        </div>
        <pre
          v-else
          class="diff mono"
        ><template
          v-for="(l, i) in d.lines"
          :key="i"
        ><span
          v-if="l"
          :class="l.kind"
        >{{ l.kind === 'add' ? '+ ' : l.kind === 'del' ? '- ' : '  ' }}{{ l.text }}
</span><span
          v-else
          class="fold"
        >  …
</span></template></pre>
      </section>
      <details class="plan">
        <summary>{{ t('modPage.request') }}</summary>
        <p class="mono small">
          <b>{{ plan.request.method }}</b> {{ plan.request.url }}
        </p>
        <pre class="mono">{{ pretty(plan.request.body) }}</pre>
      </details>
      <Message
        severity="warn"
        size="small"
        variant="simple"
      >
        {{ t('modPage.liveWarning') }}
      </Message>
    </div>

    <Message
      v-if="error"
      v-tooltip.top="error.detail || undefined"
      :severity="error.code === 'save_unsure' ? 'warn' : 'error'"
      size="small"
      variant="simple"
      class="err"
    >
      {{ error.text }}
      <RouterLink
        v-if="signInProblem"
        to="/connections"
        @click="visible = false"
      >
        {{ t('publish.toSettings') }}
      </RouterLink>
      <a
        v-if="error.code === 'save_unsure' && page"
        :href="safeUrl(page.url)"
        target="_blank"
        rel="noopener noreferrer"
      >{{ t('projects.openModPage') }} <i class="pi pi-external-link" /></a>
    </Message>

    <template #footer>
      <template v-if="step === 'edit'">
        <span
          v-if="page"
          class="muted small changed"
        >{{ changed.length ? t('modPage.changed', { list: changed.map((f) => t('modPage.' + f)).join(', ') }) : t('modPage.unchanged') }}</span>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          text
          @click="visible = false"
        />
        <Button
          v-if="!page && !loading"
          :label="t('common.retry')"
          icon="pi pi-refresh"
          severity="secondary"
          outlined
          @click="load"
        />
        <Button
          :label="t('modPage.review')"
          icon="pi pi-list-check"
          :loading="busy"
          :disabled="!page || !changed.length || !valid"
          @click="review"
        />
      </template>
      <template v-else>
        <Button
          :label="t('publish.back')"
          icon="pi pi-arrow-left"
          severity="secondary"
          text
          :disabled="busy"
          @click="step = 'edit'"
        />
        <Button
          :label="t('modPage.save')"
          icon="pi pi-check"
          :loading="busy"
          :disabled="error?.code === 'save_unsure'"
          @click="save"
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

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.label {
  display: flex;
  justify-content: space-between;
  gap: 8px;
  font-size: calc(12.5px * var(--iw-fs, 1));
  font-weight: 600;
  color: var(--iw-muted);
}

.count {
  font-weight: 400;
}

.count.over {
  color: var(--iw-danger);
}

.grid {
  display: grid;
  grid-template-columns: minmax(0, 2fr) minmax(0, 1fr);
  gap: 12px;
}

.bbcode {
  font-size: calc(12.5px * var(--iw-fs, 1));
  line-height: 1.45;
}

.meta,
.small {
  margin: 0;
  font-size: calc(12px * var(--iw-fs, 1));
}

.change {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.inline {
  overflow-wrap: anywhere;
}

del {
  color: var(--iw-danger);
}

ins {
  color: var(--iw-success);
  text-decoration: none;
}

.diff,
.plan pre {
  margin: 0;
  padding: 8px;
  max-height: 320px;
  overflow: auto;
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  border: 1px solid var(--iw-border);
  font-size: calc(12px * var(--iw-fs, 1));
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.diff .add {
  display: block;
  color: var(--iw-success);
  background: var(--iw-success-soft);
}

.diff .del {
  display: block;
  color: var(--iw-danger);
}

.diff .fold {
  display: block;
  color: var(--iw-dimmed);
}

.plan summary {
  cursor: pointer;
  color: var(--iw-muted);
}

.err {
  margin-top: 12px;
}

.changed {
  margin-right: auto;
}

@media (width <= 600px) {
  .grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
