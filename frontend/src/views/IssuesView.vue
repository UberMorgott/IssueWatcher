<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter, type LocationQueryRaw } from 'vue-router'
import Select from 'primevue/select'
import SelectButton from 'primevue/selectbutton'
import InputText from 'primevue/inputtext'
import IconField from 'primevue/iconfield'
import InputIcon from 'primevue/inputicon'
import Button from 'primevue/button'
import ToggleButton from 'primevue/togglebutton'
import { useToast } from 'primevue/usetoast'
import EmptyState from '../components/EmptyState.vue'
import ConnectHero from '../components/ConnectHero.vue'
import IssueRow from '../components/IssueRow.vue'
import { useVirtualizer } from '@tanstack/vue-virtual'
import { api } from '../api/client'
import type { Issue, IssueQuery, IssueRowData as Row } from '../api/types'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { typing } from '../lib/shortcuts'
import { useChunks } from '../lib/chunks'

const app = useAppStore()
const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const toast = useToast()

// Infinite list: fixed row height (virtual scroller, no layout shift), chunks of
// CHUNK loaded while the viewport is within ~2 screens of the loaded end. Rows use
// native title tooltips: a tooltip directive per recycled row costs frames.
const ROW = 60
const CHUNK = 50
const SKELETON_ROWS = 6

// Filters live in the URL so a filtered view can be bookmarked and survives reloads.
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const filters = computed(() => ({
  source: str(route.query.source),
  repo: Number(str(route.query.repo)) || 0,
  state: (['open', 'closed', 'all'].includes(str(route.query.state)) ? str(route.query.state) : 'open') as 'open' | 'closed' | 'all',
  label: str(route.query.label),
  q: str(route.query.q),
  unread: route.query.unread === '1',
}))
const query = (): IssueQuery => {
  const f = filters.value
  return { source: f.source, repo: f.repo, state: f.state, label: f.label, q: f.q, unread: f.unread }
}

function setQuery(patch: Partial<Record<'source' | 'repo' | 'state' | 'label' | 'q' | 'unread', string | number | boolean | null>>) {
  const q: LocationQueryRaw = { ...route.query }
  delete q.focus
  for (const [k, v] of Object.entries(patch)) {
    if (v === null || v === '' || v === false || v === 0) delete q[k]
    else q[k] = v === true ? '1' : String(v)
  }
  void router.replace({ query: q })
}

const list = useChunks<Issue>((cursor) => api.issues({ ...query(), cursor, limit: CHUNK }))
const items = list.items
const total = list.total
const listError = list.error
const selected = ref<Issue[]>([])
const cursor = ref(-1)
const seenLabels = ref(new Set<string>())
const skeletons: Row[] = Array.from({ length: SKELETON_ROWS }, (_, i) => ({ id: -1 - i, skeleton: true }) as Row)
/** Loaded rows plus skeleton rows at the tail while the next chunk loads. */
const rows = computed<Row[]>(() => (list.loading.value || (!items.value.length && !list.done.value && !list.error.value) ? [...items.value, ...skeletons] : items.value))
const state = computed(() => (list.error.value ? (list.status.value === 404 ? 'unavailable' : 'error') : 'ok'))

watch(items, (v) => {
  for (const it of v) for (const l of it.labels) seenLabels.value.add(l)
})

// --- scrolling: TanStack virtualizer over a fixed row height; rows keyed by id
// keep their component while scrolling (only rows entering the view mount).
const scrollEl = ref<HTMLElement | null>(null)
const scroller = () => scrollEl.value
const virtualizer = useVirtualizer(
  computed(() => ({
    count: rows.value.length,
    getScrollElement: () => scrollEl.value,
    estimateSize: () => ROW,
    overscan: 10,
    getItemKey: (i: number) => rows.value[i]?.id ?? i,
  })),
)
const virtualRows = computed(() => virtualizer.value.getVirtualItems())
const totalSize = computed(() => virtualizer.value.getTotalSize())
let firstVisible = 0
const newAbove = ref(0)
watch(virtualRows, (vr) => {
  if (!vr.length) return
  const el = scroller()
  firstVisible = el ? Math.floor(el.scrollTop / ROW) : vr[0].index
  const screen = el ? Math.ceil(el.clientHeight / ROW) : 12
  if (vr[vr.length - 1].index >= items.value.length - 2 * screen) void list.loadMore().then(checkNearEnd)
})
function checkNearEnd() {
  const el = scroller()
  if (!el || list.done.value || list.error.value) return
  const last = Math.floor((el.scrollTop + el.clientHeight) / ROW)
  const screen = Math.ceil(el.clientHeight / ROW)
  if (last >= items.value.length - 2 * screen) void list.loadMore().then(checkNearEnd)
}
function onScroll() {
  const el = scroller()
  if (el && el.scrollTop < ROW / 2) newAbove.value = 0
}
function toTop() {
  scroller()?.scrollTo({ top: 0, behavior: 'smooth' })
  newAbove.value = 0
}
/** Keeps the rows under the viewport in place after rows were added/removed above it. */
async function keepAnchor(deltaRows: number) {
  const el = scroller()
  if (!el || !deltaRows || el.scrollTop < ROW / 2) return
  const top = el.scrollTop
  await nextTick()
  el.scrollTop = Math.max(0, top + deltaRows * ROW)
}

