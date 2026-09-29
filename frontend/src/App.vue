<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import Toast from 'primevue/toast'
import ConfirmDialog from 'primevue/confirmdialog'
import Drawer from 'primevue/drawer'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import { useToast } from 'primevue/usetoast'
import AppSidebar from './components/AppSidebar.vue'
import AppTopbar from './components/AppTopbar.vue'
import { connectLive, type LiveEventName } from './api/live'
import type { DataChange, LiveItemEvent, SettingsDoc, SyncProgress } from './api/types'
import { useAppStore } from './stores/app'
import { useSettingsStore } from './stores/settings'
import { useUpdatesStore } from './stores/updates'
import { useJobsStore } from './stores/jobs'
import { jobOutcome, jobRef } from './lib/jobs'
import { api } from './api/client'
import type { UpdateStatus } from './api/types'
import { useShortcuts } from './lib/shortcuts'
import { prefetchViews, updateDocumentTitle } from './router'

const app = useAppStore()
const settings = useSettingsStore()
const updates = useUpdatesStore()
const jobs = useJobsStore()
const router = useRouter()
const route = useRoute()
const toast = useToast()
const { t, te } = useI18n()
const mobileNav = ref(false)
const helpOpen = ref(false)

// --- tab reuse: the tray sends navigate{path} instead of opening a new tab
let flashTimer: number | undefined
function flashTitle() {
  window.clearInterval(flashTimer)
  if (document.hasFocus()) return
  const base = document.title
  let on = false
  let n = 0
  flashTimer = window.setInterval(() => {
    on = !on
    document.title = on ? '● ' + base : base // keeps the "IssueWatcher · " marker
    if (document.hasFocus() || ++n > 30) {
      window.clearInterval(flashTimer)
      document.title = base
    }
  }, 1000)
}

function onNavigate(path: string) {
  if (path && path !== route.fullPath) void router.push(path)
  window.focus()
  flashTitle()
}

// --- live events → store + toasts
const KIND: Record<string, { key: string; icon: string; severity: 'info' | 'success' | 'secondary' }> = {
  'item.new': { key: 'app.newIssue', icon: 'pi pi-inbox', severity: 'info' },
  'comment.new': { key: 'app.newComment', icon: 'pi pi-comment', severity: 'info' },
  'item.closed': { key: 'app.issueClosed', icon: 'pi pi-check-circle', severity: 'secondary' },
}
const toastIcon = (msg: unknown) => (msg as { data?: { icon?: string } }).data?.icon ?? 'pi pi-inbox'
const jobLinks = (msg: unknown) => (msg as { data?: { links?: { label: string; to: string; note: string }[] } }).data?.links ?? []

function onLive(name: LiveEventName, data: unknown) {
  switch (name) {
    case 'navigate':
      onNavigate(((data as { path?: string }) ?? {}).path ?? '')
      return
    case 'superseded':
      supersede()
      return
    case 'auth.changed': {
      const a = (data ?? {}) as { state?: string; login?: string }
      if (a.state === 'connected' && !app.githubConnected) {
        toast.add({ severity: 'success', summary: t('app.githubConnected'), detail: a.login ? t('app.signedInAs', { login: '@' + a.login }) : undefined, life: 5000 })
      }
      void app.loadAuth()
      void app.loadSync()
      app.invalidate()
      return
    }
    case 'settings.changed':
      if (data) settings.apply(data as SettingsDoc)
      return
    case 'update.status':
      if (data) updates.apply(data as UpdateStatus)
      return
    case 'sync.status':
      app.onSyncStatus(data as SyncProgress | null)
      return
    case 'data.changed':
      app.invalidate((data as DataChange | null) ?? undefined)
      return
    case 'job.changed': {
      const done = jobs.emitJob(data)
      if (done) {
        // Failed: «Агент: ошибка» + the reason (localized for a known code);
        // finished: «Агент закончил: <outcome>» + the issue.
        const failed = done.state === 'failed'
        const outcome = jobOutcome(done)
        const label = outcome ? t('jobs.outcome.' + outcome) : t('jobs.state.' + done.state)
        const code = done.result.errorCode ?? ''
        const reason = code && te('app.failReason.' + code) ? t('app.failReason.' + code) : (done.error ?? '').split('\n')[0]
        toast.add({
          group: 'live',
          severity: failed || outcome === 'failed' ? 'error' : 'info',
          summary: failed ? t('app.agentFailed') : t('app.agentFinished', { outcome: label }),
          detail: `${jobRef(done)} · ${failed && reason ? reason : done.title}`,
          life: 10000,
          data: { id: done.itemId, job: done.itemId ? undefined : done.id, icon: failed ? 'pi pi-times-circle' : 'pi pi-sparkles' },
        } as never)
      }
      return
    }
    case 'job.log':
      jobs.emitLog(data)
      return
    case 'item.new':
    case 'comment.new':
    case 'item.closed': {
      const e = data as LiveItemEvent
      if (!e || typeof e.id !== 'number') return
      app.pushActivity(name, e)
      const k = KIND[name]
      const detail =
        name === 'comment.new' ? `${e.repo}#${e.number} · ${e.actor ?? ''}: ${e.body ?? ''}` : `${e.repo}#${e.number} · ${e.title}`
      toast.add({ group: 'live', severity: k.severity, summary: t(k.key), detail, life: 8000, data: { id: e.id, icon: k.icon } } as never)
      app.invalidate({ reason: 'sync', itemId: e.id })
    }
  }
}

