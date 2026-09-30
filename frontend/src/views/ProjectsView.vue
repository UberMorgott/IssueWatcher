<script setup lang="ts">
import { safeUrl } from '../lib/safeUrl'
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import DataTable from 'primevue/datatable'
import Column from 'primevue/column'
import Button from 'primevue/button'
import Skeleton from 'primevue/skeleton'
import type { EChartsCoreOption } from 'echarts/core'
import EChart, { type ChartTheme } from '../components/EChart.vue'
import EmptyState from '../components/EmptyState.vue'
import ListPage from '../components/ListPage.vue'
import ConnectHero from '../components/ConnectHero.vue'
import PlatformIcon from '../components/PlatformIcon.vue'
import FolderDialog from '../components/FolderDialog.vue'
import LinkDialog from '../components/LinkDialog.vue'
import { isModPlatform, platformName } from '../lib/platforms'
import { api } from '../api/client'
import type { Integration, Repo, Stats } from '../api/types'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { absTime, relTime, repoColor, repoOwner, shortDay, shortRepo } from '../lib/format'
import { useChunks } from '../lib/chunks'
import type { DataTableSortEvent } from 'primevue/datatable'
import { useTriage } from '../lib/jobs'
import { useSettingsStore } from '../stores/settings'
import { useToast } from 'primevue/usetoast'

const app = useAppStore()
const toast = useToast()
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
// One row per project: the server folds linked mod pages into Repo.integrations.
const list = useChunks<Repo>((cursor) => api.reposChunk(sortField.value, sortOrder.value < 0, filter.value.trim(), cursor, 50, true), {
  cacheKey: () => `projects:${sortField.value}:${sortOrder.value}:${filter.value.trim()}`,
})
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
/** Projects = grouped rows (linked mod pages are part of their project), unfiltered. */
const projectCount = computed(() => (!filter.value.trim() && list.total.value != null ? list.total.value : app.repos.filter((r) => !r.linkedTo).length))
/** Header totals: open issues and bug reports, open comment threads apart (Открыто = the same count as the column and chips). */
const totals = computed(() =>
  app.repos.reduce(
    (a, r) => ({ open: a.open + openIssues(r), comments: a.comments + (r.openComments ?? 0), closed: a.closed + r.closed }),
    { open: 0, comments: 0, closed: 0 },
  ),
)

async function onExpand(e: { data: Repo }) {
  const id = e.data.id
  if (repoStats[id] && repoStats[id] !== 'error') return
  repoStats[id] = 'loading'
  const r = await api.stats(id)
  repoStats[id] = r.ok ? r.data : 'error'
}

