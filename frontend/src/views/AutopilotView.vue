<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import Button from 'primevue/button'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import EmptyState from '../components/EmptyState.vue'
import ListPage from '../components/ListPage.vue'
import PlatformIcon from '../components/PlatformIcon.vue'
import { api } from '../api/client'
import type { AutopilotEvent } from '../api/types'
import { absTime, relTime } from '../lib/format'
import { heldText, noteText } from '../lib/release'
import { useAppStore } from '../stores/app'
import { useAutopilotStore } from '../stores/autopilot'

// Autopilot activity log (docs/AUTOPILOT.md → Activity log), newest first.
// Opening the page marks the unread events it loaded read (only those: an
// event that arrived unseen stays unread); live events (SSE autopilot.event)
// go on top and are marked read while the page is visible. Rows that were
// unread when shown keep their marker until the page is left.
const app = useAppStore()
const autopilot = useAutopilotStore()
const toast = useToast()
const { t, te } = useI18n()

const LIMIT = 200
const events = ref<AutopilotEvent[]>([])
const loaded = ref(false)
const error = ref('')
const status = ref(0)
/** Ids that were unread when shown here (the row marker). */
const fresh = ref(new Set<number>())

async function load() {
  const r = await api.autopilotEvents({ limit: LIMIT })
  loaded.value = true
  status.value = r.status
  if (!r.ok) {
    error.value = r.error
    return
  }
  error.value = ''
  events.value = r.data?.events ?? []
  autopilot.setCounts(r.data)
  const unread = events.value.filter((e) => !e.readAt).map((e) => e.id)
  if (unread.length) {
    fresh.value = new Set([...fresh.value, ...unread])
    queueRead(unread)
  }
}

// --- marking read: batched, and only while the page is visible
const pending = new Set<number>()
let readTimer: number | undefined
function queueRead(ids: number[]) {
  for (const id of ids) pending.add(id)
  window.clearTimeout(readTimer)
  readTimer = window.setTimeout(() => void flushRead(), 300)
}
async function flushRead() {
  if (!pending.size || document.visibilityState !== 'visible') return
  const ids = [...pending]
  pending.clear()
  if (await autopilot.markRead(ids)) setRead(ids)
  else for (const id of ids) pending.add(id)
}
function setRead(ids: number[]) {
  const set = new Set(ids)
  const now = new Date().toISOString()
  events.value = events.value.map((e) => (set.has(e.id) && !e.readAt ? { ...e, readAt: now } : e))
}
const onVisible = () => void flushRead()

// «Отметить прочитанным»: every unread event, also those beyond the loaded page.
const marking = ref(false)
async function markAll() {
  marking.value = true
  const ids = new Set(events.value.filter((e) => !e.readAt).map((e) => e.id))
  for (const id of pending) ids.add(id)
  pending.clear()
  if (autopilot.unread > ids.size) {
    const r = await api.autopilotEvents({ unreadOnly: true, limit: LIMIT })
    if (r.ok) for (const e of r.data?.events ?? []) ids.add(e.id)
  }
  const ok = await autopilot.markRead([...ids])
  marking.value = false
  if (!ok) {
    toast.add({ severity: 'error', summary: t('autopilot.markReadFailed'), life: 5000 })
    return
  }
  setRead([...ids])
}
const canMarkAll = computed(() => autopilot.unread > 0 || events.value.some((e) => !e.readAt))

onMounted(() => {
  void load()
  document.addEventListener('visibilitychange', onVisible)
})
onBeforeUnmount(() => {
  window.clearTimeout(readTimer)
  void flushRead()
  document.removeEventListener('visibilitychange', onVisible)
})
// SSE reconnect / take-over: reload what was missed.
watch(
  () => app.dataVersion,
  () => {
    if (app.lastChanges === null) void load()
  },
)
// Live: a new event goes on top.
watch(
  () => autopilot.lastEvent,
  (e) => {
    if (!e || !loaded.value || events.value.some((x) => x.id === e.id)) return
    events.value = [e, ...events.value].slice(0, LIMIT)
    if (!e.readAt) {
      fresh.value = new Set([...fresh.value, e.id])
      queueRead([e.id])
    }
  },
  { flush: 'sync' },
)