function openFromToast(msg: unknown, close: () => void) {
  const d = (msg as { data?: { id?: number; job?: number } }).data
  if (d?.job) void router.push(`/jobs/${d.job}`)
  else if (d?.id) void router.push(`/item/${d.id}`)
  close()
}

let stopLive: (() => void) | undefined

function startLive() {
  stopLive?.()
  stopLive = connectLive(onLive, async () => {
    // The app restarted: a new version (self-update) serves new assets, so
    // reload this page — same URL, route and query — instead of patching it.
    const h = await api.health()
    if (h.ok && app.version && h.data.version !== app.version) {
      window.location.reload()
      return
    }
    void updates.load()
    void settings.load()
    void app.loadAuth()
    void app.loadSync()
    void jobs.seed()
    app.bump()
  })
}

// --- one active dashboard tab: a newer tab (tray opened one, or the user did)
// supersedes this one — via BroadcastChannel between tabs and the server's
// "superseded" event. The old tab dims, drops live updates and its title marker
// (so the tray no longer brings it to the front) until the user takes it over.
const superseded = ref(false)
const tabId = Math.random().toString(36).slice(2)
let channel: BroadcastChannel | undefined
function supersede() {
  if (superseded.value) return
  superseded.value = true
  stopLive?.()
  stopLive = undefined
  document.title = t('app.inactiveTitle')
}
function announce() {
  channel?.postMessage({ type: 'active', id: tabId })
}
function takeOver() {
  superseded.value = false
  announce()
  startLive()
  void settings.load()
  void app.loadAuth()
  void app.loadSync()
  void jobs.seed()
  app.bump()
  updateDocumentTitle()
}

useShortcuts({
  sync: () => {
    if (app.anyConnected && !app.syncing) void app.syncNow()
  },
  search: () => {
    if (route.name !== 'issues') void router.push({ name: 'issues', query: { focus: '1' } })
    else window.dispatchEvent(new CustomEvent('iw:focus-search'))
  },
  help: () => (helpOpen.value = !helpOpen.value),
  sidebar: () => app.toggleSidebar(),
  go: (to) => void router.push(to),
})

onMounted(() => {
  void settings.load()
  void app.init()
  void updates.load()
  void jobs.seed()
  startLive()
  prefetchViews()
  if ('BroadcastChannel' in window) {
    channel = new BroadcastChannel('issuewatcher')
    channel.onmessage = (e: MessageEvent<{ type?: string; id?: string }>) => {
      if (e.data?.type === 'active' && e.data.id !== tabId) supersede()
    }
    announce()
  }
})

onBeforeUnmount(() => {
  stopLive?.()
  channel?.close()
})

