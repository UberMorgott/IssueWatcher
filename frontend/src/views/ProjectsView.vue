<script setup lang="ts">
import { safeUrl } from '../lib/safeUrl'
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import DataTable from 'primevue/datatable'
import Column from 'primevue/column'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import IconField from 'primevue/iconfield'
import InputIcon from 'primevue/inputicon'
import Skeleton from 'primevue/skeleton'
import type { EChartsCoreOption } from 'echarts/core'
import EChart, { type ChartTheme } from '../components/EChart.vue'
import EmptyState from '../components/EmptyState.vue'
import ConnectHero from '../components/ConnectHero.vue'
import PlatformIcon from '../components/PlatformIcon.vue'
import FolderDialog from '../components/FolderDialog.vue'
import LinkDialog from '../components/LinkDialog.vue'
import { isModPlatform, platformName } from '../lib/platforms'
import { api } from '../api/client'
import type { Repo, Stats } from '../api/types'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { absTime, relTime, repoColor, repoOwner, shortDay, shortRepo } from '../lib/format'
import { useChunks } from '../lib/chunks'
import type { DataTableSortEvent } from 'primevue/datatable'
import { useTriage } from '../lib/jobs'
import { useSettingsStore } from '../stores/settings'

const app = useAppStore()
const settings = useSettingsStore()
const triage = useTriage()
/** Fix jobs a triage of project queues (per-project override over the global value). */
function triageTopN(key: string): number {
  const ag = settings.doc?.settings.agents
  return ag?.projects[key]?.triageTopN ?? ag?.triageTopN ?? 3
}
const { t } = useI18n()
const filter = ref('')
const expanded = ref<Record<string, boolean>>({})
const repoStats = reactive<Record<number, Stats | 'loading' | 'error'>>({})

// Server-sorted keyset chunks (sort/filter changes start over); more rows load
// when the sentinel below the table comes within ~2 screens.
const sortField = ref('open')
const sortOrder = ref<1 | -1>(-1)
const list = useChunks<Repo>((cursor) => api.reposChunk(sortField.value, sortOrder.value < 0, filter.value.trim(), cursor))
const rows = list.items
const listLoading = list.loading
function reload() {
  list.reset()
  void list.loadMore()
}
function onSort(e: DataTableSortEvent) {
  sortField.value = typeof e.sortField === 'string' ? e.sortField : 'open'
  sortOrder.value = e.sortOrder === 1 ? 1 : -1
  reload()
}
let filterTimer: number | undefined
watch(filter, () => {
  window.clearTimeout(filterTimer)
  filterTimer = window.setTimeout(reload, 250)
})
const sentinel = ref<HTMLElement | null>(null)
let observer: IntersectionObserver | undefined
onMounted(() => {
  reload()
  observer = new IntersectionObserver((e) => {
    if (e.some((x) => x.isIntersecting)) void list.loadMore()
  }, { rootMargin: '0px 0px 1600px 0px' })
  if (sentinel.value) observer.observe(sentinel.value)
})
watch(sentinel, (el) => {
  observer?.disconnect()
  if (el) observer?.observe(el)
})
onBeforeUnmount(() => observer?.disconnect())
const totals = computed(() => app.repos.reduce((a, r) => ({ open: a.open + r.open, closed: a.closed + r.closed }), { open: 0, closed: 0 }))

async function onExpand(e: { data: Repo }) {
  const id = e.data.id
  if (repoStats[id] && repoStats[id] !== 'error') return
  repoStats[id] = 'loading'
  const r = await api.stats(id)
  repoStats[id] = r.ok ? r.data : 'error'
}

// Live data: re-read the loaded range in place (same sort, no scroll jump); open charts refetch quietly.
watch(
  () => app.dataVersion,
  async () => {
    const n = rows.value.length
    if (n) {
      const r = await api.reposChunk(sortField.value, sortOrder.value < 0, filter.value.trim(), '', Math.min(Math.max(n, 50), 1000))
      if (r.ok) rows.value = r.data.items
    }
    for (const k of Object.keys(expanded.value)) {
      const id = Number(k)
      void api.stats(id).then((r) => {
        if (r.ok) repoStats[id] = r.data
      })
    }
  },
)

