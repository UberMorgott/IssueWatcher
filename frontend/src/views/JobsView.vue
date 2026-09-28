<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter, type LocationQueryRaw } from 'vue-router'
import Select from 'primevue/select'
import SelectButton from 'primevue/selectbutton'
import Button from 'primevue/button'
import { useVirtualizer } from '@tanstack/vue-virtual'
import { useI18n } from 'vue-i18n'
import EmptyState from '../components/EmptyState.vue'
import JobRow from '../components/JobRow.vue'
import { api } from '../api/client'
import type { Job, JobRowData as Row } from '../api/types'
import { rowHeight } from '../lib/appearance'
import { useChunks } from '../lib/chunks'
import { JOB_STATES, matches } from '../lib/jobs'
import { useAppStore } from '../stores/app'
import { useJobEvents, useJobsStore } from '../stores/jobs'

const app = useAppStore()
const jobs = useJobsStore()
const { t } = useI18n()
const route = useRoute()
const router = useRouter()

// Same list mechanics as Issues: fixed row height, keyset chunks of CHUNK
// loaded within ~2 screens of the loaded end, skeleton rows at the tail.
const ROW = rowHeight
const CHUNK = 50
const SKELETON_ROWS = 6

// Filters live in the URL (bookmarkable, survive reloads).
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const filters = computed(() => ({
  state: ['active', ...JOB_STATES].includes(str(route.query.state)) ? str(route.query.state) : '',
  flow: ['fix', 'reply'].includes(str(route.query.flow)) ? str(route.query.flow) : '',
  project: Number(str(route.query.project)) || 0,
}))
function setQuery(patch: Partial<Record<'state' | 'flow' | 'project', string | number | null>>) {
  const q: LocationQueryRaw = { ...route.query }
  for (const [k, v] of Object.entries(patch)) {
    if (v === null || v === '' || v === 0) delete q[k]
    else q[k] = String(v)
  }
  void router.replace({ query: q })
}

const list = useChunks<Job>((cursor) => api.jobs({ ...filters.value, cursor, limit: CHUNK }))
const items = list.items
const skeletons: Row[] = Array.from({ length: SKELETON_ROWS }, (_, i) => ({ id: -1 - i, skeleton: true }) as Row)
const rows = computed<Row[]>(() => (list.loading.value || (!items.value.length && !list.done.value && !list.error.value) ? [...items.value, ...skeletons] : items.value))

const scrollEl = ref<HTMLElement | null>(null)
const virtualizer = useVirtualizer(
  computed(() => ({
    count: rows.value.length,
    getScrollElement: () => scrollEl.value,
    estimateSize: () => ROW.value,
    overscan: 10,
    getItemKey: (i: number) => rows.value[i]?.id ?? i,
  })),
)
watch(ROW, () => virtualizer.value.measure())
const virtualRows = computed(() => virtualizer.value.getVirtualItems())
const totalSize = computed(() => virtualizer.value.getTotalSize())
watch(virtualRows, (vr) => {
  if (!vr.length) return
  const el = scrollEl.value
  const screen = el ? Math.ceil(el.clientHeight / ROW.value) : 12
  if (vr[vr.length - 1].index >= items.value.length - 2 * screen) void list.loadMore().then(checkNearEnd)
})
function checkNearEnd() {
  const el = scrollEl.value
  if (!el || list.done.value || list.error.value) return
  const last = Math.floor((el.scrollTop + el.clientHeight) / ROW.value)
  const screen = Math.ceil(el.clientHeight / ROW.value)
  if (last >= items.value.length - 2 * screen) void list.loadMore().then(checkNearEnd)
}

function reload() {
  list.reset()
  scrollEl.value?.scrollTo({ top: 0 })
  void list.loadMore().then(checkNearEnd)
}
watch(filters, reload, { deep: true })
onMounted(reload)
// SSE reconnect / take-over: events may have been missed.
watch(
  () => app.dataVersion,
  () => {
    if (app.lastChanges === null) reload()
  },
)

// Live rows: job.changed patches a loaded row in place (or drops it when it no
// longer matches the filter); a new job that matches is prepended, keeping the
// rows under the viewport in place.
useJobEvents({
  job: (j) => {
    const i = items.value.findIndex((it) => it.id === j.id)
    const ok = matches(j, filters.value)
    if (i >= 0) {
      const next = items.value.slice()
      if (ok) next[i] = j
      else next.splice(i, 1)
      items.value = next
      if (!ok && list.total.value) list.total.value--
      return
    }
    if (!ok || (items.value.length && j.id < items.value[0].id)) return
    items.value = [j, ...items.value]
    if (list.total.value !== null) list.total.value++
    const el = scrollEl.value
    if (el && el.scrollTop >= ROW.value / 2) {
      const top = el.scrollTop
      void nextTick(() => (el.scrollTop = top + ROW.value))
    }
  },
})

const stateOptions = computed(() => [
  { label: t('jobs.allStates'), value: '' },
  { label: t('jobs.active'), value: 'active' },
  ...JOB_STATES.map((s) => ({ label: t('jobs.state.' + s), value: s })),
])
const flowOptions = computed(() => [
  { label: t('jobs.allFlows'), value: '' },
  { label: t('jobs.flow.fix'), value: 'fix' },
  { label: t('jobs.flow.reply'), value: 'reply' },
])
const projectOptions = computed(() => [{ label: t('issues.allProjects'), value: 0 }, ...app.repos.map((r) => ({ label: r.name, value: r.id }))])
const anyFilter = computed(() => !!(filters.value.state || filters.value.flow || filters.value.project))
const state = computed(() => (list.error.value ? (list.status.value === 404 ? 'unavailable' : 'error') : 'ok'))

