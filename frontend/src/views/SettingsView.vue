<script setup lang="ts">
import SelectButton from 'primevue/selectbutton'
import ToggleSwitch from 'primevue/toggleswitch'
import Button from 'primevue/button'
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore, type Theme } from '../stores/app'
import { liveConnected } from '../api/live'
import { absTime, duration } from '../lib/format'
import { LANGS, lang, setLang, type Lang } from '../i18n'

const app = useAppStore()
const { t } = useI18n()
const themes = computed(() => [
  { label: t('settings.dark'), value: 'dark', icon: 'pi pi-moon' },
  { label: t('settings.light'), value: 'light', icon: 'pi pi-sun' },
])
const langs = computed(() => LANGS.map((l) => ({ label: t('lang.' + l), value: l })))
</script>

<template>
  <div class="page narrow">
    <div class="page-head">
      <div>
        <h2 class="page-title">
          {{ t('nav.settings') }}
        </h2>
        <i18n-t
          keypath="settings.sub"
          tag="p"
          class="page-sub"
          scope="global"
        >
          <template #path>
            <span class="mono">data\</span>
          </template>
        </i18n-t>
      </div>
    </div>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">{{ t('settings.appearance') }}</span>
      </div>
      <div class="panel-body rows">
        <div class="row">
          <div>
            <div class="row-title">
              {{ t('settings.language') }}
            </div>
            <div class="row-text">
              {{ t('settings.languageText') }}
            </div>
          </div>
          <SelectButton
            :model-value="lang()"
            :options="langs"
            option-label="label"
            option-value="value"
            :allow-empty="false"
            :aria-label="t('settings.language')"
            @update:model-value="(v: Lang) => setLang(v)"
          />
        </div>
        <div class="row">
          <div>
            <div class="row-title">
              {{ t('settings.theme') }}
            </div>
            <div class="row-text">
              {{ t('settings.themeText') }}
            </div>
          </div>
          <SelectButton
            :model-value="app.theme"
            :options="themes"
            option-label="label"
            option-value="value"
            :allow-empty="false"
            :aria-label="t('settings.theme')"
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
              {{ t('settings.compact') }}
            </div>
            <i18n-t
              keypath="settings.compactText"
              tag="div"
              class="row-text"
              scope="global"
            >
              <template #key>
                <kbd class="mono">[</kbd>
              </template>
            </i18n-t>
          </div>
          <ToggleSwitch
            :model-value="app.sidebarCollapsed"
            :aria-label="t('settings.compact')"
            @update:model-value="app.toggleSidebar()"
          />
        </div>
      </div>
    </section>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">{{ t('settings.sync') }}</span>
      </div>
      <div class="panel-body rows">
        <div class="row">
          <div>
            <div class="row-title">
              {{ t('settings.poll') }}
            </div>
            <i18n-t
              keypath="settings.pollText"
              tag="div"
              class="row-text"
              scope="global"
            >
              <template #key>
                <span class="mono">pollIntervalMinutes</span>
              </template>
              <template #file>
                <span class="mono">data\config.json</span>
              </template>
            </i18n-t>
          </div>
          <span class="mono value">{{ duration(app.sync?.interval) || '—' }}</span>
        </div>
        <div class="row">
          <div>
            <div class="row-title">
              {{ t('settings.lastSync') }}
            </div>
            <div class="row-text">
              {{ app.sync?.lastError ? app.sync.lastError : t('settings.lastSyncText') }}
            </div>
          </div>
          <div class="row-actions">
            <span class="value">{{ absTime(app.sync?.lastSync) || t('common.never') }}</span>
            <Button
              :label="t('common.syncNow')"
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
        <span class="panel-title">{{ t('settings.notifications') }}</span>
      </div>
      <div class="panel-body rows">
        <div class="row">
          <div>
            <div class="row-title">
              {{ t('settings.tray') }}
            </div>
            <div class="row-text">
              {{ t('settings.trayText') }}
            </div>
          </div>
          <span class="value on">{{ t('settings.on') }}</span>
        </div>
        <div class="row">
          <div>
            <div class="row-title">
              {{ t('settings.live') }}
            </div>
            <div class="row-text">
              {{ t('settings.liveText') }}
            </div>
          </div>
          <span
            class="value"
            :class="liveConnected ? 'on' : 'warn'"
          >{{ liveConnected ? t('settings.connected') : t('settings.reconnecting') }}</span>
        </div>
      </div>
    </section>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">{{ t('settings.agents') }}</span>
        <span class="phase-badge">{{ t('common.phase', { n: '2 / 3' }) }}</span>
      </div>
      <div class="panel-body">
        <p class="row-text">
          {{ t('settings.agentsText') }}
        </p>
      </div>
    </section>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">{{ t('settings.about') }}</span>
      </div>
      <div class="panel-body rows">
        <div class="row">
          <div class="row-title">
            {{ t('settings.version') }}
          </div><span class="mono value">{{ app.version || 'dev' }}</span>
        </div>
        <div class="row">
          <div class="row-title">
            {{ t('settings.shortcuts') }}
          </div><i18n-t
            keypath="settings.press"
            tag="span"
            class="value"
            scope="global"
          >
            <template #key>
              <kbd class="mono">?</kbd>
            </template>
          </i18n-t>
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
