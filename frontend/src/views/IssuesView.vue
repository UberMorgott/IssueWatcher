<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter, type LocationQueryRaw } from 'vue-router'
import DataTable, { type DataTablePageEvent } from 'primevue/datatable'
import Column from 'primevue/column'
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
import LabelTag from '../components/LabelTag.vue'
import PlatformIcon from '../components/PlatformIcon.vue'
import { api } from '../api/client'
import type { Issue } from '../api/types'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { absTime, relTime, repoOwner, shortRepo } from '../lib/format'
import { typing } from '../lib/shortcuts'

const app = useAppStore()
const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const toast = useToast()

const PER_PAGE = 50

// Filters live in the URL so a filtered view can be bookmarked and survives reloads.
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const filters = computed(() => ({
  source: str(route.query.source),
  repo: Number(str(route.query.repo)) || 0,
  state: (['open', 'closed', 'all'].includes(str(route.query.state)) ? str(route.query.state) : 'open') as 'open' | 'closed' | 'all',
  label: str(route.query.label),
  q: str(route.query.q),
  unread: route.query.unread === '1',
  page: Math.max(1, Number(str(route.query.page)) || 1),
}))

function setQuery(patch: Partial<Record<'source' | 'repo' | 'state' | 'label' | 'q' | 'unread' | 'page', string | number | boolean | null>>) {
  const q: LocationQueryRaw = { ...route.query }
  delete q.focus
  for (const [k, v] of Object.entries(patch)) {
    if (v === null || v === '' || v === false || v === 0) delete q[k]
    else q[k] = v === true ? '1' : String(v)
  }
  if (!('page' in patch)) delete q.page
  void router.replace({ query: q })
}

const items = ref<Issue[]>([])
const total = ref(0)
const loading = ref(true)
const state = ref<'ok' | 'unavailable' | 'error'>('ok')
const errorText = ref('')
const selected = ref<Issue[]>([])
const cursor = ref(-1)
const seenLabels = ref(new Set<string>())

/** quiet = live refresh: no loading overlay; selection, cursor and scroll stay. */
async function load(quiet = false) {
  if (app.onboarding) {
    loading.value = false
    return
  }
  if (!quiet) loading.value = true
  const f = filters.value
  const r = await api.issues({ source: f.source, repo: f.repo, state: f.state, label: f.label, q: f.q, unread: f.unread, page: f.page, perPage: PER_PAGE })
  loading.value = false
  if (!r.ok) {
    state.value = r.status === 404 ? 'unavailable' : 'error'
    errorText.value = r.error
    items.value = []
    total.value = 0
    return
  }
  state.value = 'ok'
  items.value = r.data.items
  total.value = r.data.total
  // Keep the selection but point it at the fresh rows (unread flags may have changed).
  const byId = new Map(r.data.items.map((it) => [it.id, it]))
  selected.value = selected.value.map((s) => byId.get(s.id) ?? s)
  for (const it of r.data.items) for (const l of it.labels) seenLabels.value.add(l)
  if (cursor.value >= items.value.length) cursor.value = items.value.length - 1
}

watch(filters, () => load(), { deep: true })
watch(() => app.onboarding, () => load())
watch(() => app.dataVersion, () => load(true))

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
// PrimeVue fills {first}/{last}/{totalRecords} in; the braces must survive translation.
const pageReport = computed(() => t('issues.pageReport', { first: '{first}', last: '{last}', total: '{totalRecords}' }))
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

function onPage(e: DataTablePageEvent) {
  setQuery({ page: e.page + 1 })
}

function open(it: Issue) {
  void router.push(`/item/${it.id}`)
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
  app.invalidate() // coalesces with the server's data.changed
}

// --- keyboard: J/K move, Enter opens, X selects
const searchBox = ref<{ $el: HTMLElement } | null>(null)
function focusSearch() {
  void nextTick(() => searchBox.value?.$el?.querySelector?.('input')?.focus() ?? (searchBox.value?.$el as HTMLInputElement | undefined)?.focus())
}
function onKey(e: KeyboardEvent) {
  if (e.ctrlKey || e.metaKey || e.altKey || typing(e) || !items.value.length) return
  const k = e.key.toLowerCase()
  if (k === 'j' || k === 'k') {
    e.preventDefault()
    cursor.value = Math.min(items.value.length - 1, Math.max(0, cursor.value + (k === 'j' ? 1 : -1)))
    void nextTick(() => document.querySelector('.iw-issues tr.kbd-cursor')?.scrollIntoView({ block: 'nearest' }))
  } else if (k === 'enter' && cursor.value >= 0) {
    open(items.value[cursor.value])
  } else if (k === 'x' && cursor.value >= 0) {
    const it = items.value[cursor.value]
    selected.value = selected.value.some((s) => s.id === it.id) ? selected.value.filter((s) => s.id !== it.id) : [...selected.value, it]
  } else if (k === 'escape' && selected.value.length) {
    selected.value = []
  }
}
const rowClass = (it: Issue) => [it.unread ? 'is-unread' : '', items.value[cursor.value]?.id === it.id ? 'kbd-cursor' : '']