function open(it: Row) {
  if (!it.skeleton) void router.push(`/jobs/${it.id}`)
}
</script>

<template>
  <div class="page">
    <div class="filters panel">
      <Select
        :model-value="filters.state"
        :options="stateOptions"
        option-label="label"
        option-value="value"
        :aria-label="t('jobs.stateLabel')"
        class="f-state"
        @update:model-value="(v: string) => setQuery({ state: v })"
      />
      <SelectButton
        :model-value="filters.flow"
        :options="flowOptions"
        option-label="label"
        option-value="value"
        :allow-empty="false"
        :aria-label="t('jobs.flowLabel')"
        @update:model-value="(v: string) => setQuery({ flow: v })"
      />
      <Select
        :model-value="filters.project"
        :options="projectOptions"
        option-label="label"
        option-value="value"
        filter
        :aria-label="t('issues.project')"
        class="f-repo"
        @update:model-value="(v: number) => setQuery({ project: v })"
      />
      <Button
        v-if="anyFilter"
        :label="t('issues.reset')"
        icon="pi pi-filter-slash"
        severity="secondary"
        text
        @click="router.replace({ query: {} })"
      />
      <span
        v-if="list.total.value !== null"
        class="f-total"
      ><b class="mono">{{ list.total.value }}</b> {{ t('jobs.words', list.total.value) }}</span>
    </div>

    <div class="panel table-panel">
      <div
        class="vt"
        role="grid"
        :style="{ '--row': ROW + 'px' }"
      >
        <div
          class="vt-head"
          role="row"
        >
          <span role="columnheader">{{ t('jobs.colState') }}</span>
          <span role="columnheader">{{ t('jobs.colIssue') }}</span>
          <span
            class="c-flow"
            role="columnheader"
          >{{ t('jobs.colFlow') }}</span>
          <span
            class="c-profile"
            role="columnheader"
          >{{ t('jobs.colProfile') }}</span>
          <span
            class="c-attempt"
            role="columnheader"
          />
          <span role="columnheader">{{ t('jobs.colTime') }}</span>
          <span
            class="c-cost"
            role="columnheader"
          >{{ t('jobs.colCost') }}</span>
        </div>
        <div
          v-if="!rows.length"
          class="vt-empty"
        >
          <EmptyState
            v-if="state === 'unavailable'"
            icon="pi pi-server"
            :title="t('jobs.unavailable')"
            :text="t('jobs.unavailableText')"
          />
          <EmptyState
            v-else-if="state === 'error'"
            icon="pi pi-exclamation-triangle"
            :title="t('jobs.loadError')"
            :text="list.error.value"
          >
            <Button
              :label="t('common.retry')"
              icon="pi pi-refresh"
              size="small"
              @click="reload()"
            />
          </EmptyState>
          <EmptyState
            v-else-if="anyFilter"
            icon="pi pi-filter"
            :title="t('jobs.noMatch')"
            :text="t('issues.noMatchText')"
          />
          <EmptyState
            v-else
            icon="pi pi-microchip-ai"
            :title="t('jobs.empty')"
            :text="t('jobs.emptyText')"
          >
            <Button
              as="router-link"
              to="/issues"
              :label="t('nav.issues')"
              icon="pi pi-inbox"
              size="small"
              severity="secondary"
            />
          </EmptyState>
        </div>
        <div
          v-else
          ref="scrollEl"
          class="vt-body"
        >
          <div
            class="vt-space"
            :style="{ height: totalSize + 'px' }"
          >
            <JobRow
              v-for="v in virtualRows"
              :key="v.key as number"
              :item="rows[v.index]"
              :top="v.start"
              :profile="rows[v.index].skeleton ? '' : jobs.profileName(rows[v.index].profileId)"
              @open="open(rows[v.index])"
            />
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  padding: 12px;
}

.f-state {
  width: 190px;
}

.f-repo {
  width: 220px;
}

.f-total {
  margin-left: auto;
  color: var(--iw-muted);
  font-size: calc(13px * var(--iw-fs, 1));
  white-space: nowrap;
}

.f-total b {
  color: var(--iw-text);
}

.table-panel {
  position: relative;
  overflow: hidden;
}

.vt {
  --cols: 150px minmax(0, 1fr) 120px 150px 44px 130px 80px;
}

.vt-head {
  display: grid;
  grid-template-columns: var(--cols);
  align-items: center;
  column-gap: 12px;
  height: 44px;
  padding: 0 16px;
  border-bottom: 1px solid var(--iw-border);
  font-size: calc(12px * var(--iw-fs, 1));
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--iw-muted);
  background: var(--iw-surface);
}

.vt-head .c-cost {
  text-align: right;
}

.vt-body {
  height: calc(100vh - 240px);
  min-height: 240px;
  overflow-y: auto;
  overscroll-behavior: contain;
}

.vt-space {
  position: relative;
  width: 100%;
}

@media (width <= 1023px) {
  .vt {
    --cols: 150px minmax(0, 1fr) 120px 130px 80px;
  }

  .vt-head .c-profile,
  .vt-head .c-attempt {
    display: none;
  }
}

@media (width <= 767px) {
  .vt {
    --cols: 130px minmax(0, 1fr) 100px;
  }

  .vt-head .c-flow,
  .vt-head .c-cost {
    display: none;
  }
}
</style>
