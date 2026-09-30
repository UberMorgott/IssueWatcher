<script setup lang="ts">
import SelectButton from 'primevue/selectbutton'
import InputNumber from 'primevue/inputnumber'
import Button from 'primevue/button'
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import { useSettingsStore } from '../../stores/settings'
import { useAppStore } from '../../stores/app'
import { useSave } from '../../lib/save'
import { absTime } from '../../lib/format'
import type { ProviderSync, SyncMode } from '../../api/types'

const { t } = useI18n()
const settings = useSettingsStore()
const app = useAppStore()
const save = useSave()
const sy = computed(() => settings.doc?.settings.sync)
// Live counters (budget, 304s, quota) refresh while this section is open.
let poll: number | undefined
onMounted(() => {
  void app.loadSync()
  poll = window.setInterval(() => void app.loadSync(), 15000)
})
onBeforeUnmount(() => window.clearInterval(poll))
const custom = computed(() => sy.value?.mode === 'custom')

const plan = computed<ProviderSync | undefined>(() => {
  const s = sy.value
  if (!s) return undefined
  return s.mode === 'custom' ? s.providers.github : settings.doc?.info.syncPresets[s.mode]
})

const modes = computed(() => (['balanced', 'fast', 'custom'] as const).map((m) => ({ label: t('settings.sync.' + m), value: m })))

const FIELDS = [
  { key: 'activeMinutes', min: 1, max: 120, unit: 'min' },
  { key: 'idleMinutes', min: 1, max: 720, unit: 'min' },
  { key: 'reconcileMinutes', min: 15, max: 1440, unit: 'min' },
  { key: 'hourlyBudget', min: 100, max: 4500, unit: 'req' },
  { key: 'concurrency', min: 1, max: 8, unit: '' },
] as const

function setField(key: keyof ProviderSync, v: number | null) {
  if (v === null) return
  save.later('sync.' + key, { sync: { providers: { github: { [key]: v } } } })
}
</script>

<template>
  <template v-if="sy && plan">
    <SettingsPanel
      :title="t('settings.sync.title')"
      :text="t('settings.sync.text')"
    >
      <SettingRow
        :title="t('settings.sync.mode')"
        :text="t('settings.sync.modeText.' + sy.mode)"
      >
        <SelectButton
          :model-value="sy.mode"
          :options="modes"
          option-label="label"
          option-value="value"
          :allow-empty="false"
          :aria-label="t('settings.sync.mode')"
          @update:model-value="(v: SyncMode) => save({ sync: { mode: v } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.sync.activeDays')"
        :text="t('settings.sync.activeDaysText')"
      >
        <InputNumber
          :model-value="sy.activeDays"
          :min="1"
          :max="365"
          show-buttons
          :suffix="' ' + t('settings.unit.days')"
          input-class="num-input"
          :aria-label="t('settings.sync.activeDays')"
          @update:model-value="(v: number | null) => v && save.later('sync.activeDays', { sync: { activeDays: v } })"
        />
      </SettingRow>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.sync.intervals')">
      <SettingRow
        v-for="f in FIELDS"
        :key="f.key"
        :title="t('settings.sync.fields.' + f.key)"
        :text="t('settings.sync.fieldsText.' + f.key)"
      >
        <InputNumber
          :model-value="plan[f.key]"
          :min="f.min"
          :max="f.max"
          show-buttons
          :suffix="f.unit ? ' ' + t('settings.unit.' + f.unit) : ''"
          :disabled="!custom"
          input-class="num-input"
          :aria-label="t('settings.sync.fields.' + f.key)"
          @update:model-value="(v: number | null) => setField(f.key, v)"
        />
      </SettingRow>
      <p
        v-if="!custom"
        class="hint"
      >
        <i class="pi pi-info-circle" /> {{ t('settings.sync.presetHint') }}
      </p>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.sync.status')">
      <SettingRow
        :title="t('settings.lastSync')"
        :text="app.sync?.lastError || t('settings.lastSyncText')"
      >
        <span class="value">{{ absTime(app.sync?.lastSync) || t('common.never') }}</span>
        <Button
          :label="t('common.syncNow')"
          icon="pi pi-sync"
          size="small"
          severity="secondary"
          :loading="app.syncing"
          :disabled="!app.anyConnected"
          @click="app.syncNow()"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.sync.rateLimit')"
        :text="app.sync?.rateLimitedUntil ? t('settings.sync.limitedUntil', { time: absTime(app.sync.rateLimitedUntil) }) : t('settings.sync.rateOk')"
      >
        <span
          v-if="app.sync?.rate?.limit"
          class="value mono"
        >{{ t('settings.sync.remaining', { n: app.sync.rate.remaining, limit: app.sync.rate.limit }) }}</span>
        <span
          class="value"
          :class="app.sync?.rateLimitedUntil ? 'warn' : 'on'"
        >{{ app.sync?.rateLimitedUntil ? t('settings.sync.limited') : t('settings.sync.ok') }}</span>
      </SettingRow>
      <SettingRow
        v-if="app.sync?.budget"
        :title="t('settings.sync.budgetUsed')"
        :text="t('settings.sync.budgetText', { checks: app.sync.checks ?? 0, free: app.sync.notModified ?? 0 })"
      >
        <span class="value mono">{{ app.sync.budgetUsed ?? 0 }} / {{ app.sync.budget }}</span>
      </SettingRow>
      <SettingRow
        v-if="app.sync?.projects"
        :title="t('settings.sync.projectsTitle')"
        :text="app.sync.nextReconcile ? t('settings.sync.nextReconcile', { time: absTime(app.sync.nextReconcile) }) : ''"
      >
        <span class="value">{{ t('settings.sync.activeOf', { n: app.sync.activeProjects ?? 0, total: app.sync.projects }) }}</span>
      </SettingRow>
    </SettingsPanel>
  </template>
</template>

<style scoped>
.hint {
  margin: 12px 0 0;
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
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

:deep(.num-input) {
  width: 140px;
}
</style>
