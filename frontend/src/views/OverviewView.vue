<script setup lang="ts">
import { computed, onMounted, watch } from 'vue'
import Select from 'primevue/select'
import Button from 'primevue/button'
import Skeleton from 'primevue/skeleton'
import type { EChartsCoreOption } from 'echarts/core'
import StatCard from '../components/StatCard.vue'
import EChart, { type ChartTheme } from '../components/EChart.vue'
import EmptyState from '../components/EmptyState.vue'
import ConnectHero from '../components/ConnectHero.vue'
import PlatformTiles from '../components/PlatformTiles.vue'
import PlatformIcon from '../components/PlatformIcon.vue'
import { api } from '../api/client'
import type { Issue, Stats } from '../api/types'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { absTime, relTime, shortDay, shortRepo } from '../lib/format'
import { cachedRef } from '../lib/cache'
import { isModPlatform, repoPlatform } from '../lib/platforms'

const app = useAppStore()
const { t } = useI18n()

// Cached across visits: a revisit shows the last numbers at once and refetches quietly.
const stats = cachedRef<Stats | null>('overview.stats', null)
const statsState = cachedRef<'loading' | 'ok' | 'unavailable' | 'error'>('overview.statsState', 'loading')
const chartRepo = cachedRef<number | null>('overview.chartRepo', null)
const repoStats = cachedRef<Stats | null>('overview.repoStats', null)
const attention = cachedRef<Issue[]>('overview.attention', [])
const recent = cachedRef<Issue[]>('overview.recent', [])
const listsLoading = cachedRef('overview.listsLoading', true)

async function loadStats() {
  const r = await api.stats()
  if (r.ok) {
    stats.value = r.data
    statsState.value = 'ok'
  } else statsState.value = r.status === 404 ? 'unavailable' : 'error'
}

async function loadRepoStats() {
  if (!chartRepo.value) {
    repoStats.value = null
    return
  }
  const r = await api.stats(chartRepo.value)
  repoStats.value = r.ok ? r.data : null
}

const ATTENTION = 6
const attentionTotal = cachedRef('overview.attentionTotal', 0)
async function loadLists() {
  const [a, b] = await Promise.all([api.issues({ unread: true, state: 'all', limit: ATTENTION }), api.issues({ state: 'all', limit: 8 })])
  attention.value = a.ok ? a.data.items : []
  attentionTotal.value = a.ok ? (a.data.total ?? a.data.items.length) : 0
  recent.value = b.ok ? b.data.items : []
  listsLoading.value = false
}

function loadAll() {
  if (app.onboarding) return
  void loadStats()
  void loadLists()
  void loadRepoStats()
}

onMounted(loadAll)
watch(() => [app.onboarding, app.dataVersion], loadAll)
watch(chartRepo, loadRepoStats)

const showHero = computed(() => app.onboarding)
/** Mod pages synced: unread comments get their own counter (and link) beside unread issues. */
const hasComments = computed(() => app.repos.some((r) => isModPlatform(r.platform)))
const shown = computed(() => repoStats.value ?? stats.value)
// Issues and bug reports only: comment threads have their own tile and chart series.
const closed26 = computed(() => stats.value?.weekly.reduce((n, w) => n + w.closed, 0) ?? null)
const opened26 = computed(() => stats.value?.weekly.reduce((n, w) => n + w.opened, 0) ?? null)
const repoOptions = computed(() => [
  { label: t('overview.allProjects'), value: null as number | null, platform: '' },
  ...app.repos.map((r) => ({ label: r.name, value: r.id as number | null, platform: r.platform })),
])
const selectedChartRepo = computed(() => repoOptions.value.find((o) => o.value === chartRepo.value))
const topRepos = computed(() =>
  [...app.repos]
    .filter((r) => r.open > 0)
    .sort((a, b) => b.open - a.open)
    .slice(0, 8),
)
/** Item reference in the Overview lists: owner/repo#12, or the mod page's name (its numbers are internal ids). */
const refLabel = (it: { repo: string; number: number; platform?: string }) => (isModPlatform(it.platform ?? '') ? shortRepo(it.repo) : `${shortRepo(it.repo)}#${it.number}`)

