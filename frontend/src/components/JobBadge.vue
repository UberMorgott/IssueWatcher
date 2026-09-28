<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { JobFlow, JobState } from '../api/types'
import { FLOW_ICON, STATE_ICON } from '../lib/jobs'

// Job state pill: state colour + flow icon (+ state text unless `compact`).
// With `id` it links to the job page (clicks do not reach the row under it).
defineProps<{ state: JobState; flow?: JobFlow; id?: number; compact?: boolean }>()
const { t } = useI18n()
</script>

<template>
  <component
    :is="id ? 'RouterLink' : 'span'"
    :to="id ? '/jobs/' + id : undefined"
    class="job-badge"
    :class="state.replace('_', '-')"
    :title="(flow ? t('jobs.flow.' + flow) + ' · ' : '') + t('jobs.state.' + state)"
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
    <span v-if="!compact">{{ t('jobs.state.' + state) }}</span>
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
