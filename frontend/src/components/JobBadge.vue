<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { computed } from 'vue'
import type { JobFlow, JobState, LocalOutcome } from '../api/types'
import { FLOW_ICON, OUTCOME_TONE, STATE_ICON } from '../lib/jobs'

// Job state pill: state colour + flow icon (+ state text unless `compact`).
// A direct fix job's outcome replaces the state text and colour once it is known.
// With `id` it links to the job page (clicks do not reach the row under it).
const props = defineProps<{ state: JobState; flow?: JobFlow; id?: number; compact?: boolean; outcome?: LocalOutcome | '' }>()
const { t } = useI18n()
const shown = computed(() => (props.outcome && props.state !== 'running' && props.state !== 'queued' ? props.outcome : ''))
const label = computed(() => (shown.value ? t('jobs.outcome.' + shown.value) : t('jobs.state.' + props.state)))
</script>

<template>
  <component
    :is="id ? 'RouterLink' : 'span'"
    :to="id ? '/jobs/' + id : undefined"
    class="job-badge"
    :class="shown ? OUTCOME_TONE[shown] : state.replace('_', '-')"
    :title="(flow ? t('jobs.flow.' + flow) + ' · ' : '') + label"
    @click.stop
  >
    <i
      v-if="flow"
      :class="FLOW_ICON[flow]"
      class="flow"
    />
    <i
      v-else
      :class="STATE_ICON[state]"
    />
    <span v-if="!compact">{{ label }}</span>
    <i
      v-else-if="state === 'running'"
      class="pi pi-spin pi-spinner"
    />
  </component>
</template>

<style scoped>
.job-badge {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  flex: none;
  max-width: 100%;
  overflow: hidden;
  padding: 1px 8px;
  border-radius: 999px;
  font-size: calc(11.5px * var(--iw-fs, 1));
  font-weight: 500;
  line-height: 1.6;
  white-space: nowrap;
  color: var(--iw-muted);
  background: var(--iw-elevated);
}

.job-badge i {
  font-size: calc(10.5px * var(--iw-fs, 1));
}

.job-badge > span {
  overflow: hidden;
  text-overflow: ellipsis;
}

a.job-badge:hover {
  filter: brightness(1.15);
}

.job-badge.running {
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.job-badge.needs-review {
  color: var(--iw-warn);
  background: var(--iw-warn-soft);
}

.job-badge.done {
  color: var(--iw-success);
  background: var(--iw-success-soft);
}

.job-badge.failed {
  color: var(--iw-danger);
  background: var(--iw-danger-soft);
}

.job-badge.cancelled {
  color: var(--iw-dimmed);
}
</style>
