<script setup lang="ts">
import Skeleton from 'primevue/skeleton'
import { useI18n } from 'vue-i18n'
import JobBadge from './JobBadge.vue'
import JobProgress from './JobProgress.vue'
import type { JobRowData as Row } from '../api/types'
import { absTime, elapsed, relTime, usd } from '../lib/format'
import { FLOW_ICON, isActive, jobCost, jobDuration, jobOutcome, jobRef } from '../lib/jobs'

// One virtualised row of the jobs list (same contract as IssueRow).
defineProps<{ item: Row; top: number; profile: string }>()
const emit = defineEmits<{ open: [] }>()
const { t } = useI18n()
</script>

<template>
  <div
    class="vrow"
    :class="{ skeleton: item.skeleton }"
    :style="{ transform: `translateY(${top}px)` }"
    role="row"
    @click="!item.skeleton && emit('open')"
  >
    <template v-if="item.skeleton">
      <span><Skeleton
        width="80px"
        height="14px"
      /></span>
      <span class="c-title t-skel"><Skeleton
        width="70%"
        height="14px"
      /><Skeleton
        width="40%"
        height="10px"
      /></span>
    </template>
    <template v-else>
      <span
        class="c-state"
        role="gridcell"
      >
        <JobBadge
          :state="item.state"
          :outcome="jobOutcome(item)"
        />
        <span
          v-if="item.phase && isActive(item.state) && item.state !== 'running'"
          class="phase"
        >{{ t('jobs.phase.' + item.phase) }}</span>
      </span>
      <span
        class="c-title"
        role="gridcell"
      >
        <span
          class="t-title"
          :title="item.title.length > 80 ? item.title : undefined"
        >{{ item.title || t('job.triage.project') }}</span>
        <span
          v-if="item.state === 'running'"
          class="t-meta t-run"
        ><span class="mono">{{ jobRef(item) }}</span><JobProgress
          :id="item.id"
          :attempt="item.attempt"
          :started-at="item.startedAt"
          :phase="item.phase"
        /></span>
        <span
          v-else
          class="t-meta"
        ><span class="mono">{{ jobRef(item) }}</span><template v-if="item.error"> · <span class="err">{{ item.error }}</span></template></span>
      </span>
      <span
        class="c-flow"
        role="gridcell"
      ><i :class="FLOW_ICON[item.flow]" /> {{ t('jobs.flow.' + item.flow) }}<i
        v-if="item.origin === 'rule'"
        class="pi pi-bolt origin"
        :title="t('jobs.origin.rule') + (item.ruleId ? ' · ' + item.ruleId : '')"
        :aria-label="t('jobs.origin.rule')"
      /></span>
      <span
        class="c-profile"
        role="gridcell"
      >{{ profile }}</span>
      <span
        class="c-attempt mono"
        role="gridcell"
        :title="t('jobs.attempt')"
      >{{ item.attempt > 1 ? '×' + item.attempt : '' }}</span>
      <span
        class="c-time"
        role="gridcell"
        :title="absTime(item.createdAt)"
      >
        <span>{{ relTime(item.createdAt) }}</span>
        <span class="t-meta">{{ elapsed(jobDuration(item)) }}</span>
      </span>
      <span
        class="c-cost mono"
        role="gridcell"
      >{{ usd(jobCost(item)) }}</span>
    </template>
  </div>
</template>

<style scoped>
.vrow {
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  height: var(--row, 60px);
  display: grid;
  grid-template-columns: var(--cols);
  align-items: center;
  column-gap: 12px;
  padding: 0 16px;
  border-bottom: 1px solid var(--iw-border);
  cursor: pointer;
  contain: strict;
}

.vrow:hover {
  background: var(--iw-hover);
}

.vrow.skeleton {
  cursor: default;
}

.c-state {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 2px;
  min-width: 0;
}

.phase {
  font-size: calc(11.5px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.c-title {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.t-skel {
  gap: 8px;
}

.t-title {
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.t-meta {
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-muted);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.t-run {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.err {
  color: var(--iw-danger);
}

.origin {
  color: var(--iw-warn);
  font-size: calc(11px * var(--iw-fs, 1));
}

.c-flow,
.c-profile {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  color: var(--iw-muted);
}

.c-attempt,
.c-cost {
  color: var(--iw-muted);
  text-align: right;
}

.c-time {
  display: flex;
  flex-direction: column;
  color: var(--iw-muted);
  white-space: nowrap;
}

@media (width <= 1023px) {
  .c-profile,
  .c-attempt {
    display: none;
  }
}

@media (width <= 767px) {
  .c-flow,
  .c-cost {
    display: none;
  }
}
</style>