function reload() {
  list.reset()
  newAbove.value = 0
  cursor.value = -1
  scroller()?.scrollTo({ top: 0 })
  if (!app.onboarding) void list.loadMore().then(checkNearEnd)
}
watch(filters, reload, { deep: true })
watch(() => app.onboarding, reload)

// --- live updates: new/moved rows at the head, changed rows patched in place,
// rows that left the filter removed; the scroll position stays put.
let refreshing = false
async function refreshLive() {
  if (app.onboarding || refreshing) return
  if (!items.value.length) {
    if (!list.loading.value) reload()
    return
  }
  refreshing = true
  try {
    const changes = app.lastChanges
    let added = 0
    let removedAbove = 0
    if (list.head.value) {
      const r = await api.issues({ ...query(), after: list.head.value, limit: 200 })
      if (r.ok && r.data.more) {
        reload() // too much changed: start over
        return
      }
      if (r.ok && r.data.items.length) {
        const ids = new Set(r.data.items.map((it) => it.id))
        const kept: Issue[] = []
        items.value.forEach((it, i) => {
          if (ids.has(it.id)) {
            if (i < firstVisible) removedAbove++
          } else kept.push(it)
        })
        items.value = [...r.data.items, ...kept]
        list.head.value = r.data.headCursor
        added = r.data.items.length
      }
      if (r.ok && r.data.total !== undefined) list.total.value = r.data.total
    }
    // Re-check loaded rows: only the named items, or all of them when unknown.
    const named = changes?.map((c) => c.itemId).filter((id): id is number => !!id)
    const check = changes === null ? items.value.map((it) => it.id) : [...new Set(named)]
    for (let i = 0; i < check.length; i += 200) {
      const part = check.slice(i, i + 200)
      const r = await api.issues({ ...query(), ids: part })
      if (!r.ok) break
      const fresh = new Map(r.data.items.map((it) => [it.id, it]))
      const gone = new Set(part.filter((id) => !fresh.has(id)))
      if (!fresh.size && !gone.size) continue
      const next: Issue[] = []
      items.value.forEach((it, idx) => {
        if (gone.has(it.id)) {
          if (idx < firstVisible + added) removedAbove++
          return
        }
        next.push(fresh.get(it.id) ?? it)
      })
      items.value = next
    }
    const byId = new Map(items.value.map((it) => [it.id, it]))
    selected.value = selected.value.filter((s) => byId.has(s.id)).map((s) => byId.get(s.id) as Issue)
    if (added && (scroller()?.scrollTop ?? 0) >= ROW / 2) newAbove.value += added
    await keepAnchor(added - removedAbove)
  } finally {
    refreshing = false
  }
}
watch(() => app.dataVersion, () => void refreshLive())

// --- filter controls
const search = ref(filters.value.q)
let searchTimer: number | undefined
watch(search, (v) => {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => setQuery({ q: v.trim() }), 250)
})
watch(
  () => filters.value.q,
  (v) => {
    if (v !== search.value.trim()) search.value = v
  },
)

const sourceOptions = computed(() => [
  { label: t('issues.allSources'), value: '' },
  { label: 'GitHub', value: 'github' },
  { label: t('common.soonSuffix', { name: 'CurseForge' }), value: 'curseforge', disabled: true },
  { label: t('common.soonSuffix', { name: 'Nexus Mods' }), value: 'nexusmods', disabled: true },
  { label: t('common.soonSuffix', { name: 'Steam Workshop' }), value: 'steam', disabled: true },
])
const repoOptions = computed(() => [{ label: t('issues.allProjects'), value: 0 }, ...app.repos.map((r) => ({ label: r.name, value: r.id }))])
const stateOptions = computed(() => [
  { label: t('issues.open'), value: 'open' },
  { label: t('issues.closed'), value: 'closed' },
  { label: t('issues.all'), value: 'all' },
])
const labelOptions = computed(() => {
  const s = new Set(seenLabels.value)
  if (filters.value.label) s.add(filters.value.label)
  return [{ label: t('issues.anyLabel'), value: '' }, ...[...s].sort().map((l) => ({ label: l, value: l }))]
})
const anyFilter = computed(() => {
  const f = filters.value
  return !!(f.source || f.repo || f.label || f.q || f.unread || f.state !== 'open')
})
function reset() {
  search.value = ''
  void router.replace({ query: {} })
}