onMounted(() => {
  void load()
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
      </div>

      <div class="panel table-panel">
        <DataTable
          v-model:selection="selected"
          class="iw-issues"
          :value="items"
          data-key="id"
          lazy
          paginator
          :rows="PER_PAGE"
          :first="(filters.page - 1) * PER_PAGE"
          :total-records="total"
          :loading="loading"
          :row-class="rowClass"
          scrollable
          scroll-height="calc(100vh - 330px)"
          paginator-template="FirstPageLink PrevPageLink PageLinks NextPageLink LastPageLink CurrentPageReport"
          :current-page-report-template="pageReport"
          @page="onPage"
          @row-click="(e) => open(e.data as Issue)"
        >
          <template #empty>
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
              :text="errorText"
            >
              <Button
                :label="t('common.retry')"
                icon="pi pi-refresh"
                size="small"
                @click="load()"
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
              v-else-if="!loading"
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
          </template>

          <Column
            selection-mode="multiple"
            header-style="width: 3rem"
            frozen
          />
          <Column
            :header="t('issues.colIssue')"
            class="col-title"
          >
            <template #body="{ data }: { data: Issue }">
              <div class="t-cell">
                <span
                  class="state-icon"
                  :class="data.state"
                ><i :class="data.state === 'closed' ? 'pi pi-check-circle' : 'pi pi-circle'" /></span>
                <div class="t-main">
                  <div class="t-line">
                    <span
                      v-if="data.unread"
                      class="unread-dot"
                      :aria-label="t('issues.unreadAria')"
                    />
                    <span
                      v-tooltip.top="data.title.length > 80 ? data.title : undefined"
                      class="t-title"
                    >{{ data.title }}</span>
                  </div>
                  <div class="t-meta">
                    <span class="mono">#{{ data.number }}</span> {{ t('issues.byOpened', { author: data.author || t('common.unknown'), time: relTime(data.createdAt) }) }}
                  </div>
                </div>
              </div>
            </template>
          </Column>
          <Column
            :header="t('issues.colProject')"
            class="col-project"
          >
            <template #body="{ data }: { data: Issue }">
              <div class="p-cell">
                <PlatformIcon
                  platform="github"
                  :size="14"
                />
                <span class="p-name">
                  <span class="p-owner">{{ repoOwner(data.repo) }}/</span>{{ shortRepo(data.repo) }}
                </span>
              </div>
            </template>
          </Column>
          <Column
            :header="t('issues.colLabels')"
            class="col-labels"
          >
            <template #body="{ data }: { data: Issue }">
              <div class="l-cell">
                <LabelTag
                  v-for="l in data.labels.slice(0, 3)"
                  :key="l"
                  :name="l"
                />
                <span
                  v-if="data.labels.length > 3"
                  v-tooltip.top="data.labels.slice(3).join(', ')"
                  class="l-more"
                >+{{ data.labels.length - 3 }}</span>
              </div>
            </template>
          </Column>
          <Column
            :header="t('issues.colComments')"
            class="col-num"
          >
            <template #body="{ data }: { data: Issue }">
              <span
                class="c-cell"
                :class="{ zero: !data.comments }"
              ><i class="pi pi-comment" /> <span class="mono">{{ data.comments }}</span></span>
            </template>
          </Column>
          <Column
            :header="t('issues.colUpdated')"
            class="col-updated"
          >
            <template #body="{ data }: { data: Issue }">
              <span
                v-tooltip.left="absTime(data.updatedAt)"
                class="u-cell"
              >{{ relTime(data.updatedAt) }}</span>
            </template>
          </Column>
        </DataTable>
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

.t-cell {
  display: flex;
  gap: 10px;
  min-width: 0;
}

.state-icon {
  margin-top: 2px;
  font-size: 14px;
}

.state-icon.open {
  color: var(--iw-success);
}

.state-icon.closed {
  color: var(--iw-dimmed);
}

.t-main {
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
  max-width: 640px;
}

.t-meta {
  font-size: 12px;
  color: var(--iw-muted);
}

.p-cell {
  display: flex;
  align-items: center;
  gap: 8px;
  white-space: nowrap;
  color: var(--iw-text);
}

.p-owner {
  color: var(--iw-dimmed);
}

.l-cell {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.l-more {
  font-size: 12px;
  color: var(--iw-muted);
}

.c-cell {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--iw-muted);
}

.c-cell.zero {
  opacity: 0.5;
}

.u-cell {
  color: var(--iw-muted);
  white-space: nowrap;
}

:deep(.iw-issues .p-datatable-tbody > tr) {
  cursor: pointer;
  transition: background 120ms ease;
}

:deep(.iw-issues .p-datatable-tbody > tr:hover) {
  background: var(--iw-hover);
}

:deep(.iw-issues .p-datatable-tbody > tr.is-unread .t-title) {
  font-weight: 650;
}

:deep(.iw-issues .p-datatable-tbody > tr.kbd-cursor) {
  box-shadow: inset 3px 0 0 var(--iw-primary);
  background: var(--iw-primary-soft);
}

:deep(.iw-issues .p-datatable-thead > tr > th) {
  font-size: 12px;
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--iw-muted);
  background: var(--iw-surface);
}

:deep(.iw-issues .p-datatable-tbody > tr > td) {
  padding-top: 10px;
  padding-bottom: 10px;
  border-color: var(--iw-border);
}

:deep(.iw-issues .p-paginator) {
  border-top: 1px solid var(--iw-border);
  background: var(--iw-surface);
}

:deep(.col-num),
:deep(.col-updated) {
  width: 110px;
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
  :deep(.col-labels) {
    display: none;
  }
}

@media (width <= 767px) {
  :deep(.col-project),
  :deep(.col-num) {
    display: none;
  }
}
</style>