const shortcuts = computed(() => [
  ['/', t('app.keys.search')],
  ['J / K', t('app.keys.nextPrev')],
  ['Enter', t('app.keys.open')],
  ['X', t('app.keys.toggle')],
  ['R', t('app.keys.sync')],
  ['[', t('app.keys.sidebar')],
  [t('app.keys.goKeys'), t('app.keys.go')],
  ['?', t('app.keys.help')],
])
</script>

<template>
  <div
    class="shell"
    :class="{ collapsed: app.sidebarCollapsed }"
  >
    <aside class="shell-side">
      <AppSidebar />
    </aside>
    <Drawer
      v-model:visible="mobileNav"
      class="mobile-drawer"
      :show-close-icon="false"
      :pt="{ content: { style: 'padding:0' } }"
    >
      <AppSidebar
        mobile
        @navigate="mobileNav = false"
      />
    </Drawer>

    <div class="shell-main">
      <AppTopbar @menu="mobileNav = true" />
      <div
        v-if="app.offlineData"
        class="offline-banner"
        role="status"
      >
        <i class="pi pi-exclamation-circle" />
        {{ t('app.offline') }}
        <RouterLink to="/connections">
          {{ t('app.reconnect') }}
        </RouterLink>
      </div>
      <main class="shell-content">
        <!-- Enter-only fade: the new page mounts and paints at once (no out-in
             wait), the old one leaves in the same frame. -->
        <RouterView v-slot="{ Component }">
          <Transition name="page">
            <component :is="Component" />
          </Transition>
        </RouterView>
      </main>
    </div>

    <Toast
      group="live"
      position="top-right"
    >
      <template #container="{ message, closeCallback }">
        <div
          class="live-toast"
          role="button"
          tabindex="0"
          @click="openFromToast(message, closeCallback)"
          @keydown.enter="openFromToast(message, closeCallback)"
        >
          <span
            class="live-icon"
            :class="message.severity"
          ><i
            :class="toastIcon(message)"
          /></span>
          <div class="live-body">
            <div class="live-title">
              {{ message.summary }}
            </div>
            <div class="live-detail">
              {{ message.detail }}
            </div>
          </div>
          <button
            type="button"
            class="live-close"
            :aria-label="t('app.dismiss')"
            @click.stop="closeCallback"
          >
            <i class="pi pi-times" />
          </button>
        </div>
      </template>
    </Toast>
    <Toast position="top-center" />
    <Toast
      group="jobs"
      position="top-center"
    >
      <template #message="{ message }">
        <div class="jobs-toast">
          <div class="live-title">
            {{ message.summary }}
          </div>
          <div
            v-if="message.detail"
            class="live-detail"
          >
            {{ message.detail }}
          </div>
          <div
            v-if="jobLinks(message).length"
            class="jobs-links"
          >
            <RouterLink
              v-for="l in jobLinks(message)"
              :key="l.to"
              :to="l.to"
              class="mono"
            >
              {{ l.label }}<span
                v-if="l.note"
                class="muted"
              > · {{ l.note }}</span>
            </RouterLink>
          </div>
        </div>
      </template>
    </Toast>
    <ConfirmDialog />

    <Dialog
      v-model:visible="helpOpen"
      :header="t('app.shortcutsTitle')"
      modal
      :style="{ width: '480px' }"
      dismissable-mask
    >
      <table class="keys">
        <tbody>
          <tr
            v-for="[k, what] in shortcuts"
            :key="k"
          >
            <td><kbd class="mono">{{ k }}</kbd></td>
            <td>{{ what }}</td>
          </tr>
        </tbody>
      </table>
      <template #footer>
        <Button
          :label="t('common.close')"
          severity="secondary"
          @click="helpOpen = false"
        />
      </template>
    </Dialog>

    <div
      v-if="superseded"
      class="superseded"
      role="alertdialog"
      aria-modal="true"
      :aria-label="t('app.superseded')"
    >
      <div class="superseded-card panel">
        <i class="pi pi-clone" />
        <h2>{{ t('app.superseded') }}</h2>
        <p class="muted">
          {{ t('app.supersededText') }}
        </p>
        <Button
          :label="t('app.takeOver')"
          icon="pi pi-arrow-down-left"
          @click="takeOver"
        />
      </div>
    </div>
  </div>