const weeklyOption = computed(() => {
  const weekly = shown.value?.weekly ?? []
  const names = { opened: t('overview.opened'), closed: t('overview.closed'), comments: t('overview.threadsSeries') }
  const withComments = hasComments.value
  const days = weekly.map((w) => shortDay(w.start))
  return (c: ChartTheme): EChartsCoreOption => ({
    grid: { left: 36, right: 12, top: 36, bottom: 28 },
    legend: { top: 0, right: 0, icon: 'roundRect', itemWidth: 10, itemHeight: 10, textStyle: { color: c.muted } },
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'shadow', shadowStyle: { color: 'rgba(134,165,255,0.06)' } },
      backgroundColor: c.surface,
      borderColor: c.border,
      textStyle: { color: c.text },
    },
    xAxis: {
      type: 'category',
      data: days,
      axisLine: { lineStyle: { color: c.border } },
      axisTick: { show: false },
      axisLabel: { color: c.muted, interval: 3 },
    },
    yAxis: { type: 'value', minInterval: 1, splitLine: { lineStyle: { color: c.border, type: 'dashed' } }, axisLabel: { color: c.muted } },
    series: [
      { name: names.opened, type: 'bar', data: weekly.map((w) => w.opened), itemStyle: { color: c.opened, borderRadius: [4, 4, 0, 0] }, barGap: '15%', barMaxWidth: 14 },
      { name: names.closed, type: 'bar', data: weekly.map((w) => w.closed), itemStyle: { color: c.closed, borderRadius: [4, 4, 0, 0] }, barMaxWidth: 14 },
      ...(withComments
        ? [{ name: names.comments, type: 'bar', data: weekly.map((w) => w.comments ?? 0), itemStyle: { color: c.warn, borderRadius: [4, 4, 0, 0] }, barMaxWidth: 14 }]
        : []),
    ],
  })
})

const escHtml = (s: string) => s.replace(/[&<>"]/g, (ch) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[ch] ?? ch)

/** Open items per project, split by kind: issues (incl. bug reports) and mod page comment threads. */
const reposOption = computed(() => {
  const rows = [...topRepos.value].reverse().map((r) => {
    const comments = r.openComments ?? 0
    return { name: shortRepo(r.name), issues: r.open - comments, comments, total: r.open }
  })
  const names = { issues: t('overview.issuesSeries'), comments: t('overview.commentsSeries'), total: t('overview.totalSeries') }
  const withComments = hasComments.value
  const end = [0, 4, 4, 0]
  return (c: ChartTheme): EChartsCoreOption => ({
    grid: { left: 8, right: 36, top: withComments ? 28 : 8, bottom: 8, containLabel: true },
    legend: withComments ? { top: 0, right: 0, icon: 'roundRect', itemWidth: 10, itemHeight: 10, textStyle: { color: c.muted } } : undefined,
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'none' },
      backgroundColor: c.surface,
      borderColor: c.border,
      textStyle: { color: c.text },
      formatter: (ps: { dataIndex: number }[]) => {
        const r = rows[ps[0]?.dataIndex ?? 0]
        if (!r) return ''
        const dot = (col: string) => `<span style="display:inline-block;width:8px;height:8px;border-radius:2px;margin-right:6px;background:${col}"></span>`
        const lines = [`<b>${escHtml(r.name)}</b>`, `${dot(c.primary)}${escHtml(names.issues)}: ${r.issues}`]
        if (withComments) lines.push(`${dot(c.warn)}${escHtml(names.comments)}: ${r.comments}`, `${escHtml(names.total)}: ${r.total}`)
        return lines.join('<br>')
      },
    },
    xAxis: { type: 'value', show: false },
    yAxis: { type: 'category', data: rows.map((r) => r.name), axisLine: { show: false }, axisTick: { show: false }, axisLabel: { color: c.muted } },
    series: [
      {
        name: names.issues,
        type: 'bar',
        stack: 'open',
        data: rows.map((r) => ({ value: r.issues, itemStyle: { borderRadius: withComments && r.comments ? 0 : end } })),
        itemStyle: { color: c.primary },
        barMaxWidth: 16,
        label: withComments ? undefined : { show: true, position: 'right', color: c.muted },
      },
      ...(withComments
        ? [
            {
              name: names.comments,
              type: 'bar',
              stack: 'open',
              data: rows.map((r) => r.comments),
              itemStyle: { color: c.warn, borderRadius: end },
              barMaxWidth: 16,
              // Label on the stack's last segment: shows the project's total at the bar end.
              label: { show: true, position: 'right', color: c.muted, formatter: (p: { dataIndex: number }) => String(rows[p.dataIndex]?.total ?? '') },
            },
          ]
        : []),
    ],
  })
})