function chart(repo: Repo, s: Stats) {
  const names = { opened: t('overview.opened'), closed: t('overview.closed') }
  return (c: ChartTheme): EChartsCoreOption => ({
    grid: { left: 30, right: 8, top: 10, bottom: 22 },
    tooltip: { trigger: 'axis', backgroundColor: c.surface, borderColor: c.border, textStyle: { color: c.text } },
    xAxis: { type: 'category', data: s.weekly.map((w) => shortDay(w.start)), axisLine: { lineStyle: { color: c.border } }, axisTick: { show: false }, axisLabel: { color: c.muted, interval: 4 } },
    yAxis: { type: 'value', minInterval: 1, splitLine: { lineStyle: { color: c.border, type: 'dashed' } }, axisLabel: { color: c.muted } },
    series: [
      { name: names.opened, type: 'line', smooth: true, symbol: 'none', data: s.weekly.map((w) => w.opened), lineStyle: { color: repoColor(repo.id), width: 2 }, itemStyle: { color: repoColor(repo.id) }, areaStyle: { color: repoColor(repo.id), opacity: 0.12 } },
      { name: names.closed, type: 'line', smooth: true, symbol: 'none', data: s.weekly.map((w) => w.closed), lineStyle: { color: c.closed, width: 2, type: 'dashed' }, itemStyle: { color: c.closed } },
    ],
  })
}

// «Выбрать» / «Изменить» in the folder column: the shared folder dialog.
const folderOpen = ref(false)
const folderProject = ref<Repo | null>(null)
function mapFolder(r: Repo) {
  folderProject.value = r
  folderOpen.value = true
}

// «Площадки»: mod pages ↔ code project links.
const linkOpen = ref(false)
const linkProject = ref<Repo | null>(null)
function openLinks(r: Repo) {
  linkProject.value = app.repos.find((x) => x.id === r.id) ?? r // freshest links
  linkOpen.value = true
}
const repoById = computed(() => new Map(app.repos.map((r) => [r.id, r])))
/** A project's current links (the live repo list is fresher than the loaded chunk). */
const linksOf = (r: Repo) => (repoById.value.get(r.id)?.links ?? r.links ?? []).map((id) => repoById.value.get(id)).filter((x): x is Repo => !!x)
const codeOf = (r: Repo) => {
  const id = repoById.value.get(r.id)?.linkedTo ?? r.linkedTo
  return id ? repoById.value.get(id) : undefined
}
const hasMods = computed(() => app.repos.some((r) => isModPlatform(r.platform)))

const closedShare = (r: Repo) => (r.open + r.closed ? Math.round((r.closed / (r.open + r.closed)) * 100) : 0)
</script>

