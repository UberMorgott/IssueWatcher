<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
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
import { api } from '../api/client'
import type { Repo, Stats } from '../api/types'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { absTime, relTime, repoColor, repoOwner, shortDay, shortRepo } from '../lib/format'

const app = useAppStore()
const { t } = useI18n()
const filter = ref('')
const expanded = ref<Record<string, boolean>>({})
const repoStats = reactive<Record<number, Stats | 'loading' | 'error'>>({})

const rows = computed(() => {
  const f = filter.value.trim().toLowerCase()
  return f ? app.repos.filter((r) => r.name.toLowerCase().includes(f)) : app.repos
})
const totals = computed(() => app.repos.reduce((a, r) => ({ open: a.open + r.open, closed: a.closed + r.closed }), { open: 0, closed: 0 }))

async function onExpand(e: { data: Repo }) {
  const id = e.data.id
  if (repoStats[id] && repoStats[id] !== 'error') return
  repoStats[id] = 'loading'
  const r = await api.stats(id)
  repoStats[id] = r.ok ? r.data : 'error'
}

// Live data: the list comes from the store (refreshed there); open charts refetch quietly.
watch(
  () => app.dataVersion,
  () => {
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

const closedShare = (r: Repo) => (r.open + r.closed ? Math.round((r.closed / (r.open + r.closed)) * 100) : 0)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">
          {{ t('nav.projects') }}
        </h2>
        <p class="page-sub">
          {{ t('projects.sub') }}
        </p>
      </div>
      <div
        v-if="app.repos.length"
        class="summary muted"
      >
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
        <span v-tooltip.bottom="t('projects.discoverTip')">
          <Button
            :label="t('projects.discover')"
            icon="pi pi-folder-open"
            severity="secondary"
            outlined
            disabled
          />
        </span>
      </div>

      <div class="panel table-panel">
        <DataTable
          v-model:expanded-rows="expanded"
          class="iw-projects"
          :value="rows"
          data-key="id"
          :loading="!app.reposLoaded"
          sort-field="open"
          :sort-order="-1"
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
              v-else-if="app.reposLoaded"
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
                    {{ shortRepo(data.name) }}
                  </RouterLink>
                  <span class="owner">{{ repoOwner(data.name) }}</span>
                </div>
              </div>
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
              <div class="folder-cell">
                <span
                  v-if="data.localPath"
                  v-tooltip.top="data.localPath"
                  class="mapped mono"
                ><i class="pi pi-folder-open" /> {{ data.localPath }}</span>
                <span
                  v-else
                  class="not-mapped"
                ><i class="pi pi-folder" /> {{ t('projects.notMapped') }}</span>
                <span v-tooltip.top="t('projects.mappingSoon')">
                  <Button
                    :label="t('projects.choose')"
                    size="small"
                    severity="secondary"
                    text
                    disabled
                  />
                </span>
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
                  :href="data.url"
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
      </div>
    </template>
  </div>
</template>

<style scoped>
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
  font-size: 12px;
  color: var(--iw-dimmed);
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
  font-size: 12px;
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
  font-size: 12px;
  color: var(--iw-text);
}

.not-mapped {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--iw-dimmed);
  font-size: 13px;
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
  font-size: 11.5px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--iw-dimmed);
}

.exp-val {
  font-size: 20px;
  font-weight: 650;
}

:deep(.iw-projects .p-datatable-thead > tr > th) {
  font-size: 12px;
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