const counts = computed(() => {
  const repos = filters.value.repo ? app.repos.filter((r) => r.id === filters.value.repo) : app.repos
  return repos.reduce((a, r) => ({ open: a.open + r.open, closed: a.closed + r.closed, unread: a.unread + r.unread }), { open: 0, closed: 0, unread: 0 })
})

function open(it: Row) {
  if (!it.skeleton) void router.push(`/item/${it.id}`)
}

// --- bulk actions
async function markSelectedRead() {
  const ids = selected.value.filter((i) => i.unread).map((i) => i.id)
  const results = await Promise.all(ids.map((id) => api.markRead(id)))
  const failed = results.filter((r) => !r.ok).length
  toast.add({
    severity: failed ? 'warn' : 'success',
    summary: failed ? t('issues.markFailed', { failed, total: ids.length }) : t('issues.marked', { n: ids.length }),
    life: 3000,
  })
  selected.value = []
  for (const id of ids) app.invalidate({ reason: 'read', itemId: id }) // coalesces with the server's data.changed
}

// --- keyboard: J/K move, Enter opens, X selects
const searchBox = ref<{ $el: HTMLElement } | null>(null)
function focusSearch() {
  void nextTick(() => searchBox.value?.$el?.querySelector?.('input')?.focus() ?? (searchBox.value?.$el as HTMLInputElement | undefined)?.focus())
}
function ensureVisible(i: number) {
  virtualizer.value.scrollToIndex(i, { align: 'auto' })
}
function onKey(e: KeyboardEvent) {
  if (e.ctrlKey || e.metaKey || e.altKey || typing(e) || !items.value.length) return
  const k = e.key.toLowerCase()
  if (k === 'j' || k === 'k') {
    e.preventDefault()
    cursor.value = Math.min(items.value.length - 1, Math.max(0, cursor.value + (k === 'j' ? 1 : -1)))
    ensureVisible(cursor.value)
  } else if (k === 'enter' && cursor.value >= 0) {
    open(items.value[cursor.value])
  } else if (k === 'x' && cursor.value >= 0) {
    const it = items.value[cursor.value]
    selected.value = selected.value.some((s) => s.id === it.id) ? selected.value.filter((s) => s.id !== it.id) : [...selected.value, it]
  } else if (k === 'escape' && selected.value.length) {
    selected.value = []
  }
}
const selectedIds = computed(() => new Set(selected.value.map((s) => s.id)))
const cursorId = computed(() => items.value[cursor.value]?.id)
function toggle(it: Row) {
  selected.value = selectedIds.value.has(it.id) ? selected.value.filter((s) => s.id !== it.id) : [...selected.value, it]
}
const allSelected = computed(() => items.value.length > 0 && selected.value.length === items.value.length)
function toggleAll() {
  selected.value = allSelected.value ? [] : [...items.value]
}
onMounted(() => {
  reload()
  window.addEventListener('keydown', onKey)
  window.addEventListener('iw:focus-search', focusSearch)
  if (route.query.focus) focusSearch()
})
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKey)
  window.removeEventListener('iw:focus-search', focusSearch)
})
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">
          {{ t('nav.issues') }}
        </h2>
        <p class="page-sub">
          {{ t('issues.sub') }}
        </p>
      </div>
      <div
        v-if="!app.onboarding"
        class="counters"
      >
        <span class="counter"><span class="c-dot open" /> <b class="mono">{{ counts.open }}</b> {{ t('words.open', counts.open) }}</span>
        <span class="counter"><span class="c-dot closed" /> <b class="mono">{{ counts.closed }}</b> {{ t('words.closed', counts.closed) }}</span>
        <span class="counter"><span class="unread-dot" /> <b class="mono">{{ counts.unread }}</b> {{ t('words.unread', counts.unread) }}</span>
      </div>
    </div>

    <ConnectHero v-if="app.onboarding" />

    <template v-else>
      <div class="filters panel">
        <IconField class="f-search">
          <InputIcon class="pi pi-search" />
          <InputText
            ref="searchBox"
            v-model="search"
            :placeholder="t('issues.searchPlaceholder')"
            :aria-label="t('issues.searchAria')"
            fluid
          />
        </IconField>
        <Select
          :model-value="filters.source"
          :options="sourceOptions"
          option-label="label"
          option-value="value"
          option-disabled="disabled"
          :aria-label="t('issues.source')"
          class="f-source"
          @update:model-value="(v: string) => setQuery({ source: v })"
        />
        <Select
          :model-value="filters.repo"
          :options="repoOptions"
          option-label="label"
          option-value="value"
          filter
          :aria-label="t('issues.project')"
          class="f-repo"
          @update:model-value="(v: number) => setQuery({ repo: v })"
        />
        <Select
          :model-value="filters.label"
          :options="labelOptions"
          option-label="label"
          option-value="value"
          filter
          :aria-label="t('issues.label')"
          class="f-label"
          @update:model-value="(v: string) => setQuery({ label: v })"
        />
        <SelectButton
          :model-value="filters.state"
          :options="stateOptions"
          option-label="label"
          option-value="value"
          :allow-empty="false"
          :aria-label="t('issues.state')"
          @update:model-value="(v: string) => setQuery({ state: v === 'open' ? '' : v })"
        />
        <ToggleButton
          :model-value="filters.unread"
          :on-label="t('issues.unread')"
          :off-label="t('issues.unread')"
          on-icon="pi pi-circle-fill"
          off-icon="pi pi-circle"
          :aria-label="t('issues.onlyUnread')"
          @update:model-value="(v: boolean) => setQuery({ unread: v })"
        />
        <Button
          v-if="anyFilter"
          :label="t('issues.reset')"
          icon="pi pi-filter-slash"
          severity="secondary"
          text
          @click="reset"
        />
        <span
          v-if="total !== null"
          class="f-total"
        ><b class="mono">{{ total }}</b> {{ t('words.issues', total) }}</span>
      </div>

      <div class="panel table-panel">
        <button
          v-if="newAbove > 0"
          type="button"
          class="new-pill"
          @click="toTop"
        >
          <i class="pi pi-arrow-up" /> {{ t('issues.newAbove', newAbove) }}
        </button>
        <div
          class="vt"
          role="grid"
          :aria-rowcount="total ?? undefined"
          :style="{ '--row': ROW + 'px' }"
        >
          <div
            class="vt-head"
            role="row"
          >
            <span class="c-sel"><input
              type="checkbox"
              :checked="allSelected"
              :indeterminate="selected.length > 0 && !allSelected"
              :disabled="!items.length"
              :aria-label="t('issues.selectAll')"
              @change="toggleAll"
            ></span>
            <span role="columnheader">{{ t('issues.colIssue') }}</span>
            <span
              class="c-project"
              role="columnheader"
            >{{ t('issues.colProject') }}</span>
            <span
              class="c-labels"
              role="columnheader"
            >{{ t('issues.colLabels') }}</span>
            <span
              class="c-num"
              role="columnheader"
            >{{ t('issues.colComments') }}</span>
            <span role="columnheader">{{ t('issues.colUpdated') }}</span>
          </div>
          <div
            v-if="!rows.length"
            class="vt-empty"
          >
            <EmptyState
              v-if="state === 'unavailable'"
              icon="pi pi-server"
              :title="t('issues.unavailable')"
              :text="t('issues.unavailableText')"
            />
            <EmptyState
              v-else-if="state === 'error'"
              icon="pi pi-exclamation-triangle"
              :title="t('issues.loadError')"
              :text="listError"
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
              :title="t('issues.noMatch')"
              :text="t('issues.noMatchText')"
            >
              <Button
                :label="t('issues.resetFilters')"
                icon="pi pi-filter-slash"
                size="small"
                severity="secondary"
                @click="reset"
              />
            </EmptyState>
            <EmptyState
              v-else
              icon="pi pi-inbox"
              :title="t('issues.noOpen')"
              :text="app.sync?.lastSync ? t('issues.nothingOpen') : t('issues.nothingSynced')"
            >
              <Button
                v-if="!app.sync?.lastSync"
                :label="t('common.syncNow')"
                icon="pi pi-sync"
                size="small"
                :loading="app.syncing"
                @click="app.syncNow()"
              />
            </EmptyState>
          </div>
          <div
            v-else
            ref="scrollEl"
            class="vt-body"
            @scroll.passive="onScroll"
          >
            <div
              class="vt-space"
              :style="{ height: totalSize + 'px' }"
            >
              <IssueRow
                v-for="v in virtualRows"
                :key="v.key as number"
                :item="rows[v.index]"
                :top="v.start"
                :selected="selectedIds.has(rows[v.index].id)"
                :active="cursorId === rows[v.index].id"
                @toggle="toggle(rows[v.index])"
                @open="open(rows[v.index])"
              />
            </div>
          </div>
        </div>
      </div>

      <Transition name="bulk">
        <div
          v-if="selected.length"
          class="bulk"
          role="toolbar"
          :aria-label="t('issues.bulkAria')"
        >
          <span class="bulk-count"><b class="mono">{{ selected.length }}</b> {{ t('words.selected', selected.length) }}</span>
          <span v-tooltip.top="t('issues.agentSoon')">
            <Button
              :label="t('issues.sendToAgent')"
              icon="pi pi-sparkles"
              disabled
            />
          </span>
          <Button
            :label="t('issues.markRead')"
            icon="pi pi-eye"
            severity="secondary"
            :disabled="!selected.some((i) => i.unread)"
            @click="markSelectedRead"
          />
          <Button
            v-tooltip.top="t('issues.clearTip')"
            icon="pi pi-times"
            severity="secondary"
            text
            rounded
            :aria-label="t('issues.clearAria')"
            @click="selected = []"
          />
        </div>
      </Transition>
    </template>
  </div>
