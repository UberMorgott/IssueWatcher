<script setup lang="ts">
import { computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { JobPhase } from '../api/types'
import { elapsed } from '../lib/format'
import { useNow } from '../lib/jobs'
import { useJobsStore } from '../stores/jobs'

// One line for a running job: spinner, ticking run time and the agent's current
// step (live from job.log; on first show the log tail, else the phase).
const props = defineProps<{ id: number; attempt: number; startedAt?: string; phase?: JobPhase }>()
const { t } = useI18n()
const jobs = useJobsStore()
const now = useNow()

watch(
  () => [props.id, props.attempt],
  () => void jobs.loadStep(props.id, props.attempt),
  { immediate: true },
)

const took = computed(() => {
  const start = props.startedAt ? Date.parse(props.startedAt) : NaN
  return Number.isFinite(start) ? elapsed(Math.max(0, now.value - start)) : ''
})
const step = computed(() => jobs.stepOf(props.id, props.attempt) || (props.phase ? t('jobs.phase.' + props.phase) : t('jobs.state.running')))
</script>

<template>
  <span
    class="job-progress"
    :title="step"
  >
    <i class="pi pi-spin pi-spinner" />
    <span
      v-if="took"
      class="took mono"
    >{{ took }}</span>
    <span class="step">{{ step }}</span>
  </span>
</template>

<style scoped>
.job-progress {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  max-width: 100%;
  color: var(--iw-muted);
  font-size: calc(12px * var(--iw-fs, 1));
}

.job-progress i {
  flex: none;
  font-size: calc(11px * var(--iw-fs, 1));
  color: var(--iw-primary);
}

.took {
  flex: none;
  color: var(--iw-primary);
}

.step {
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
</style>
