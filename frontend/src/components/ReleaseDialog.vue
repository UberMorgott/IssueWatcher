<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import Skeleton from 'primevue/skeleton'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import ReleasePlan from './ReleasePlan.vue'
import { api } from '../api/client'
import type { ReleasePlan as Plan, ReleaseRequest } from '../api/types'
import { releaseError } from '../lib/release'

// «Выпустить релиз»: the dry-run plan of the code project (version, changelog,
// targets, steps, refusals); an optional explicit version and the targets to
// publish to; «Выпустить» starts the run and opens its page.
const visible = defineModel<boolean>('visible', { required: true })
const props = defineProps<{ project: { id: number; name: string } | null }>()
const { t } = useI18n()
const toast = useToast()
const router = useRouter()

const plan = ref<Plan | null>(null)
const loading = ref(false)
const error = ref('')
const version = ref('')
const selected = ref<string[]>([])
/** What the shown plan was made for; the version / targets changed since → stale. */
const planned = ref({ version: '', targets: '' })
const asKey = (l: string[]) => [...l].sort().join(',')
const stale = computed(() => !!plan.value && (version.value.trim() !== planned.value.version || asKey(selected.value) !== planned.value.targets))
function markPlanned() {
  planned.value = { version: version.value.trim(), targets: asKey(selected.value) }
}
const starting = ref(false)

const VERSION_RE = /^\d+\.\d+\.\d+$/
const versionBad = computed(() => !!version.value.trim() && !VERSION_RE.test(version.value.trim()))

function request(): ReleaseRequest {
  const v = version.value.trim()
  return { ...(v ? { version: v } : {}), ...(plan.value ? { targets: selected.value } : {}) }
}

async function load(first = false) {
  const p = props.project
  if (!p) return
  loading.value = true
  error.value = ''
  const r = await api.releasePlan(p.id, first ? {} : request())
  loading.value = false
  if (!r.ok) {
    error.value = releaseError(r)
    return
  }
  plan.value = r.data
  if (first) selected.value = (r.data.targets ?? []).filter((x) => x.selected).map((x) => x.key)
  markPlanned()
}

watch(visible, (v) => {
  if (!v) return
  plan.value = null
  version.value = ''
  selected.value = []
  void load(true)
})

const canStart = computed(() => !!plan.value && !loading.value && !starting.value && !versionBad.value && (plan.value.ok || stale.value))

async function start() {
  const p = props.project
  if (!p || !canStart.value) return
  starting.value = true
  const r = await api.release(p.id, request())
  starting.value = false
  if (!r.ok) {
    const body = r.body as { plan?: Plan } | undefined
    if (r.status === 409 && body?.plan) {
      // Refused: the plan in the answer says why (shown above the button).
      plan.value = body.plan
      markPlanned()
      return
    }
    toast.add({ severity: 'error', summary: t('release.startFailed'), detail: releaseError(r), life: 8000 })
    return
  }
  const run = r.data?.run
  toast.add({ severity: 'success', summary: t('release.started', { version: run?.version || r.data?.plan?.version || '' }), detail: p.name, life: 5000 })
  visible.value = false
  if (run?.id) void router.push(`/runs/${run.id}`)
}
</script>

<template>
  <Dialog
    v-model:visible="visible"
    :header="t('release.dialogTitle', { name: project?.name ?? '' })"
    modal
    :dismissable-mask="!starting"
    :style="{ width: '720px' }"
    :breakpoints="{ '760px': '94vw' }"
  >
    <div class="body">
      <p class="muted intro">
        {{ t('release.dialogText') }}
      </p>
      <div class="version-row">
        <label
          for="release-version"
          class="label"
        >{{ t('release.version') }}</label>
        <InputText
          id="release-version"
          v-model="version"
          class="mono version"
          :placeholder="plan?.version || '1.2.3'"
          :invalid="versionBad"
          @keydown.enter.prevent="load()"
        />
        <Button
          :label="t('release.replan')"
          icon="pi pi-refresh"
          severity="secondary"
          size="small"
          :loading="loading"
          :disabled="versionBad"
          @click="load()"
        />
      </div>
      <small class="muted">{{ versionBad ? t('release.versionBad') : t('release.versionHint') }}</small>

      <Message
        v-if="error"
        severity="error"
        :closable="false"
      >
        {{ error }}
      </Message>
      <template v-if="loading && !plan">
        <Skeleton height="60px" />
        <Skeleton height="160px" />
      </template>
      <Message
        v-if="stale && plan"
        severity="info"
        :closable="false"
      >
        {{ t('release.stale') }}
      </Message>
      <ReleasePlan
        v-if="plan"
        v-model:selected="selected"
        :plan="plan"
        selectable
      />
    </div>
    <template #footer>
      <Button
        :label="t('common.cancel')"
        severity="secondary"
        text
        :disabled="starting"
        @click="visible = false"
      />
      <Button
        :label="t('release.start')"
        icon="pi pi-send"
        :loading="starting"
        :disabled="!canStart"
        @click="start"
      />
    </template>
  </Dialog>
</template>

<style scoped>
.body {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.intro {
  margin: 0;
  font-size: calc(13px * var(--iw-fs, 1));
}

.version-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.label {
  font-weight: 500;
}

.version {
  width: 140px;
}
</style>
