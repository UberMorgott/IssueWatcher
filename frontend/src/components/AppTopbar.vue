<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Button from 'primevue/button'
import Menu from 'primevue/menu'
import Popover from 'primevue/popover'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { liveConnected } from '../api/live'
import { routeTitle } from '../router'
import { crumbs } from '../lib/crumbs'
import { absTime, duration, relTime } from '../lib/format'

defineEmits<{ menu: [] }>()
const app = useAppStore()
const route = useRoute()
const router = useRouter()
const toast = useToast()
const { t } = useI18n()

const title = computed(() => routeTitle(route))

type Tone = 'ok' | 'busy' | 'warn' | 'error' | 'off'
/** Every source's status (the top-level fields alone are the GitHub one). */
const sources = computed(() => app.sync?.sources ?? (app.sync ? [{ ...app.sync, platform: 'github', account: '' }] : []))
const rateLimitedUntil = computed(() => sources.value.map((s) => s.rateLimitedUntil).find(Boolean) ?? '')
const lastError = computed(() => sources.value.filter((s) => s.signedIn).map((s) => s.lastError).find(Boolean) ?? '')
const lastSync = computed(() => sources.value.map((s) => s.lastSync).filter(Boolean).sort().at(-1) ?? '')
const status = computed<{ tone: Tone; text: string }>(() => {
  if (!app.anyConnected) return { tone: 'off', text: t('topbar.notConnected') }
  const p = app.progress
  if (p && p.total > 0) return { tone: 'busy', text: t('topbar.syncProgress', { done: p.done, total: p.total }) }
  if (app.syncing) return { tone: 'busy', text: t('topbar.syncing') }
  if (!app.sync) return { tone: 'off', text: t('topbar.syncUnavailable') }
  if (rateLimitedUntil.value) return { tone: 'warn', text: t('topbar.rateLimited') }
  if (lastError.value) return { tone: 'error', text: t('topbar.syncError') }
  if (lastSync.value) return { tone: 'ok', text: t('topbar.synced', { time: relTime(lastSync.value) }) }
  return { tone: 'off', text: t('topbar.waitingFirstSync') }
})
/** Background work (scheduled reconcile, change checks): a quiet spinner with a tooltip, never a blocked button. */
const backgroundText = computed(() => {
  const b = app.background
  if (!b || app.syncing) return ''
  return b.total > 0 ? t('topbar.background', { done: b.done, total: b.total }) : t('topbar.backgroundShort')
})

const pop = ref<InstanceType<typeof Popover>>()
const menu = ref<InstanceType<typeof Menu>>()

async function syncNow() {
  const err = await app.syncNow()
  if (err) toast.add({ severity: 'error', summary: t('topbar.syncStartFailed'), detail: err, life: 5000 })
}

const accountItems = computed(() => [
  { label: app.github?.login ? '@' + app.github.login : t('topbar.notSignedIn'), disabled: true },
  { separator: true },
  { label: t('nav.connections'), icon: 'pi pi-link', command: () => router.push('/connections') },
  { label: t('nav.settings'), icon: 'pi pi-cog', command: () => router.push('/settings') },
  ...(app.githubConnected ? [{ label: t('topbar.signOut'), icon: 'pi pi-sign-out', command: () => app.disconnect('github') }] : []),
])
</script>

<template>
  <header class="topbar">
    <button
      type="button"
      class="icon-btn only-mobile"
      :aria-label="t('topbar.openMenu')"
      @click="$emit('menu')"
    >
      <i class="pi pi-bars" />
    </button>
    <nav
      v-if="crumbs.length"
      class="topbar-title crumbs"
      :aria-label="t('item.breadcrumb')"
    >
      <template
        v-for="(c, i) in crumbs"
        :key="i"
      >
        <i
          v-if="i > 0"
          class="pi pi-angle-right sep"
        />
        <RouterLink
          v-if="c.to && i < crumbs.length - 1"
          :to="c.to"
          class="crumb-link"
          :class="{ mono: c.mono }"
        >
          {{ c.label }}
        </RouterLink>
        <h1
          v-else-if="i === crumbs.length - 1"
          class="crumb-current"
          :class="{ mono: c.mono }"
        >
          {{ c.label }}
        </h1>
        <span
          v-else
          :class="{ mono: c.mono }"
        >{{ c.label }}</span>
      </template>
    </nav>
    <h1
      v-else
      class="topbar-title"
    >
      {{ title }}
    </h1>

    <div class="topbar-right">
      <button
        type="button"
        class="sync-chip"
        :class="status.tone"
        aria-haspopup="dialog"
        @click="pop?.toggle($event)"
      >
        <span class="dot" />
        <span class="sync-text">{{ status.text }}</span>
        <i
          v-if="backgroundText"
          v-tooltip.bottom="backgroundText"
          class="pi pi-sync bg-spin"
          :aria-label="backgroundText"
        />
      </button>
      <Popover ref="pop">
        <div class="sync-pop">
          <div class="row">
            <span class="muted">GitHub</span><span>{{ app.githubConnected ? '@' + app.github?.login : t('topbar.notConnectedLower') }}</span>
          </div>
          <div
            v-if="app.progress?.repo"
            class="row"
          >
            <span class="muted">{{ t('topbar.current') }}</span><span class="mono">{{ app.progress.repo }}</span>
          </div>
          <div
            v-if="backgroundText"
            class="row"
          >
            <span class="muted">{{ backgroundText }}</span><span class="mono">{{ app.background?.repo }}</span>
          </div>
          <div class="row">
            <span class="muted">{{ t('topbar.lastSync') }}</span><span>{{ absTime(lastSync) || t('common.never') }}</span>
          </div>
          <div class="row">
            <span class="muted">{{ t('topbar.interval') }}</span><span class="mono">{{ duration(app.sync?.interval) || '—' }}</span>
          </div>
          <div class="row">
            <span class="muted">{{ t('topbar.liveUpdates') }}</span><span>{{ liveConnected ? t('topbar.liveConnected') : t('topbar.liveReconnecting') }}</span>
          </div>
          <div
            v-if="rateLimitedUntil"
            class="row warn"
          >
            <span>{{ t('topbar.rateLimitedUntil') }}</span><span>{{ absTime(rateLimitedUntil) }}</span>
          </div>
          <p
            v-if="lastError"
            class="err"
          >
            {{ lastError }}
          </p>
        </div>
      </Popover>

      <Button
        v-tooltip.bottom="t('topbar.syncNowTip')"
        icon="pi pi-sync"
        :class="{ spinning: app.syncing }"
        severity="secondary"
        text
        rounded
        :aria-label="t('common.syncNow')"
        :disabled="!app.anyConnected || app.syncing"
        @click="syncNow"
      />
      <Button
        v-tooltip.bottom="app.theme === 'dark' ? t('topbar.lightTheme') : t('topbar.darkTheme')"
        :icon="app.theme === 'dark' ? 'pi pi-sun' : 'pi pi-moon'"
        severity="secondary"
        text
        rounded
        :aria-label="t('topbar.toggleTheme')"
        @click="app.setTheme(app.theme === 'dark' ? 'light' : 'dark')"
      />
      <button
        type="button"
        class="avatar-btn"
        :aria-label="t('topbar.account')"
        aria-haspopup="menu"
        @click="menu?.toggle($event)"
      >
        <img
          v-if="app.github?.avatarUrl"
          :src="app.github.avatarUrl"
          alt=""
          class="avatar"
          referrerpolicy="no-referrer"
        >
        <span
          v-else
          class="avatar placeholder"
        ><i class="pi pi-user" /></span>
      </button>
      <Menu
        ref="menu"
        :model="accountItems"
        popup
      />
    </div>
  </header>