</template>

<style scoped>
.counters {
  display: flex;
  gap: 18px;
  color: var(--iw-muted);
  font-size: 13px;
}

.counter {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.counter b {
  color: var(--iw-text);
  font-weight: 600;
}

.c-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
}

.c-dot.open {
  background: var(--iw-success);
}

.c-dot.closed {
  background: var(--iw-dimmed);
}

.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  padding: 12px;
}

.f-search {
  flex: 1 1 220px;
  min-width: 200px;
}

.f-source {
  width: 140px;
}

.f-repo {
  width: 200px;
}

.f-label {
  width: 150px;
}

.table-panel {
  overflow: hidden;
}

/* Virtual list: header + scroll body share one grid; every row is --row high. */
.vt {
  --cols: 2.25rem minmax(0, 1fr) 220px 190px 110px 120px;
}

.vt-head {
  display: grid;
  grid-template-columns: var(--cols);
  align-items: center;
  column-gap: 12px;
  height: 44px;
  padding: 0 16px 0 12px;
  border-bottom: 1px solid var(--iw-border);
  font-size: 12px;
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--iw-muted);
  background: var(--iw-surface);
}

.vt-head .c-sel {
  display: grid;
  place-items: center;
}

.vt-head input {
  width: 16px;
  height: 16px;
  accent-color: var(--iw-primary);
  cursor: pointer;
}