const activityIcon: Record<string, string> = { 'item.new': 'pi pi-inbox', 'comment.new': 'pi pi-comment', 'item.closed': 'pi pi-check-circle' }
const activityKey: Record<string, string> = { 'item.new': 'overview.activity.issue', 'comment.new': 'overview.activity.comment', 'item.closed': 'overview.activity.closed' }
/** Activity line by event and item kind: a new mod page thread or bug report is no «issue». */
function activityPath(event: string, kind?: string) {
  if (event === 'item.new' && kind === 'comment') return 'overview.activity.thread'
  if (event === 'item.new' && kind === 'bug') return 'overview.activity.bug'
  return activityKey[event]
}
</script>

<template>
  <div class="page">
    <template v-if="!app.authLoaded || !app.reposLoaded">
      <div class="stats-row">
        <Skeleton
          v-for="i in 4"
          :key="i"
          height="112px"
          border-radius="12px"
        />
      </div>
      <Skeleton
        height="340px"
        border-radius="12px"
      />
    </template>

    <template v-else-if="showHero">
      <ConnectHero />
      <PlatformTiles />
    </template>

    <template v-else>
      <div
        class="stats-row"
        :class="{ five: hasComments }"
      >
        <StatCard
          :label="t('overview.statOpen')"
          :value="stats?.open ?? null"
          icon="pi pi-inbox"
          :loading="statsState === 'loading'"
          :hint="t('overview.openHint')"
          :to="{ name: 'issues' }"
        />
        <StatCard
          :label="t('overview.statUnread')"
          :value="app.unreadIssues"
          icon="pi pi-bell"
          tone="warn"
          :loading="!app.reposLoaded"
          :hint="t('overview.unreadHint')"
          :to="{ name: 'issues', query: { unread: '1', state: 'all' } }"
        />
        <StatCard
          v-if="hasComments"
          :label="t('overview.statComments')"
          :value="app.unreadComments"
          icon="pi pi-comments"
          tone="warn"
          :loading="!app.reposLoaded"
          :hint="stats ? t('overview.commentsWaiting', { n: stats.openComments ?? 0 }) : t('overview.commentsHint')"
          :to="{ name: 'comments', query: { unread: '1', state: 'all' } }"
        />
        <StatCard
          :label="t('overview.opened26')"
          :value="opened26"
          icon="pi pi-arrow-up-right"
          tone="muted"
          :loading="statsState === 'loading'"
          :hint="t('overview.opened26Hint')"
        />
        <StatCard
          :label="t('overview.closed26')"
          :value="closed26"
          icon="pi pi-check-circle"
          tone="success"
          :loading="statsState === 'loading'"
          :hint="stats ? t('overview.closedAllTime', { n: stats.closed }) : undefined"
          :to="{ name: 'issues', query: { state: 'closed' } }"
        />
      </div>

      <PlatformTiles v-if="app.platforms.some((p) => p.id !== 'github' && p.enabled) || app.repos.some((r) => r.platform !== 'github')" />

      <div class="grid">
        <section class="panel chart-panel">
          <div class="panel-head">
            <span class="panel-title">{{ t('overview.chartTitle') }}</span>
            <Select
              v-model="chartRepo"
              :options="repoOptions"
              option-label="label"
              option-value="value"
              size="small"
              class="repo-select"
              filter
              :aria-label="t('overview.project')"
            >
              <template #value>
                <span class="opt"><PlatformIcon
                  v-if="selectedChartRepo?.platform"
                  :platform="selectedChartRepo.platform"
                  :size="14"
                />{{ selectedChartRepo?.label ?? t('overview.allProjects') }}</span>
              </template>
              <template #option="{ option }">
                <span class="opt"><PlatformIcon
                  v-if="option.platform"
                  :platform="option.platform"
                  :size="14"
                />{{ option.label }}</span>
              </template>
            </Select>
          </div>
          <div class="panel-body">
            <EmptyState
              v-if="statsState === 'unavailable' || statsState === 'error'"
              icon="pi pi-chart-bar"
              :title="statsState === 'unavailable' ? t('overview.statsUnavailable') : t('overview.statsError')"
              :text="t('overview.statsHint')"
              compact
            />
            <Skeleton
              v-else-if="statsState === 'loading'"
              height="300px"
            />
            <EChart
              v-else
              :option="weeklyOption"
              height="300px"
              :label="t('overview.chartLabel')"
            />
          </div>
        </section>

        <div class="side">
          <section class="panel">
            <div class="panel-head">
              <span class="panel-title">{{ t('overview.needsAttention') }}</span>
              <span class="more-links">
                <RouterLink
                  :to="{ name: 'issues', query: { unread: '1' } }"
                  class="more"
                ><template v-if="hasComments"><i class="pi pi-inbox" /> {{ t('nav.issues') }}</template><template v-else>{{ t('overview.allUnread') }}</template></RouterLink>
                <RouterLink
                  v-if="hasComments"
                  :to="{ name: 'comments', query: { unread: '1' } }"
                  class="more"
                ><i class="pi pi-comments" /> {{ t('overview.allComments') }}</RouterLink>
              </span>
            </div>
            <div class="panel-body list">
              <template v-if="listsLoading">
                <Skeleton
                  v-for="i in 3"
                  :key="i"
                  height="44px"
                />
              </template>
              <EmptyState
                v-else-if="!attention.length"
                icon="pi pi-check"
                :title="t('overview.caughtUp')"
                :text="t('overview.caughtUpText')"
                compact
              />
              <RouterLink
                v-for="it in attention"
                :key="it.id"
                :to="`/item/${it.id}`"
                class="row-link"
              >
                <span class="unread-dot" />
                <span class="row-main">
                  <span class="row-title">{{ it.title }}</span>
                  <span class="row-meta"><PlatformIcon
                    :platform="it.platform || 'github'"
                    :size="12"
                    class="meta-mark"
                  /><span class="mono">{{ refLabel(it) }}</span> · {{ relTime(it.updatedAt) }}</span>
                </span>
              </RouterLink>
              <div
                v-if="!listsLoading && attentionTotal > attention.length"
                class="more-row"
              >
                <RouterLink
                  :to="{ name: 'issues', query: { unread: '1', state: 'all' } }"
                  class="more"
                >
                  {{ t('overview.moreUnread', { n: attentionTotal - attention.length }) }}
                </RouterLink>
              </div>
            </div>
          </section>

          <section class="panel">
            <div class="panel-head">
              <span class="panel-title">{{ t('overview.recentActivity') }}</span>
              <span
                v-if="app.activity.length"
                class="live-pill"
              ><span class="live-dot" /> {{ t('common.live') }}</span>
            </div>
            <div class="panel-body list">
              <RouterLink
                v-for="a in app.activity.slice(0, 5)"
                :key="'a' + a.key"
                :to="`/item/${a.data.id}`"
                class="row-link"
              >
                <i
                  :class="a.kind === 'item.new' && a.data.kind === 'comment' ? 'pi pi-comments' : activityIcon[a.kind]"
                  class="row-icon"
                />
                <span class="row-main">
                  <span class="row-title">{{ a.data.title }}</span>
                  <span class="row-meta"><PlatformIcon
                    v-if="repoPlatform(a.data.repo, app.repos)"
                    :platform="repoPlatform(a.data.repo, app.repos)"
                    :size="12"
                    class="meta-mark"
                  /><i18n-t
                    :keypath="activityPath(a.kind, a.data.kind)"
                    scope="global"
                  ><template #actor>{{ a.data.actor || t('common.someone') }}</template><template #ref><span class="mono">{{ refLabel({ repo: a.data.repo, number: a.data.number, platform: repoPlatform(a.data.repo, app.repos) }) }}</span></template></i18n-t> · {{ relTime(a.at) }}</span>
                </span>
              </RouterLink>
              <template v-if="listsLoading && !app.activity.length">
                <Skeleton
                  v-for="i in 4"
                  :key="i"
                  height="44px"
                />
              </template>
              <EmptyState
                v-else-if="!recent.length && !app.activity.length"
                icon="pi pi-history"
                :title="t('overview.noActivity')"
                :text="t('overview.noActivityText')"
                compact
              />
              <RouterLink
                v-for="it in recent.slice(0, app.activity.length ? 3 : 6)"
                :key="it.id"
                v-tooltip.left="absTime(it.updatedAt)"
                :to="`/item/${it.id}`"
                class="row-link"
              >
                <i
                  class="row-icon"
                  :class="it.state === 'closed' ? 'pi pi-check-circle closed' : 'pi pi-circle open'"
                />
                <span class="row-main">
                  <span class="row-title">{{ it.title }}</span>
                  <span class="row-meta"><PlatformIcon
                    :platform="it.platform || 'github'"
                    :size="12"
                    class="meta-mark"
                  /><span class="mono">{{ refLabel(it) }}</span> · {{ t('overview.updated', { time: relTime(it.updatedAt) }) }}</span>
                </span>
              </RouterLink>
            </div>
          </section>
        </div>
      </div>

      <section class="panel">
        <div class="panel-head">
          <span class="panel-title">{{ t('overview.topProjects') }}</span>
          <RouterLink
            to="/projects"
            class="more"
          >
            {{ t('overview.allProjectsLink') }}
          </RouterLink>
        </div>
        <div class="panel-body">
          <EmptyState
            v-if="app.reposLoaded && !app.repos.length"
            icon="pi pi-folder"
            :title="t('overview.noProjects')"
            :text="t('overview.noProjectsText')"
            compact
          >
            <Button
              :label="t('common.syncNow')"
              icon="pi pi-sync"
              size="small"
              :loading="app.syncing"
              @click="app.syncNow()"
            />
          </EmptyState>
          <EChart
            v-else-if="app.repos.length"
            :option="reposOption"
            :height="Math.max(120, topRepos.length * 34) + 'px'"
            :label="t('overview.perProjectLabel')"
          />
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.stats-row {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 20px;
}

