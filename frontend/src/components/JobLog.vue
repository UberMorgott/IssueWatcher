<script setup lang="ts">
import { nextTick, ref, shallowRef, watch } from 'vue'
import Skeleton from 'primevue/skeleton'
import Message from 'primevue/message'
import { useI18n } from 'vue-i18n'
import { api } from '../api/client'
import type { JobStep } from '../api/types'
import { useJobEvents } from '../stores/jobs'
import { lang } from '../i18n'

// Log of one attempt: GET /api/jobs/{id}/log, then job.log steps of the same
// attempt appended live. Follows the tail unless the reader scrolled up.
const props = defineProps<{ jobId: number; attempt: number }>()
const { t } = useI18n()

const steps = shallowRef<JobStep[]>([])
const loading = ref(false)
const error = ref('')
const box = ref<HTMLElement | null>(null)
const follow = ref(true)
let pending: JobStep[] | null = null
let gen = 0

const key = (s: JobStep) => s.t + '\u0000' + s.kind + '\u0000' + s.text

async function load() {
  const g = ++gen
  loading.value = true
  pending = []
  steps.value = []
  follow.value = true
  const r = await api.jobLog(props.jobId, props.attempt)
  if (g !== gen) return
  loading.value = false
  const buffered = pending ?? []
  pending = null
  if (!r.ok) {
    error.value = r.error
    return
  }
  error.value = ''
  const loaded = r.data.steps ?? []
  // Steps that arrived live while the log loaded: keep the ones not in it yet.
  const seen = new Set(loaded.slice(-500).map(key))
  steps.value = loaded.concat(buffered.filter((s) => !seen.has(key(s))))
  void scrollEnd()
}
watch(() => [props.jobId, props.attempt], load, { immediate: true })

useJobEvents({
  log: (e) => {
    if (e.id !== props.jobId || e.attempt !== props.attempt) return
    if (pending) {
      pending.push(...e.steps)
      return
    }
    steps.value = steps.value.concat(e.steps)
    void scrollEnd()
  },
})

function onScroll() {
  const el = box.value
  if (el) follow.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

async function scrollEnd(force = false) {
  if (!follow.value && !force) return
  await nextTick()
  const el = box.value
  if (el) el.scrollTop = el.scrollHeight
  if (force) follow.value = true
}

const timeFmt = () => new Intl.DateTimeFormat(lang(), { hour: '2-digit', minute: '2-digit', second: '2-digit' })
let tf = timeFmt()
watch(lang, () => (tf = timeFmt()))
function clock(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : tf.format(d)
}

const ICON: Record<string, string> = {
  info: 'pi pi-info-circle',
  text: 'pi pi-comment',
  tool: 'pi pi-cog',
  output: 'pi pi-angle-right',
  error: 'pi pi-times-circle',
  stderr: 'pi pi-exclamation-triangle',
  result: 'pi pi-flag',
}
</script>

<template>
  <div class="log-wrap">
    <Message
      v-if="error"
      severity="error"
      size="small"
    >
      {{ error }}
    </Message>
    <div
      ref="box"
      class="log mono"
      role="log"
      :aria-label="t('job.log.title')"
      aria-live="polite"
      @scroll.passive="onScroll"
    >
      <template v-if="loading">
        <Skeleton
          v-for="i in 4"
          :key="i"
          height="14px"
          :width="30 + i * 12 + '%'"
        />
      </template>
      <p
        v-else-if="!steps.length && !error"
        class="muted empty"
      >
        {{ t('job.log.empty') }}
      </p>
      <div
        v-for="(s, i) in steps"
        :key="i"
        class="step"
        :class="s.kind"
      >
        <span class="time">{{ clock(s.t) }}</span>
        <i
          :class="ICON[s.kind] ?? 'pi pi-circle'"
          :title="t('job.log.kind.' + s.kind)"
        />
        <span class="text">{{ s.text }}</span>
      </div>
    </div>
    <button
      v-if="!follow && steps.length"
      type="button"
      class="tail"
      @click="scrollEnd(true)"
    >
      <i class="pi pi-arrow-down" /> {{ t('job.log.follow') }}
    </button>
  </div>
</template>

<style scoped>
.log-wrap {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.log {
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-height: 60vh;
  min-height: 120px;
  overflow-y: auto;
  padding: 10px 12px;
  border-radius: var(--iw-radius-sm);
  border: 1px solid var(--iw-border);
  background: var(--iw-bg);
  font-size: calc(12.5px * var(--iw-fs, 1));
  line-height: 1.5;
}

.empty {
  margin: 0;
}

.step {
  display: grid;
  grid-template-columns: 64px 16px minmax(0, 1fr);
  gap: 8px;
  align-items: baseline;
}

.step i {
  font-size: calc(11px * var(--iw-fs, 1));
  color: var(--iw-dimmed);
}

.time {
  color: var(--iw-dimmed);
  font-size: calc(11.5px * var(--iw-fs, 1));
}

.text {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.step.info .text {
  color: var(--iw-muted);
}

.step.text .text {
  font-family: var(--iw-font);
  color: var(--iw-text);
}

.step.tool .text,
.step.tool i {
  color: var(--iw-primary);
}

.step.output .text {
  display: block;
  max-height: 9.5em;
  overflow: auto;
  padding: 2px 8px;
  border-radius: 4px;
  color: var(--iw-muted);
  background: var(--iw-elevated);
}

.step.error .text,
.step.error i {
  color: var(--iw-danger);
}

.step.stderr .text,
.step.stderr i {
  color: var(--iw-warn);
}

.step.result .text,
.step.result i {
  color: var(--iw-success);
  font-weight: 600;
}

.tail {
  position: absolute;
  right: 16px;
  bottom: 12px;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 12px;
  border: 0;
  border-radius: 999px;
  font: inherit;
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-on-primary);
  background: var(--iw-primary);
  box-shadow: var(--iw-shadow);
  cursor: pointer;
}
</style>