// --- row text
const KIND_ICON: Record<string, string> = {
  'release.held': 'pi pi-pause-circle',
  'release.done': 'pi pi-check-circle',
  'release.cancelled': 'pi pi-ban',
}
function icon(e: AutopilotEvent): string {
  return KIND_ICON[e.kind] ?? (e.severity === 'attention' ? 'pi pi-exclamation-triangle' : 'pi pi-info-circle')
}
function tone(e: AutopilotEvent): string {
  if (e.severity === 'attention') return 'attention'
  if (e.kind === 'release.done') return 'done'
  return ''
}
/** Kind label (release.held → «Релиз приостановлен»); unknown kinds: none (title only). */
function kindText(kind: string): string {
  const k = 'autopilot.kind.' + kind.replace(/\./g, '_')
  return te(k) ? t(k) : ''
}
const str = (v: unknown) => (typeof v === 'string' ? v : typeof v === 'number' ? String(v) : '')
/** detail.reason (a held reason code) as text. */
function reasonText(e: AutopilotEvent): string {
  const r = str(e.detail?.reason)
  return r ? heldText(r) : ''
}
function detailText(e: AutopilotEvent): string {
  return noteText(str(e.detail?.detail))
}
function versionText(e: AutopilotEvent): string {
  const v = str(e.detail?.version)
  return v ? t('autopilot.version', { v }) : ''
}
function project(e: AutopilotEvent) {
  if (!e.projectId) return null
  const r = app.repos.find((x) => x.id === e.projectId)
  return { id: e.projectId, name: r?.name ?? '#' + e.projectId, platform: r?.platform ?? '' }
}
</script>