.stats-row.five {
  grid-template-columns: repeat(5, minmax(0, 1fr));
}

.opt {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.more-row {
  padding: 6px 0 2px;
}

.more-links {
  display: inline-flex;
  gap: 14px;
  white-space: nowrap;
}

.grid {
  display: grid;
  grid-template-columns: minmax(0, 2fr) minmax(0, 1fr);
  gap: 20px;
  align-items: start;
}

.side {
  display: flex;
  flex-direction: column;
  gap: 20px;
  min-width: 0;
}

.repo-select {
  width: 220px;
}

.more {
  font-size: calc(13px * var(--iw-fs, 1));
  font-weight: 500;
}

.list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding-top: 10px;
}

.row-link {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 8px 10px;
  margin: 0 -10px;
  border-radius: 8px;
  color: var(--iw-text);
  transition: background 120ms ease;
}

.row-link:hover {
  background: var(--iw-hover);
  color: var(--iw-text);
}

.row-link .unread-dot {
  margin-top: 7px;
}

.row-icon {
  margin-top: 3px;
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.row-icon.open {
  color: var(--iw-success);
}

.row-icon.closed {
  color: var(--iw-dimmed);
}

.row-main {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.row-title {
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.meta-mark {
  vertical-align: -2px;
  margin-right: 5px;
}

.row-meta {
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.live-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-success);
}

.live-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--iw-success);
}

@media (width <= 1279px) {
  .stats-row {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  /* Five tiles: 3 + 2 instead of 2 + 2 + 1. */
  .stats-row.five {
    grid-template-columns: repeat(6, minmax(0, 1fr));
  }

  .stats-row.five > :nth-child(-n + 3) {
    grid-column: span 2;
  }

  .stats-row.five > :nth-child(n + 4) {
    grid-column: span 3;
  }

  .grid {
    grid-template-columns: minmax(0, 1fr);
  }
}

@media (width <= 599px) {
  .stats-row,
  .stats-row.five {
    grid-template-columns: minmax(0, 1fr);
  }

  .stats-row.five > :nth-child(n) {
    grid-column: auto;
  }
}
</style>