</template>

<style scoped>
.shell {
  display: grid;
  grid-template-columns: var(--iw-sidebar) minmax(0, 1fr);
  min-height: 100%;
  transition: grid-template-columns 160ms ease;
}

.shell.collapsed {
  grid-template-columns: var(--iw-sidebar-collapsed) minmax(0, 1fr);
}

.shell-side {
  position: sticky;
  top: 0;
  height: 100vh;
}

.shell-main {
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.shell-content {
  flex: 1;
}

/* A keyframe animation starts on the first frame (a transition would hold the
   enter-from opacity for two frames first), and from 0.4 the page reads at once. */
.page-enter-active {
  animation: page-in 100ms ease-out;
}

@keyframes page-in {
  from {
    opacity: 0.4;
  }
}

/* The old page is gone the moment the new one mounts: never two pages stacked. */
.page-leave-active {
  display: none;
}

@media (prefers-reduced-motion: reduce) {
  .page-enter-active {
    animation: none;
  }
}

.offline-banner {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px var(--iw-gutter);
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-warn);
  background: var(--iw-warn-soft);
  border-bottom: 1px solid var(--iw-border);
}

.offline-banner a {
  margin-left: 4px;
  font-weight: 600;
}

.live-toast {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 14px;
  cursor: pointer;
}

.live-icon {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  flex: none;
  border-radius: 9px;
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.live-icon.secondary {
  color: var(--iw-success);
  background: var(--iw-success-soft);
}

.live-icon.error {
  color: var(--iw-danger);
  background: var(--iw-danger-soft);
}

.live-body {
  min-width: 0;
  flex: 1;
}

.live-title {
  font-weight: 600;
}

.live-detail {
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.jobs-toast {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
  flex: 1;
}

.jobs-links {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin-top: 4px;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.live-close {
  border: 0;
  background: none;
  color: var(--iw-muted);
  cursor: pointer;
  padding: 2px;
}

.superseded {
  position: fixed;
  inset: 0;
  z-index: 1200;
  display: grid;
  place-items: center;
  background: color-mix(in srgb, var(--iw-bg) 72%, transparent);
  backdrop-filter: blur(3px) grayscale(0.6);
}

.superseded-card {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  max-width: 420px;
  padding: 28px 32px;
  text-align: center;
  box-shadow: var(--iw-shadow);
}

.superseded-card i {
  font-size: calc(24px * var(--iw-fs, 1));
  color: var(--iw-primary);
}

.superseded-card h2 {
  font-size: calc(18px * var(--iw-fs, 1));
  font-weight: 600;
}

.superseded-card p {
  margin: 0 0 6px;
}

.keys {
  width: 100%;
  border-collapse: collapse;
}

.keys td {
  padding: 8px 4px;
  border-bottom: 1px solid var(--iw-border);
}

kbd {
  display: inline-block;
  padding: 2px 8px;
  border-radius: 6px;
  border: 1px solid var(--iw-border-strong);
  background: var(--iw-elevated);
  font-size: calc(12px * var(--iw-fs, 1));
}

@media (width <= 899px) {
  .shell,
  .shell.collapsed {
    grid-template-columns: minmax(0, 1fr);
  }

  .shell-side {
    display: none;
  }
}
</style>

<style>
/* Toasts sit top-right below the top bar: clear of the item page's agent
   buttons (bottom of its sidebar) and of the tray's popup cards (bottom-right
   of the screen). Live events go top-right, action results top-center, so a
   dispatch summary and the agent's result never cover each other. */
.p-toast.p-toast-top-right,
.p-toast.p-toast-top-center {
  top: calc(var(--iw-topbar) + 12px) !important;
}

/* Toast surfaces use the app tokens (the container slot drops PrimeVue's colouring). */
.p-toast-message {
  background: var(--iw-surface) !important;
  border: 1px solid var(--iw-border) !important;
  box-shadow: var(--iw-shadow) !important;
  color: var(--iw-text) !important;
  backdrop-filter: none !important;
}

.mobile-drawer.p-drawer {
  width: var(--iw-sidebar) !important;
}
</style>