.vt-body {
  height: calc(100vh - 300px);
  min-height: 240px;
  overflow-y: auto;
  overscroll-behavior: contain;
}

.vt-space {
  position: relative;
  width: 100%;
}

.table-panel {
  position: relative;
}

.f-total {
  margin-left: auto;
  color: var(--iw-muted);
  font-size: 13px;
  white-space: nowrap;
}

.f-total b {
  color: var(--iw-text);
}

.new-pill {
  position: absolute;
  top: 52px;
  left: 50%;
  z-index: 5;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 14px;
  transform: translateX(-50%);
  border: 0;
  border-radius: 999px;
  font: inherit;
  font-size: 13px;
  font-weight: 600;
  color: var(--iw-on-primary);
  background: var(--iw-primary);
  box-shadow: var(--iw-shadow);
  cursor: pointer;
}

.bulk {
  position: fixed;
  left: 50%;
  bottom: 24px;
  z-index: 30;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px 10px 18px;
  transform: translateX(-50%);
  border-radius: 14px;
  background: var(--iw-elevated);
  border: 1px solid var(--iw-border-strong);
  box-shadow: var(--iw-shadow);
}

.bulk-count {
  margin-right: 6px;
  color: var(--iw-muted);
}

.bulk-count b {
  color: var(--iw-text);
}

.bulk-enter-active,
.bulk-leave-active {
  transition: opacity 160ms ease, transform 160ms ease;
}

.bulk-enter-from,
.bulk-leave-to {
  opacity: 0;
  transform: translate(-50%, 12px);
}

@media (width <= 1023px) {
  .vt {
    --cols: 2.25rem minmax(0, 1fr) 200px 100px 110px;
  }

  .vt-head .c-labels {
    display: none;
  }
}

@media (width <= 767px) {
  .vt {
    --cols: 2.25rem minmax(0, 1fr) 100px;
  }

  .vt-head .c-project,
  .vt-head .c-num {
    display: none;
  }
}</style>