<template>
  <div class="page">
    <div
      v-if="app.repos.length"
      class="page-head"
    >
      <div class="summary muted">
        <b class="mono">{{ app.repos.length }}</b> {{ t('words.projects', app.repos.length) }} · <b class="mono">{{ totals.open }}</b> {{ t('words.open', totals.open) }} · <b class="mono">{{ totals.closed }}</b> {{ t('words.closed', totals.closed) }}
      </div>
    </div>

    <ConnectHero v-if="app.onboarding" />

    <template v-else>
      <div class="toolbar">
        <IconField class="search">
          <InputIcon class="pi pi-search" />
          <InputText
            v-model="filter"
            :placeholder="t('projects.filter')"
            :aria-label="t('projects.filter')"
            fluid
          />
        </IconField>
        <Button
          v-tooltip.bottom="t('projects.discoverTip')"
          as="router-link"
          to="/settings/projects"
          :label="t('projects.discover')"
          icon="pi pi-folder-open"
          severity="secondary"
          outlined
        />
      </div>
      <FolderDialog
        v-model:visible="folderOpen"
        :project="folderProject"
      />
      <LinkDialog
        v-model:visible="linkOpen"
        :project="linkProject"
      />

      <div class="panel table-panel">
        <DataTable
          v-model:expanded-rows="expanded"
          class="iw-projects"
          :value="rows"
          data-key="id"
          lazy
          :sort-field="sortField"
          :sort-order="sortOrder"
          @sort="onSort"
          @row-expand="onExpand"
        >
          <template #empty>
            <EmptyState
              v-if="!app.reposAvailable"
              icon="pi pi-server"
              :title="t('projects.unavailable')"
              :text="t('projects.unavailableText')"
            />
            <EmptyState
              v-else-if="filter"
              icon="pi pi-filter"
              :title="t('projects.noMatch')"
              :text="t('projects.noMatchText', { filter })"
            />
            <EmptyState
              v-else-if="!listLoading"
              icon="pi pi-folder"
              :title="t('projects.empty')"
              :text="t('projects.emptyText')"
            >
              <Button
                :label="t('common.syncNow')"
                icon="pi pi-sync"
                size="small"
                :loading="app.syncing"
                :disabled="!app.githubConnected"
                @click="app.syncNow()"
              />
            </EmptyState>
          </template>

          <Column
            expander
            header-style="width: 3rem"
          />
          <Column
            field="name"
            :header="t('projects.colProject')"
            sortable
          >
            <template #body="{ data }: { data: Repo }">
              <div class="name-cell">
                <span
                  class="swatch"
                  :style="{ background: repoColor(data.id) }"
                />
                <PlatformIcon
                  :platform="data.platform || 'github'"
                  :size="16"
                />
                <div class="name-main">
                  <RouterLink
                    :to="{ name: 'issues', query: { repo: String(data.id) } }"
                    class="name"
                  >
                    {{ isModPlatform(data.platform) ? data.name : shortRepo(data.name) }}
                  </RouterLink>
                  <span class="owner">{{ isModPlatform(data.platform) ? platformName(data.platform) : repoOwner(data.name) }}</span>
                  <!-- «Площадки»: a code project's mod pages, a mod page's code project -->
                  <span
                    v-if="!isModPlatform(data.platform) && (linksOf(data).length || hasMods)"
                    class="chips"
                  >
                    <RouterLink
                      v-for="m in linksOf(data)"
                      :key="m.id"
                      v-tooltip.top="platformName(m.platform) + ' · ' + m.name"
                      :to="{ name: 'issues', query: { repo: String(m.id), state: 'all' } }"
                      class="chip"
                    ><PlatformIcon
                      :platform="m.platform"
                      :size="12"
                    /><span class="chip-name">{{ m.name }}</span></RouterLink>
                    <button
                      type="button"
                      class="chip add"
                      :aria-label="t('platforms.linkMods')"
                      @click.stop="openLinks(data)"
                    ><i class="pi pi-link" />{{ linksOf(data).length ? '' : t('platforms.mods') }}</button>
                  </span>
                  <span
                    v-else-if="isModPlatform(data.platform)"
                    class="chips"
                  >
                    <RouterLink
                      v-if="codeOf(data)"
                      :to="{ name: 'issues', query: { repo: String(codeOf(data)?.id), state: 'all' } }"
                      class="chip"
                    ><PlatformIcon
                      platform="github"
                      :size="12"
                    /><span class="chip-name">{{ codeOf(data)?.name }}</span></RouterLink>
                    <button
                      type="button"
                      class="chip add"
                      :aria-label="t('platforms.linkedCode')"
                      @click.stop="openLinks(data)"
                    ><i :class="codeOf(data) ? 'pi pi-pencil' : 'pi pi-link'" />{{ codeOf(data) ? '' : t('platforms.linkShort') }}</button>
                  </span>
                </div>
              </div>
            </template>
          </Column>
          <Column class="actions-col">
            <template #body="{ data }: { data: Repo }">
              <span
                v-if="!isModPlatform(data.platform)"
                v-tooltip.top="data.localPath ? t('jobs.triage.runTip', { n: triageTopN(data.key) }) : t('folder.needed')"
              >
                <Button
                  :label="t('jobs.triage.run')"
                  icon="pi pi-sort-amount-down"
                  size="small"
                  severity="secondary"
                  text
                  class="nowrap"
                  :loading="triage.busy.value"
                  :disabled="!data.open || !data.localPath"
                  @click.stop="triage.run(data.id, data.name)"
                />
              </span>
            </template>
          </Column>
          <Column
            field="open"
            :header="t('projects.colOpen')"
            sortable
            class="num"
          >
            <template #body="{ data }: { data: Repo }">
              <span class="mono strong">{{ data.open }}</span>
            </template>
          </Column>
          <Column
            field="closed"
            :header="t('projects.colClosed')"
            sortable
            class="num"
          >
            <template #body="{ data }: { data: Repo }">
              <span class="mono muted">{{ data.closed }}</span>
            </template>
          </Column>
          <Column
            :header="t('projects.colProgress')"
            class="progress-col"
          >
            <template #body="{ data }: { data: Repo }">
              <div
                v-tooltip.top="t('projects.closedPct', { n: closedShare(data) })"
                class="bar"
              >
                <span :style="{ width: closedShare(data) + '%' }" />
              </div>
            </template>
          </Column>
          <Column
            field="unread"
            :header="t('projects.colUnread')"
            sortable
            class="num"
          >
            <template #body="{ data }: { data: Repo }">
              <RouterLink
                v-if="data.unread"
                :to="{ name: 'issues', query: { repo: String(data.id), unread: '1', state: 'all' } }"
                class="unread-chip mono"
              >
                {{ data.unread }}
              </RouterLink>
              <span
                v-else
                class="muted"
              >—</span>
            </template>
          </Column>
          <Column :header="t('projects.colFolder')">
            <template #body="{ data }: { data: Repo }">
              <div
                v-if="isModPlatform(data.platform)"
                class="folder-cell"
              >
                <span
                  v-if="codeOf(data)?.localPath"
                  v-tooltip.top="codeOf(data)?.localPath"
                  class="mapped mono"
                ><i class="pi pi-folder-open" /> {{ t('platforms.viaCode', { name: codeOf(data)?.name }) }}</span>
                <span
                  v-else
                  class="not-mapped"
                ><i class="pi pi-folder" /> {{ codeOf(data) ? t('projects.notMapped') : t('platforms.notLinked') }}</span>
              </div>
              <div
                v-else
                class="folder-cell"
              >
                <span
                  v-if="data.localPath"
                  v-tooltip.top="data.localPath"
                  class="mapped mono"
                ><i class="pi pi-folder-open" /> {{ data.localPath }}</span>
                <span
                  v-else
                  class="not-mapped"
                ><i class="pi pi-folder" /> {{ t('projects.notMapped') }}</span>
                <Button
                  :label="data.localPath ? t('projects.change') : t('projects.choose')"
                  size="small"
                  severity="secondary"
                  text
                  @click.stop="mapFolder(data)"
                />
              </div>
            </template>
          </Column>
          <Column
            field="lastSync"
            :header="t('projects.colLastSync')"
            sortable
          >
            <template #body="{ data }: { data: Repo }">
              <span
                v-tooltip.left="absTime(data.lastSync ?? data.syncedAt)"
                class="muted nowrap"
              >{{ relTime(data.lastSync ?? data.syncedAt) || t('common.never') }}</span>
            </template>
          </Column>

          <template #expansion="{ data }: { data: Repo }">
            <div class="expansion">
              <div class="exp-stats">
                <div>
                  <div class="exp-label">
                    {{ t('projects.colOpen') }}
                  </div><div class="exp-val mono">
                    {{ data.open }}
                  </div>
                </div>
                <div>
                  <div class="exp-label">
                    {{ t('projects.colClosed') }}
                  </div><div class="exp-val mono">
                    {{ data.closed }}
                  </div>
                </div>
                <div>
                  <div class="exp-label">
                    {{ t('projects.closedShare') }}
                  </div><div class="exp-val mono">
                    {{ closedShare(data) }}%
                  </div>
                </div>
                <Button
                  as="a"
                  :href="safeUrl(data.url)"
                  target="_blank"
                  rel="noopener noreferrer"
                  :label="t('projects.openRepo')"
                  icon="pi pi-external-link"
                  size="small"
                  severity="secondary"
                  outlined
                />
              </div>
              <div class="exp-chart">
                <Skeleton
                  v-if="repoStats[data.id] === 'loading'"
                  height="180px"
                />
                <EmptyState
                  v-else-if="repoStats[data.id] === 'error' || !repoStats[data.id]"
                  icon="pi pi-chart-line"
                  :title="t('projects.noStats')"
                  compact
                />
                <EChart
                  v-else
                  :option="chart(data, repoStats[data.id] as Stats)"
                  height="180px"
                  :label="t('projects.weeklyFor', { name: data.name })"
                />
              </div>
            </div>
          </template>
        </DataTable>
        <div
          v-if="listLoading"
          class="tail"
        >
          <Skeleton
            v-for="i in 3"
            :key="i"
            height="40px"
          />
        </div>
        <div
          ref="sentinel"
          aria-hidden="true"
        />
      </div>
    </template>
  </div>
