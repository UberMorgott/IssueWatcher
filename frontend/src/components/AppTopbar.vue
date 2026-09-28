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
import { absTime, duration, relTime } from '../lib/format'

defineEmits<{ menu: [] }>()
const app = useAppStore()
const route = useRoute()
const router = useRouter()
const toast = useToast()
const { t } = useI18n()

const title = computed(() => routeTitle(route))

type Tone = 'ok' | 'busy' | 'warn' | 'error' | 'off'
const status = computed<{ tone: Tone; text: string }>(() => {
  const s = app.sync
  if (!app.githubConnected) return { tone: 'off', text: t('topbar.notConnected') }
  if (app.syncing) return { tone: 'busy', text: t('topbar.syncing') }
  if (!s) return { tone: 'off', text: t('topbar.syncUnavailable') }
  if (s.rateLimitedUntil) return { tone: 'warn', text: t('topbar.rateLimited') }
  if (s.lastError) return { tone: 'error', text: t('topbar.syncError') }
  if (s.lastSync) return { tone: 'ok', text: t('topbar.synced', { time: relTime(s.lastSync) }) }
  return { tone: 'off', text: t('topbar.waitingFirstSync') }
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
    <h1 class="topbar-title">
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
      </button>
      <Popover ref="pop">
        <div class="sync-pop">
          <div class="row">
            <span class="muted">GitHub</span><span>{{ app.githubConnected ? '@' + app.github?.login : t('topbar.notConnectedLower') }}</span>
          </div>
          <div class="row">
            <span class="muted">{{ t('topbar.lastSync') }}</span><span>{{ absTime(app.sync?.lastSync) || t('common.never') }}</span>
          </div>
          <div class="row">
            <span class="muted">{{ t('topbar.interval') }}</span><span class="mono">{{ duration(app.sync?.interval) || '—' }}</span>
          </div>
          <div class="row">
            <span class="muted">{{ t('topbar.liveUpdates') }}</span><span>{{ liveConnected ? t('topbar.liveConnected') : t('topbar.liveReconnecting') }}</span>
          </div>
          <div
            v-if="app.sync?.rateLimitedUntil"
            class="row warn"
          >
            <span>{{ t('topbar.rateLimitedUntil') }}</span><span>{{ absTime(app.sync.rateLimitedUntil) }}</span>
          </div>
          <p
            v-if="app.sync?.lastError"
            class="err"
          >
            {{ app.sync.lastError }}
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
        :disabled="!app.githubConnected || app.syncing"
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
  font-size: 16px;
  font-weight: 600;
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
  font-size: 12.5px;
  font-weight: 500;
  cursor: pointer;
  transition: border-color 140ms ease;
}

.sync-chip:hover {
  border-color: var(--iw-border-strong);
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
  font-size: 13px;
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
