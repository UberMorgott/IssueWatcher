<script setup lang="ts">
import Skeleton from 'primevue/skeleton'
import { useI18n } from 'vue-i18n'
import LabelTag from './LabelTag.vue'
import PlatformIcon from './PlatformIcon.vue'
import { computed } from 'vue'
import JobBadge from './JobBadge.vue'
import JobProgress from './JobProgress.vue'
import type { IssueRowData as Row } from '../api/types'
import { absTime, relTime, repoOwner, shortRepo } from '../lib/format'
import { jobOutcome } from '../lib/jobs'
import { isModPlatform } from '../lib/platforms'
import { useJobsStore } from '../stores/jobs'

// One virtualised row of the issues list. Props are primitives or the row object
// itself, so a row that stays in view never re-renders while the list scrolls.
const props = defineProps<{ item: Row; top: number; selected: boolean; active: boolean }>()
const emit = defineEmits<{ toggle: []; open: [] }>()
const { t } = useI18n()

// The row's job as the live job events know it (fresher than the list row).
const jobs = useJobsStore()
const live = computed(() => (props.item.job ? jobs.byId.get(props.item.job.id) : undefined))
const jobState = computed(() => live.value?.state ?? props.item.job?.state)
const mod = computed(() => isModPlatform(props.item.platform))
/** GitHub rows show #N; mod rows their kind (the number is a local ordinal). */
const ref_ = computed(() => (mod.value ? t('platforms.kindOne.' + (props.item.kind || 'comment')) : '#' + props.item.number))
const outcome = computed(() => (live.value ? jobOutcome(live.value) : (props.item.job?.outcome ?? '')))
</script>

<template>
  <div
    class="vrow"
    :class="{ skeleton: item.skeleton, unread: item.unread, selected, active }"
    :style="{ transform: `translateY(${top}px)` }"
    role="row"
    :aria-selected="selected"
    @click="!item.skeleton && emit('open')"
  >
    <span
      class="c-sel"
      role="gridcell"
      @click.stop
    >
      <input
        v-if="!item.skeleton"
        type="checkbox"
        :checked="selected"
        :aria-label="'#' + item.number"
        @change="emit('toggle')"
      >
    </span>
    <template v-if="item.skeleton">
      <span class="c-title t-skel"><Skeleton
        width="70%"
        height="14px"
      /><Skeleton
        width="40%"
        height="10px"
      /></span>
      <span class="c-project"><Skeleton
        width="60%"
        height="12px"
      /></span>
    </template>
    <template v-else>
      <span
        class="c-title"
        role="gridcell"
      >
        <span
          class="state-icon"
          :class="item.state"
        ><i :class="item.state === 'closed' ? 'pi pi-check-circle' : 'pi pi-circle'" /></span>
        <span class="t-main">
          <span class="t-line">
            <span
              v-if="item.unread"
              class="unread-dot"
              :aria-label="t('issues.unreadAria')"
            />
            <span
              class="t-title"
              :title="item.title.length > 80 ? item.title : undefined"
            >{{ item.title }}</span>
            <JobBadge
              v-if="item.job && jobState"
              :id="item.job.id"
              :state="jobState"
              :flow="item.job.flow"
              :outcome="outcome"
              compact
            />
          </span>
          <span
            v-if="item.job && jobState === 'running'"
            class="t-meta t-run"
          ><span :class="{ mono: !mod, kind: mod }">{{ ref_ }}</span><JobProgress
            :id="item.job.id"
            :attempt="live?.attempt ?? 1"
            :started-at="live?.startedAt"
            :phase="live?.phase"
          /></span>
          <span
            v-else
            class="t-meta"
          ><span :class="{ mono: !mod, kind: mod }">{{ ref_ }}</span><template v-if="outcome"> · <span
            class="outcome"
            :class="outcome"
          >{{ t('jobs.outcome.' + outcome) }}</span> ·</template> {{ t('issues.byOpened', { author: item.author || t('common.unknown'), time: relTime(item.createdAt) }) }}</span>
        </span>
      </span>
      <span
        class="c-project"
        role="gridcell"
      >
        <PlatformIcon
          :platform="item.platform || 'github'"
          :size="14"
        />
        <span
          v-if="mod"
          class="p-name"
        >{{ item.repo }}</span>
        <span
          v-else
          class="p-name"
        ><span class="p-owner">{{ repoOwner(item.repo) }}/</span>{{ shortRepo(item.repo) }}</span>
      </span>
      <span
        class="c-labels"
        role="gridcell"
      >
        <LabelTag
          v-for="l in item.labels.slice(0, 2)"
          :key="l"
          :name="l"
        />
        <span
          v-if="item.labels.length > 2"
          class="l-more"
          :title="item.labels.slice(2).join(', ')"
        >+{{ item.labels.length - 2 }}</span>
        <span
          v-if="mod && !item.labels.length"
          class="l-none"
          :title="t('issues.noLabels')"
        >—</span>
      </span>
      <span
        class="c-num"
        role="gridcell"
        :class="{ zero: !item.comments }"
      ><i class="pi pi-comment" /> <span class="mono">{{ item.comments }}</span></span>
      <span
        class="c-upd"
        role="gridcell"
        :title="absTime(item.updatedAt)"
      >{{ relTime(item.updatedAt) }}</span>
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
  padding: 0 16px 0 12px;
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

.vrow.selected {
  background: var(--iw-primary-soft);
}

.vrow.active {
  box-shadow: inset 3px 0 0 var(--iw-primary);
  background: var(--iw-primary-soft);
}

.c-sel {
  display: grid;
  place-items: center;
  height: 100%;
}

.c-sel input {
  width: 16px;
  height: 16px;
  accent-color: var(--iw-primary);
  cursor: pointer;
}

.c-title {
  display: flex;
  gap: 10px;
  min-width: 0;
}

.t-skel {
  flex-direction: column;
  gap: 8px;
}

.state-icon {
  margin-top: 2px;
  font-size: calc(14px * var(--iw-fs, 1));
}

.state-icon.open {
  color: var(--iw-success);
}

.state-icon.closed {
  color: var(--iw-dimmed);
}

.t-main {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.t-line {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.t-title {
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.unread .t-title {
  font-weight: 650;
}

.t-meta {
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-muted);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.kind {
  color: var(--iw-muted);
  font-weight: 500;
}

.t-run {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.outcome {
  color: var(--iw-warn);
}

.outcome.pushed {
  color: var(--iw-primary);
}

.outcome.closed {
  color: var(--iw-success);
}

.outcome.failed {
  color: var(--iw-danger);
}

.outcome.not_reproduced {
  color: var(--iw-muted);
}

.c-project {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  color: var(--iw-text);
}

.p-name {
  overflow: hidden;
  text-overflow: ellipsis;
}

.p-owner {
  color: var(--iw-dimmed);
}

.c-labels {
  display: flex;
  gap: 4px;
  min-width: 0;
  overflow: hidden;
}

.l-more,
.l-none {
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.c-num {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--iw-muted);
}

.c-num.zero {
  opacity: 0.5;
}

.c-upd {
  color: var(--iw-muted);
  white-space: nowrap;
}

@media (width <= 1023px) {
  .c-labels {
    display: none;
  }
}

@media (width <= 767px) {
  .c-project,
  .c-num {
    display: none;
  }
}
</style>
