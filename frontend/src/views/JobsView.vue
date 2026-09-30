<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter, type LocationQueryRaw } from 'vue-router'
import Select from 'primevue/select'
import SelectButton from 'primevue/selectbutton'
import Button from 'primevue/button'
import { useVirtualizer } from '@tanstack/vue-virtual'
import { useI18n } from 'vue-i18n'
import EmptyState from '../components/EmptyState.vue'
import ListPage from '../components/ListPage.vue'
import PlatformIcon from '../components/PlatformIcon.vue'
import JobRow from '../components/JobRow.vue'
import { api } from '../api/client'
import type { Job, JobRowData as Row } from '../api/types'
import { rowHeight } from '../lib/appearance'
import { useChunks } from '../lib/chunks'
import { JOB_FLOWS, JOB_STATES, matches } from '../lib/jobs'
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
  flow: (JOB_FLOWS as string[]).includes(str(route.query.flow)) ? str(route.query.flow) : '',
  origin: (['manual', 'rule'] as const).find((o) => o === str(route.query.origin)) ?? ('' as const),
  project: Number(str(route.query.project)) || 0,
}))
function setQuery(patch: Partial<Record<'state' | 'flow' | 'origin' | 'project', string | number | null>>) {
  const q: LocationQueryRaw = { ...route.query }
  for (const [k, v] of Object.entries(patch)) {
    if (v === null || v === '' || v === 0) delete q[k]
    else q[k] = String(v)
  }
  void router.replace({ query: q })
}

const list = useChunks<Job>((cursor) => api.jobs({ ...filters.value, origin: filters.value.origin || undefined, cursor, limit: CHUNK }), {
  cacheKey: () => 'jobs:' + JSON.stringify(filters.value),
})
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
// longer matches the filter); a job that now matches — new, or an older one that
// moved into the filter (e.g. into needs_review) — goes to its id-sorted place when
// that place is within the loaded range, keeping the rows under the viewport in place.
/** The project filter's scope: the project and its linked mod pages (their jobs belong to it). */
const projectScope = computed(() => {
  const id = filters.value.project
  return new Set([id, ...(app.repos.find((r) => r.id === id)?.links ?? [])])
})
useJobEvents({
  job: (j) => {
    const i = items.value.findIndex((it) => it.id === j.id)
    const ok = matches(j, { ...filters.value, projects: projectScope.value })
    if (i >= 0) {
      const next = items.value.slice()
      if (ok) next[i] = j
      else next.splice(i, 1)
      items.value = next
      if (!ok && list.total.value) list.total.value--
      return
    }
    if (!ok) return
    // Rows are newest first (j.id DESC); past the loaded tail the next chunk brings it.
    let at = items.value.findIndex((it) => it.id < j.id)
    if (at < 0) {
      if (!list.done.value) return
      at = items.value.length
    }
    const next = items.value.slice()
    next.splice(at, 0, j)
    items.value = next
    if (list.total.value !== null) list.total.value++
    const el = scrollEl.value
    if (el && el.scrollTop >= ROW.value / 2 && at <= Math.floor(el.scrollTop / ROW.value)) {
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
  ...JOB_FLOWS.map((f) => ({ label: t('jobs.flow.' + f), value: f })),
])
const originOptions = computed(() => [
  { label: t('jobs.allOrigins'), value: '' },
  { label: t('jobs.origin.manual'), value: 'manual' },
  { label: t('jobs.origin.rule'), value: 'rule' },
])
/** Grouped projects only: a linked mod page's jobs belong to its project (the filter spans the group). */
const projectOptions = computed(() => [
  { label: t('issues.allProjects'), value: 0, platform: '' },
  ...app.repos.filter((r) => !r.linkedTo || r.id === filters.value.project).map((r) => ({ label: r.name, value: r.id, platform: r.platform })),
])
const selectedProject = computed(() => projectOptions.value.find((o) => o.value === filters.value.project))
const anyFilter = computed(() => !!(filters.value.state || filters.value.flow || filters.value.origin || filters.value.project))
const state = computed(() => (list.error.value ? (list.status.value === 404 ? 'unavailable' : 'error') : 'ok'))

function open(it: Row) {
  if (!it.skeleton) void router.push(`/jobs/${it.id}`)
}
</script>

<template>
  <div class="page">
    <ListPage
      :resettable="anyFilter"
      :total="list.total.value"
      :total-label="list.total.value === null ? '' : t('jobs.words', list.total.value)"
      @reset="router.replace({ query: {} })"
    >
      <template #filters>
        <Select
          :model-value="filters.state"
          :options="stateOptions"
          option-label="label"
          option-value="value"
          :aria-label="t('jobs.stateLabel')"
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
          :model-value="filters.origin"
          :options="originOptions"
          option-label="label"
          option-value="value"
          :aria-label="t('jobs.originLabel')"
          @update:model-value="(v: string) => setQuery({ origin: v })"
        />
        <Select
          :model-value="filters.project"
          :options="projectOptions"
          option-label="label"
          option-value="value"
          filter
          :aria-label="t('issues.project')"
          @update:model-value="(v: number) => setQuery({ project: v })"
        >
          <template #value="{ placeholder }">
            <span
              v-if="selectedProject"
              class="opt"
            ><PlatformIcon
              v-if="selectedProject.platform"
              :platform="selectedProject.platform"
              :size="14"
            />{{ selectedProject.label }}</span>
            <span v-else>{{ placeholder }}</span>
          </template>
          <template #option="{ option }">
            <span class="opt"><PlatformIcon
              v-if="option.platform"
              :platform="option.platform"
              :size="14"
            />{{ option.label }}</span>
          </template>
        </Select>
      </template>

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
              :title="t('jobs.attempt')"
              :aria-label="t('jobs.attempt')"
            ><i
              class="pi pi-replay"
              aria-hidden="true"
            /></span>
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
              <Button
                as="router-link"
                to="/projects"
                :label="t('nav.projects')"
                icon="pi pi-folder"
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
    </ListPage>
  </div>
</template>

<style scoped>
.opt {
  display: inline-flex;
  align-items: center;
  gap: 8px;
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