// Live data: re-read the loaded range in place (same sort, no scroll jump) and take its cursor too,
// so the next chunk continues after the re-read rows; open charts refetch quietly.
watch(
  () => app.dataVersion,
  async () => {
    const n = rows.value.length
    if (n) {
      const key = [sortField.value, sortOrder.value, filter.value.trim()].join('|')
      const r = await api.reposChunk(sortField.value, sortOrder.value < 0, filter.value.trim(), '', Math.min(Math.max(n, 50), 1000), true)
      if (r.ok && key === [sortField.value, sortOrder.value, filter.value.trim()].join('|')) list.replace(r.data)
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
/** A row's channels: its own project, then its linked mod pages, each with its own counts. */
const channels = (r: Repo): Integration[] =>
  r.integrations?.length
    ? r.integrations
    : [{ id: r.id, name: r.name, url: r.url, platform: r.platform || 'github', open: r.open, closed: r.closed, unread: r.unread, openComments: r.openComments, unreadComments: r.unreadComments, lastSync: r.lastSync ?? '' }]
/** Open / unread issues and bug reports of a channel or row: everything but comments. */
const openIssues = (c: { open: number; openComments?: number }) => c.open - (c.openComments ?? 0)
const unreadIssues = (c: { unread: number; unreadComments?: number }) => c.unread - (c.unreadComments ?? 0)
/** Issues (or comments) of the whole project on one platform (the project filter spans its linked mod pages). */
const issuesTo = (r: Repo, platform: string, unread = false, page: 'issues' | 'comments' = 'issues') => ({
  name: page,
  query: unread ? { repo: String(r.id), source: platform, unread: '1', state: 'all' } : { repo: String(r.id), source: platform },
})
/** A channel's name for tooltips: the platform, plus the mod page's name (the chip itself shows only icon + count). */
const channelLabel = (c: Integration) => (isModPlatform(c.platform) ? `${platformName(c.platform)} «${c.name}»` : platformName(c.platform))
/** A channel chip's tooltip: what + any sync trouble (relogin note). */
function channelTip(c: Integration, what = ''): string {
  const trouble = channelTrouble(c.platform)
  return [what || channelLabel(c), trouble].filter(Boolean).join(' · ')
}
/** Sync trouble of a platform's source: relogin first, else its last error ("" = fine). */
function channelTrouble(platform: string): string {
  const srcs = (app.sync?.sources ?? []).filter((s) => s.platform === platform)
  const s = srcs.find((x) => x.relogin) ?? srcs.find((x) => x.lastError)
  if (!s) return ''
  const name = platformName(platform)
  return s.relogin ? t('projects.channelRelogin', { platform: name }) : t('projects.channelError', { platform: name, error: s.lastError })
}

// Row sync: the project and every linked mod page, each through its source.
// The row spins until each started channel reported sync.status done/error
// (Progress.repo = that project's name); a lost event gives up after 2 min.
const rowSyncing = reactive<Record<number, boolean>>({})
const pendingSync = new Map<number, { names: Set<string>; timer: number }>()
function endRowSync(id: number) {
  window.clearTimeout(pendingSync.get(id)?.timer)
  pendingSync.delete(id)
  rowSyncing[id] = false
}
watch(
  () => app.lastProgress,
  (p) => {
    if (!p?.repo || (p.state !== 'done' && p.state !== 'error')) return
    for (const [id, s] of pendingSync) if (s.names.delete(p.repo) && !s.names.size) endRowSync(id)
  },
  { flush: 'sync' },
)
onBeforeUnmount(() => pendingSync.forEach((s) => window.clearTimeout(s.timer)))
async function syncProject(r: Repo) {
  endRowSync(r.id)
  rowSyncing[r.id] = true
  const res = await api.syncProject(r.id)
  if (!res.ok) {
    endRowSync(r.id)
    toast.add({ severity: 'error', summary: t('topbar.syncStartFailed'), detail: r.name, life: 4000 })
    return
  }
  const started = res.data?.started ?? []
  const missing = res.data?.missing ?? []
  const names = new Set(channels(r).filter((c) => started.includes(c.platform)).map((c) => c.name))
  if (names.size) pendingSync.set(r.id, { names, timer: window.setTimeout(() => endRowSync(r.id), 120_000) })
  else endRowSync(r.id)
  const platforms = (l: string[]) => [...new Set(l)].map(platformName).join(', ')
  if (started.length) toast.add({ severity: 'info', summary: t('projects.syncStarted', { list: platforms(started) }), life: 2500 })
  if (missing.length) toast.add({ severity: 'warn', summary: t('projects.syncMissing', { list: platforms(missing) }), life: 4000 })
}

/** Patches a loaded row's folder (the dialog or the inline unmap; the API saves only a clone of the project). */
function setRowFolder(id: number, localPath: string) {
  rows.value = rows.value.map((x) => (x.id === id ? { ...x, localPath, ...(x.fixProjectId === id ? { fixFolder: localPath, fixable: !!localPath } : {}) } : x))
}
async function unmapFolder(r: Repo) {
  const res = await api.setFolder(r.id, '')
  if (!res.ok) {
    toast.add({ severity: 'error', summary: t('settings.folders.saveFailed'), detail: r.name, life: 4000 })
    return
  }
  setRowFolder(r.id, '')
  toast.add({ severity: 'success', summary: t('folder.unmapped'), detail: r.name, life: 3000 })
  void app.loadRepos()
}
const codeOf = (r: Repo) => {
  const id = repoById.value.get(r.id)?.linkedTo ?? r.linkedTo
  return id ? repoById.value.get(id) : undefined
}
const hasMods = computed(() => app.repos.some((r) => isModPlatform(r.platform)))
/** Name-match suggestions of an unlinked mod page (store.AutoLink links the certain ones itself). */
const suggestionsOf = (r: Repo) =>
  codeOf(r) ? [] : (repoById.value.get(r.id)?.suggest ?? r.suggest ?? []).map((id) => repoById.value.get(id)).filter((x): x is Repo => !!x)
async function acceptSuggestion(mod: Repo, code: Repo) {
  const r = await api.setLinks(code.id, [...new Set([...(code.links ?? []), mod.id])])
  if (!r.ok) {
    toast.add({ severity: 'error', summary: t('issues.bulkFailed'), detail: mod.name, life: 4000 })
    return
  }
  toast.add({ severity: 'success', summary: t('platforms.linkedTo', { name: code.name }), life: 2500 })
  await app.loadRepos()
  reload() // the mod page folds into its project's row
}

const closedShare = (r: Repo) => (r.open + r.closed ? Math.round((r.closed / (r.open + r.closed)) * 100) : 0)
</script>

<template>
  <div class="page">
    <div
      v-if="app.repos.length"
      class="page-head"
    >
      <div class="summary muted">
        <b class="mono">{{ projectCount }}</b> {{ t('words.projects', projectCount) }} · <b class="mono">{{ totals.open }}</b> {{ t('words.open', totals.open) }}<template v-if="totals.comments">
          · <b class="mono">{{ totals.comments }}</b> {{ t('projects.openThreads', totals.comments) }}
        </template> · <b class="mono">{{ totals.closed }}</b> {{ t('words.closed', totals.closed) }}
      </div>
    </div>

    <ConnectHero v-if="app.onboarding" />

    <template v-else>
      <ListPage
        v-model:search="filter"
        :search-placeholder="t('projects.filter')"
        :resettable="!!filter"
        :total="filter.trim() ? list.total.value : null"
        :total-label="list.total.value === null ? '' : t('words.projects', list.total.value)"
        :skeleton="!rows.length && listLoading"
        @reset="filter = ''"
      >
        <template #actions>
          <Button
            v-tooltip.bottom="t('projects.discoverTip')"
            as="router-link"
            to="/settings/projects"
            :label="t('projects.discover')"
            icon="pi pi-folder-open"
            severity="secondary"
            outlined
          />
        </template>
        <FolderDialog
          v-model:visible="folderOpen"
          :project="folderProject"
          @saved="(p: string) => folderProject && setRowFolder(folderProject.id, p)"
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
                  :disabled="!app.anyConnected"
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
                    <!-- Channels: the project itself + its linked mod pages, each with its counters -->
                    <span class="chips">
                      <span
                        v-for="c in channels(data)"
                        :key="c.id"
                        class="chip channel"
                        :class="{ trouble: !!channelTrouble(c.platform) }"
                      >
                        <a
                          v-if="c.id !== data.id"
                          v-tooltip.top="channelTip(c, t('projects.openModPage') + ' · ' + channelLabel(c))"
                          :href="safeUrl(c.url)"
                          target="_blank"
                          rel="noopener noreferrer"
                          class="chip-link"
                          :aria-label="channelTip(c)"
                        ><PlatformIcon
                          :platform="c.platform"
                          :size="12"
                        /></a>
                        <PlatformIcon
                          v-else
                          :platform="c.platform"
                          :size="12"
                        />
                        <RouterLink
                          v-tooltip.top="channelTip(c, t('projects.channelIssues', { platform: channelLabel(c) }) + ' · ' + openIssues(c) + ' ' + t('words.open', openIssues(c)))"
                          :to="issuesTo(data, c.platform)"
                          class="count mono"
                        >{{ openIssues(c) }}</RouterLink>
                        <RouterLink
                          v-if="unreadIssues(c)"
                          v-tooltip.top="channelTip(c, t('projects.channelUnread', { platform: channelLabel(c) }) + ' · ' + unreadIssues(c) + ' ' + t('words.unread', unreadIssues(c)))"
                          :to="issuesTo(data, c.platform, true)"
                          class="count unread mono"
                        >{{ unreadIssues(c) }}</RouterLink>
                        <!-- A mod page's comment thread: its own counters, deep-linked to the Comments page -->
                        <template v-if="c.openComments || c.unreadComments">
                          <RouterLink
                            v-tooltip.top="channelTip(c, t('projects.channelComments', { platform: channelLabel(c) }) + ' · ' + (c.openComments ?? 0) + ' ' + t('words.open', c.openComments ?? 0))"
                            :to="issuesTo(data, c.platform, false, 'comments')"
                            class="count comments mono"
                          ><i class="pi pi-comments" />{{ c.openComments ?? 0 }}</RouterLink>
                          <RouterLink
                            v-if="c.unreadComments"
                            v-tooltip.top="channelTip(c, t('projects.channelUnreadComments', { platform: channelLabel(c) }) + ' · ' + c.unreadComments + ' ' + t('words.unread', c.unreadComments))"
                            :to="issuesTo(data, c.platform, true, 'comments')"
                            class="count unread mono"
                          >{{ c.unreadComments }}</RouterLink>
                        </template>
                        <i
                          v-if="channelTrouble(c.platform)"
                          v-tooltip.top="channelTrouble(c.platform)"
                          class="pi pi-exclamation-triangle warn"
                        />
                      </span>
                      <button
                        v-if="!isModPlatform(data.platform) && (channels(data).length > 1 || hasMods)"
                        type="button"
                        class="chip add"
                        :aria-label="t('platforms.linkMods')"
                        @click.stop="openLinks(data)"
                      ><i class="pi pi-link" />{{ channels(data).length > 1 ? '' : t('platforms.mods') }}</button>
                    </span>
                    <!-- A mod page without a code project: «не привязан», link / one-click suggestion -->
                    <span
                      v-if="isModPlatform(data.platform)"
                      class="chips"
                    >
                      <span
                        v-if="!codeOf(data)"
                        class="chip tag"
                      >{{ t('platforms.notLinked') }}</span>
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
                      <button
                        v-for="s in suggestionsOf(data)"
                        :key="'s' + s.id"
                        v-tooltip.top="t('platforms.suggestHint')"
                        type="button"
                        class="chip suggest"
                        @click.stop="acceptSuggestion(data, s)"
                      ><PlatformIcon
                        platform="github"
                        :size="12"
                      /><span class="chip-name">{{ t('platforms.suggestLink', { name: s.name }) }}</span></button>
                    </span>
                  </div>
                </div>
              </template>
            </Column>
            <Column class="actions-col">
              <template #body="{ data }: { data: Repo }">
                <Button
                  v-tooltip.top="t('projects.syncTip')"
                  icon="pi pi-sync"
                  size="small"
                  severity="secondary"
                  text
                  rounded
                  :aria-label="t('projects.sync')"
                  :loading="!!rowSyncing[data.id]"
                  @click.stop="syncProject(data)"
                />
                <span
                  v-if="!isModPlatform(data.platform)"
                  v-tooltip.top="!data.fixable ? t('folder.needed') : !openIssues(data) ? t('jobs.triage.nothingOpen') : t('jobs.triage.runTip', { n: triageTopN(data.key) })"
                >
                  <Button
                    :label="t('jobs.triage.run')"
                    :aria-label="t('jobs.triage.run')"
                    icon="pi pi-sort-amount-down"
                    size="small"
                    severity="secondary"
                    text
                    class="nowrap"
                    :loading="triage.busy.value.has(data.id)"
                    :disabled="!openIssues(data) || !data.fixable"
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
                <span
                  v-tooltip.top="data.openComments ? t('projects.openSplit', { issues: openIssues(data), comments: data.openComments }) : undefined"
                  class="mono strong"
                >{{ openIssues(data) }}</span>
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
                  :to="{ name: unreadIssues(data) ? 'issues' : 'comments', query: { repo: String(data.id), unread: '1', state: 'all' } }"
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
            <Column
              :header="t('projects.colFolder')"
              class="folder-col"
            >
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
                    v-else-if="codeOf(data)"
                    class="not-mapped"
                  ><i class="pi pi-folder" /> {{ t('projects.notMapped') }}</span>
                  <!-- Not linked to a repository: the name cell already says so; the folder comes with the link -->
                  <span
                    v-else
                    v-tooltip.top="t('platforms.fixNeedsLink')"
                    class="not-mapped"
                  ><i class="pi pi-folder" /> —</span>
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
                    :aria-label="data.localPath ? t('projects.change') : t('projects.choose')"
                    :icon="data.localPath ? 'pi pi-pencil' : 'pi pi-folder-plus'"
                    class="folder-btn"
                    size="small"
                    severity="secondary"
                    text
                    @click.stop="mapFolder(data)"
                  />
                  <Button
                    v-if="data.localPath"
                    v-tooltip.top="t('folder.unmap')"
                    icon="pi pi-times"
                    size="small"
                    severity="secondary"
                    text
                    rounded
                    :aria-label="t('folder.unmap')"
                    @click.stop="unmapFolder(data)"
                  />
                </div>
              </template>
            </Column>
            <Column
              field="lastSync"
              :header="t('projects.colLastSync')"
              sortable
              class="sync-col"
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
                      {{ openIssues(data) }}
                    </div>
                  </div>
                  <div v-if="data.openComments">
                    <div class="exp-label">
                      {{ t('projects.colThreads') }}
                    </div><div class="exp-val mono">
                      {{ data.openComments }}
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
                    :label="isModPlatform(data.platform) ? t('projects.openModPage') : t('projects.openRepo')"
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
            v-if="listLoading && rows.length"
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
      </ListPage>
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

.table-panel {
  overflow: hidden;
  container: projects / inline-size;
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

.chip.suggest {
  color: var(--iw-primary);
  border-style: dashed;
  border-color: var(--iw-primary);
}

.chip.channel {
  max-width: 300px;
  cursor: default;
}

.chip.channel.trouble {
  border-color: var(--iw-warn);
}

.chip-link {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  min-width: 0;
  color: inherit;
}

.chip-link:hover {
  color: var(--iw-primary);
}

.count {
  min-width: 18px;
  padding: 0 5px;
  border-radius: 999px;
  text-align: center;
  font-weight: 600;
  color: var(--iw-text);
  background: var(--iw-surface);
}

.count:hover {
  color: var(--iw-primary);
}

.count.comments {
  display: inline-flex;
  align-items: center;
  gap: 3px;
}

.count.comments i {
  font-size: calc(10px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.count.unread {
  color: var(--iw-on-primary);
  background: var(--iw-primary);
}

.warn {
  font-size: calc(11px * var(--iw-fs, 1));
  color: var(--iw-warn);
}

.chip.tag {
  color: var(--iw-dimmed);
  cursor: default;
}

:deep(.actions-col) {
  white-space: nowrap;
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
  max-width: 180px;
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
  width: 72px;
}

/* The table fits its panel at every width instead of being clipped by it: the
   panel is a size container and, as it narrows, the row actions and folder
   buttons drop their labels (tooltip and aria-label keep them), then the
   progress bar and the last-sync column go (.table-panel is the container). */
@container projects (width < 1300px) {
  :deep(.actions-col .p-button-label),
  :deep(.folder-btn .p-button-label) {
    display: none;
  }

  :deep(.progress-col) {
    display: none;
  }

  .name-main {
    min-width: 180px;
  }

  :deep(.iw-projects .p-datatable-thead > tr > th),
  :deep(.iw-projects .p-datatable-tbody > tr > td) {
    padding-inline: 8px;
  }

  .mapped {
    max-width: 140px;
  }
}

@container projects (width < 1000px) {
  :deep(.sync-col) {
    display: none;
  }
}

@media (width <= 1023px) {
  .expansion {
    grid-template-columns: minmax(0, 1fr);
    padding-left: 8px;
  }
}
</style>