<template>
  <div class="page">
    <ListPage
      :total="loaded && !error ? events.length : null"
      :total-label="t('autopilot.words', events.length)"
      :skeleton="!loaded"
    >
      <template #filters>
        <span
          v-if="autopilot.unread"
          class="counts"
        >
          {{ t('autopilot.unreadCount', { n: autopilot.unread }) }}<template v-if="autopilot.attention">
            · <span class="attn">{{ t('autopilot.attentionCount', { n: autopilot.attention }) }}</span>
          </template>
        </span>
      </template>
      <template #actions>
        <Button
          :label="t('autopilot.markRead')"
          icon="pi pi-check"
          severity="secondary"
          :loading="marking"
          :disabled="!canMarkAll"
          @click="markAll"
        />
      </template>

      <div class="panel log">
        <template v-if="!events.length">
          <EmptyState
            v-if="error && status === 404"
            icon="pi pi-server"
            :title="t('autopilot.unavailable')"
            :text="t('autopilot.unavailableText')"
          />
          <EmptyState
            v-else-if="error"
            icon="pi pi-exclamation-triangle"
            :title="t('autopilot.loadError')"
            :text="error"
          >
            <Button
              :label="t('common.retry')"
              icon="pi pi-refresh"
              size="small"
              @click="load()"
            />
          </EmptyState>
          <EmptyState
            v-else
            icon="pi pi-bell"
            :title="t('autopilot.empty')"
            :text="t('autopilot.emptyText')"
          />
        </template>
        <ul
          v-else
          class="events"
        >
          <li
            v-for="e in events"
            :key="e.id"
            class="ev"
            :class="[tone(e), { unread: fresh.has(e.id) || !e.readAt }]"
          >
            <span
              class="ev-icon"
              :class="tone(e)"
            ><i :class="icon(e)" /></span>
            <div class="ev-body">
              <div class="ev-head">
                <span
                  v-if="fresh.has(e.id) || !e.readAt"
                  v-tooltip.top="t('autopilot.unread')"
                  class="dot"
                  :aria-label="t('autopilot.unread')"
                />
                <span
                  v-if="kindText(e.kind)"
                  class="ev-kind"
                >{{ kindText(e.kind) }}</span>
                <span class="ev-title">{{ e.title }}</span>
                <span
                  v-if="e.severity === 'attention'"
                  class="ev-attn"
                >{{ t('autopilot.attention') }}</span>
                <span
                  v-tooltip.top="absTime(e.at)"
                  class="ev-time muted"
                >{{ relTime(e.at) }}</span>
              </div>
              <div
                v-if="reasonText(e)"
                class="ev-reason"
              >
                <i class="pi pi-pause-circle" /> {{ reasonText(e) }}
              </div>
              <div
                v-if="detailText(e)"
                class="ev-detail"
              >
                {{ detailText(e) }}
              </div>
              <div
                v-if="versionText(e) || e.runId || e.projectId || e.itemId"
                class="ev-links"
              >
                <span
                  v-if="versionText(e)"
                  class="mono muted"
                >{{ versionText(e) }}</span>
                <RouterLink
                  v-if="e.runId"
                  :to="`/runs/${e.runId}`"
                >
                  <i class="pi pi-send" /> {{ t('autopilot.run', { id: e.runId }) }}
                </RouterLink>
                <RouterLink
                  v-if="project(e)"
                  :to="{ path: '/issues', query: { repo: String(e.projectId) } }"
                  class="proj"
                >
                  <PlatformIcon
                    v-if="project(e)?.platform"
                    :platform="project(e)?.platform ?? ''"
                    :size="13"
                  />{{ project(e)?.name }}
                </RouterLink>
                <RouterLink
                  v-if="e.itemId"
                  :to="`/item/${e.itemId}`"
                >
                  <i class="pi pi-inbox" /> {{ t('autopilot.item') }}
                </RouterLink>
              </div>
            </div>
          </li>
        </ul>
      </div>
    </ListPage>
  </div>
</template>

<style scoped>
.counts {
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.attn {
  color: var(--iw-danger);
  font-weight: 600;
}

.log {
  overflow: hidden;
}

.events {
  margin: 0;
  padding: 0;
  list-style: none;
}

.ev {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--iw-border);
  border-left: 3px solid transparent;
}

.ev:last-child {
  border-bottom: 0;
}

.ev.attention {
  background: var(--iw-danger-soft);
  border-left-color: var(--iw-danger);
}

.ev-icon {
  display: grid;
  place-items: center;
  width: 30px;
  height: 30px;
  flex: none;
  border-radius: 8px;
  color: var(--iw-muted);
  background: var(--iw-elevated);
}

.ev-icon.attention {
  color: var(--iw-danger);
  background: var(--iw-surface);
}

.ev-icon.done {
  color: var(--iw-success);
  background: var(--iw-success-soft);
}

.ev-body {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
  flex: 1;
}

.ev-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px 8px;
}

.dot {
  width: 8px;
  height: 8px;
  flex: none;
  border-radius: 50%;
  background: var(--iw-primary);
}

.ev-kind {
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.ev-title {
  font-weight: 500;
  overflow-wrap: anywhere;
}

.ev.unread .ev-title {
  font-weight: 600;
}

.ev-attn {
  padding: 0 6px;
  border-radius: 999px;
  font-size: calc(11px * var(--iw-fs, 1));
  font-weight: 600;
  color: #fff;
  background: var(--iw-danger);
}

.ev-time {
  margin-left: auto;
  font-size: calc(12px * var(--iw-fs, 1));
  white-space: nowrap;
}

.ev-reason {
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-warn);
}

.ev-detail {
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.ev-links {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px 14px;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.ev-links a {
  display: inline-flex;
  align-items: center;
  gap: 5px;
}
</style>