</template>

<style scoped>
.topbar {
  position: sticky;
  top: 0;
  z-index: 20;
  display: flex;
  align-items: center;
  gap: 12px;
  height: var(--iw-topbar);
  padding: 0 var(--iw-gutter);
  background: color-mix(in srgb, var(--iw-bg) 82%, transparent);
  backdrop-filter: blur(12px);
  border-bottom: 1px solid var(--iw-border);
}

.topbar-title {
  min-width: 0;
  font-size: calc(16px * var(--iw-fs, 1));
  font-weight: 600;
}

.crumbs {
  display: flex;
  align-items: center;
  gap: 8px;
  white-space: nowrap;
}

.crumbs h1 {
  overflow: hidden;
  font-size: inherit;
  text-overflow: ellipsis;
}

.crumb-link {
  color: var(--iw-muted);
  font-weight: 500;
}

.crumb-link:hover {
  color: var(--iw-text);
}

.crumbs .sep {
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-dimmed);
}

.topbar-right {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 6px;
}

.sync-chip {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  height: 32px;
  padding: 0 12px;
  margin-right: 4px;
  border: 1px solid var(--iw-border);
  border-radius: 999px;
  background: var(--iw-surface);
  color: var(--iw-muted);
  font: inherit;
  font-size: calc(12.5px * var(--iw-fs, 1));
  font-weight: 500;
  cursor: pointer;
  transition: border-color 140ms ease;
}

.sync-chip:hover {
  border-color: var(--iw-border-strong);
}

.bg-spin {
  font-size: calc(11px * var(--iw-fs, 1));
  color: var(--iw-dimmed);
  animation: spin 1.6s linear infinite;
}

.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--iw-dimmed);
}

.ok .dot {
  background: var(--iw-success);
  box-shadow: 0 0 0 3px var(--iw-success-soft);
}

.busy .dot {
  background: var(--iw-primary);
  animation: pulse 1s ease-in-out infinite;
}

.warn .dot {
  background: var(--iw-warn);
}

.error .dot {
  background: var(--iw-danger);
}

.error {
  color: var(--iw-danger);
}

.sync-pop {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 260px;
  font-size: calc(13px * var(--iw-fs, 1));
}

.sync-pop .row {
  display: flex;
  justify-content: space-between;
  gap: 16px;
}

.sync-pop .row.warn {
  color: var(--iw-warn);
}

.sync-pop .err {
  margin: 0;
  padding: 8px 10px;
  border-radius: 8px;
  color: var(--iw-danger);
  background: var(--iw-danger-soft);
  overflow-wrap: anywhere;
}

.avatar-btn {
  padding: 0;
  margin-left: 6px;
  border: 0;
  background: none;
  cursor: pointer;
  border-radius: 50%;
}

.avatar {
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  border-radius: 50%;
  border: 2px solid var(--iw-border);
  object-fit: cover;
}

.avatar.placeholder {
  color: var(--iw-muted);
  background: var(--iw-elevated);
}

.icon-btn {
  display: grid;
  place-items: center;
  width: 36px;
  height: 36px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: var(--iw-text);
  cursor: pointer;
}

.only-mobile {
  display: none;
}

:deep(.spinning .pi-sync) {
  animation: spin 1s linear infinite;
}

@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}

@keyframes pulse {
  50% {
    opacity: 0.35;
  }
}

@media (width <= 899px) {
  .only-mobile {
    display: grid;
  }
}

@media (width <= 599px) {
  .sync-text {
    display: none;
  }
}
</style>