</template>

<style scoped>
.tail {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 8px 12px 12px;
}

.summary b {
  color: var(--iw-text);
}

.toolbar {
  display: flex;
  gap: 12px;
  align-items: center;
  flex-wrap: wrap;
}

.search {
  flex: 0 1 360px;
}

.table-panel {
  overflow: hidden;
}

.name-cell {
  display: flex;
  align-items: center;
  gap: 10px;
}

.swatch {
  width: 4px;
  height: 28px;
  border-radius: 2px;
}

.name-main {
  display: flex;
  flex-direction: column;
  min-width: 220px;
  line-height: 1.3;
}

.name {
  font-weight: 600;
  color: var(--iw-text);
}

.name:hover {
  color: var(--iw-primary);
}

.owner {
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-dimmed);
}

.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  margin-top: 4px;
}

.chip {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  max-width: 220px;
  padding: 1px 8px;
  border-radius: 999px;
  border: 1px solid var(--iw-border-strong);
  background: var(--iw-elevated);
  font: inherit;
  font-size: calc(11.5px * var(--iw-fs, 1));
  color: var(--iw-text);
  white-space: nowrap;
  cursor: pointer;
}

.chip:hover {
  border-color: var(--iw-primary);
  color: var(--iw-text);
}

.chip-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.chip.add {
  color: var(--iw-muted);
  border-style: dashed;
}

