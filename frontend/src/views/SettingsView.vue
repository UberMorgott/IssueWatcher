<script setup lang="ts">
import SelectButton from 'primevue/selectbutton'
import ToggleSwitch from 'primevue/toggleswitch'
import Button from 'primevue/button'
import { useAppStore, type Theme } from '../stores/app'
import { liveConnected } from '../api/live'
import { absTime } from '../lib/format'

const app = useAppStore()
const themes = [
  { label: 'Dark', value: 'dark', icon: 'pi pi-moon' },
  { label: 'Light', value: 'light', icon: 'pi pi-sun' },
]
</script>

<template>
  <div class="page narrow">
    <div class="page-head">
      <div>
        <h2 class="page-title">
          Settings
        </h2>
        <p class="page-sub">
          Everything is stored next to the exe in <span class="mono">data\</span> — portable, no registry.
        </p>
      </div>
    </div>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">Appearance</span>
      </div>
      <div class="panel-body rows">
        <div class="row">
          <div>
            <div class="row-title">
              Theme
            </div>
            <div class="row-text">
              Dark is the default. Saved in this browser.
            </div>
          </div>
          <SelectButton
            :model-value="app.theme"
            :options="themes"
            option-label="label"
            option-value="value"
            :allow-empty="false"
            aria-label="Theme"
            @update:model-value="(v: Theme) => app.setTheme(v)"
          >
            <template #option="{ option }">
              <i :class="option.icon" /> {{ option.label }}
            </template>
          </SelectButton>
        </div>
        <div class="row">
          <div>
            <div class="row-title">
              Compact sidebar
            </div>
            <div class="row-text">
              Icons only. Shortcut <kbd class="mono">[</kbd>.
            </div>
          </div>
          <ToggleSwitch
            :model-value="app.sidebarCollapsed"
            aria-label="Compact sidebar"
            @update:model-value="app.toggleSidebar()"
          />
        </div>
      </div>
    </section>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">Sync</span>
      </div>
      <div class="panel-body rows">
        <div class="row">
          <div>
            <div class="row-title">
              Poll interval
            </div>
            <div class="row-text">
              Set <span class="mono">pollIntervalMinutes</span> in <span class="mono">data\config.json</span> (default 5, min 1); applies on restart.
            </div>
          </div>
          <span class="mono value">{{ app.sync?.interval || '—' }}</span>
        </div>
        <div class="row">
          <div>
            <div class="row-title">
              Last sync
            </div>
            <div class="row-text">
              {{ app.sync?.lastError ? app.sync.lastError : 'Issues, comments and closes from all installed repositories.' }}
            </div>
          </div>
          <div class="row-actions">
            <span class="value">{{ absTime(app.sync?.lastSync) || 'never' }}</span>
            <Button
              label="Sync now"
              icon="pi pi-sync"
              size="small"
              severity="secondary"
              :loading="app.syncing"
              :disabled="!app.githubConnected"
              @click="app.syncNow()"
            />
          </div>
        </div>
      </div>
    </section>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">Notifications</span>
      </div>
      <div class="panel-body rows">
        <div class="row">
          <div>
            <div class="row-title">
              Tray notifications
            </div>
            <div class="row-text">
              New issue, new comment and closed issue show a Windows notification; clicking it opens the issue in this tab.
            </div>
          </div>
          <span class="value on">On</span>
        </div>
        <div class="row">
          <div>
            <div class="row-title">
              Live updates in this tab
            </div>
            <div class="row-text">
              Toasts and counters update without reloading.
            </div>
          </div>
          <span
            class="value"
            :class="liveConnected ? 'on' : 'warn'"
          >{{ liveConnected ? 'Connected' : 'Reconnecting…' }}</span>
        </div>
      </div>
    </section>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">Agents &amp; prompts</span>
        <span class="phase-badge">Phase 2 / 3</span>
      </div>
      <div class="panel-body">
        <p class="row-text">
          Agent profiles, concurrency limits and prompt layers will live here.
        </p>
      </div>
    </section>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">About</span>
      </div>
      <div class="panel-body rows">
        <div class="row">
          <div class="row-title">
            Version
          </div><span class="mono value">{{ app.version || 'dev' }}</span>
        </div>
        <div class="row">
          <div class="row-title">
            Keyboard shortcuts
          </div><span class="value">press <kbd class="mono">?</kbd></span>
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.narrow {
  max-width: 880px;
}

.rows {
  display: flex;
  flex-direction: column;
  padding-top: 8px;
}

.row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
  padding: 14px 0;
  border-bottom: 1px solid var(--iw-border);
}

.row:last-child {
  border-bottom: 0;
}

.row-title {
  font-weight: 500;
}

.row-text {
  margin: 0;
  font-size: 13px;
  color: var(--iw-muted);
}

.row-actions {
  display: flex;
  align-items: center;
  gap: 12px;
  white-space: nowrap;
}

.value {
  color: var(--iw-muted);
  white-space: nowrap;
}

.value.on {
  color: var(--iw-success);
}

.value.warn {
  color: var(--iw-warn);
}

kbd {
  padding: 1px 6px;
  border-radius: 5px;
  border: 1px solid var(--iw-border-strong);
  background: var(--iw-elevated);
  font-size: 12px;
}
</style>