.strong {
  font-weight: 600;
}

.nowrap {
  white-space: nowrap;
}

.bar {
  width: 120px;
  height: 6px;
  border-radius: 3px;
  background: var(--iw-elevated);
  overflow: hidden;
}

.bar span {
  display: block;
  height: 100%;
  border-radius: 3px;
  background: var(--iw-success);
}

.unread-chip {
  display: inline-block;
  min-width: 26px;
  padding: 1px 8px;
  border-radius: 999px;
  text-align: center;
  font-size: calc(12px * var(--iw-fs, 1));
  font-weight: 600;
  color: var(--iw-on-primary);
  background: var(--iw-primary);
}

.folder-cell {
  display: flex;
  align-items: center;
  gap: 6px;
  white-space: nowrap;
}

.mapped {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-text);
}

.not-mapped {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--iw-dimmed);
  font-size: calc(13px * var(--iw-fs, 1));
}

.expansion {
  display: grid;
  grid-template-columns: 220px minmax(0, 1fr);
  gap: 24px;
  padding: 8px 8px 8px 48px;
}

.exp-stats {
  display: flex;
  flex-direction: column;
  gap: 12px;
  align-items: flex-start;
}

.exp-label {
  font-size: calc(11.5px * var(--iw-fs, 1));
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--iw-dimmed);
}

.exp-val {
  font-size: calc(20px * var(--iw-fs, 1));
  font-weight: 650;
}

:deep(.iw-projects .p-datatable-thead > tr > th) {
  font-size: calc(12px * var(--iw-fs, 1));
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--iw-muted);
  background: var(--iw-surface);
}

:deep(.iw-projects .p-datatable-tbody > tr:not(.p-datatable-row-expansion):hover) {
  background: var(--iw-hover);
}

:deep(.iw-projects .p-datatable-row-expansion) {
  background: var(--iw-bg);
}

:deep(.num) {
  width: 90px;
}

@media (width <= 1023px) {
  :deep(.progress-col) {
    display: none;
  }

  .expansion {
    grid-template-columns: minmax(0, 1fr);
    padding-left: 8px;
  }
}
</style>
