<script setup lang="ts">
import Button from 'primevue/button'
import ProgressBar from 'primevue/progressbar'
import Select from 'primevue/select'
import SelectButton from 'primevue/selectbutton'
import Tag from 'primevue/tag'
import ToggleSwitch from 'primevue/toggleswitch'
import { computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import type { UpdateChannel } from '../../api/types'
import { useAppStore } from '../../stores/app'
import { useSettingsStore } from '../../stores/settings'
import { useUpdatesStore } from '../../stores/updates'
import { useSave } from '../../lib/save'
import { absTime, relTime } from '../../lib/format'

const { t } = useI18n()
const app = useAppStore()
const settings = useSettingsStore()
const updates = useUpdatesStore()
const save = useSave()
const s = computed(() => settings.doc?.settings.updates)
const st = computed(() => updates.status)

const channels = computed(() => [
  { label: t('settings.updates.stable'), value: 'stable' },
  { label: t('settings.updates.preview'), value: 'preview' },
])
const intervals = computed(() =>
  [6, 12, 24, 72, 168].map((h) => ({ label: t('settings.updates.everyHours', { n: h }), value: h })),
)

const percent = computed(() => {
  const x = st.value
  return x?.total ? Math.min(100, Math.round(((x.done ?? 0) / x.total) * 100)) : 0
})
const mb = (n?: number) => ((n ?? 0) / 1048576).toFixed(1)
const errorText = computed(() => updates.error || st.value?.error || '')
const canInstall = computed(() => !!st.value?.updateAvailable && !st.value.devBuild)
// The outcome of the last update is news only for a few days.
const RESULT_TTL = 3 * 24 * 3600 * 1000
const lastResult = computed(() => {
  const r = st.value?.lastResult
  return r && Date.now() - Date.parse(r.at) < RESULT_TTL ? r : undefined
})

async function setChannel(v: UpdateChannel) {
  await save({ updates: { channel: v } })
  await updates.check()
}

onMounted(() => void updates.load())
</script>

<template>
  <SettingsPanel :title="t('settings.sections.updates')">
    <SettingRow :title="t('settings.version')">
      <template #text>
        <span class="status">
          <span class="mono current">{{ st?.current || app.version || 'dev' }}</span>
          <Tag
            v-if="st?.devBuild"
            severity="secondary"
            :value="t('settings.updates.devBuild')"
          />
          <template v-if="st?.updateAvailable && st.available">
            <span class="mono next">→ {{ st.available.version }}</span>
            <Tag
              v-if="st.available.prerelease"
              severity="warn"
              :value="t('settings.updates.preview')"
            />
          </template>
          <span v-else-if="st?.available">· {{ t('settings.updates.upToDate') }}</span>
          <span v-else-if="st?.checkedAt">· {{ t('settings.updates.noReleases') }}</span>
          <span
            v-if="st?.checkedAt"
            v-tooltip.top="absTime(st.checkedAt)"
          >· {{ t('settings.updates.checked', { when: relTime(st.checkedAt) }) }}</span>
        </span>
      </template>
      <Button
        :label="t('settings.updates.checkNow')"
        icon="pi pi-refresh"
        size="small"
        severity="secondary"
        :loading="st?.state === 'checking'"
        :disabled="updates.busy"
        @click="updates.check()"
      />
      <Button
        v-if="canInstall"
        :label="t('settings.updates.install', { version: st?.available?.version ?? '' })"
        icon="pi pi-download"
        size="small"
        :loading="st?.state === 'downloading' || st?.state === 'installing' || st?.state === 'restarting'"
        :disabled="updates.busy"
        @click="updates.install()"
      />
      <span
        v-if="st?.devBuild && st.updateAvailable"
        class="muted"
      >{{ t('settings.updates.devBuildText') }}</span>
    </SettingRow>

    <div
      v-if="st?.state === 'downloading'"
      class="progress"
    >
      <ProgressBar
        :value="percent"
        :show-value="false"
        style="height: 6px"
      />
      <span class="muted mono">{{ mb(st.done) }} / {{ mb(st.total) }} MB</span>
    </div>
    <p
      v-else-if="st?.state === 'installing' || st?.state === 'restarting'"
      class="state"
      role="status"
    >
      <i class="pi pi-spin pi-spinner" /> {{ t('settings.updates.' + st.state) }}
    </p>

    <p
      v-if="errorText"
      class="error"
      role="alert"
    >
      <i class="pi pi-exclamation-triangle" /> {{ errorText }}
    </p>
    <p
      v-if="lastResult"
      class="result"
      :class="{ error: !lastResult.ok }"
    >
      <i :class="lastResult.ok ? 'pi pi-check-circle' : 'pi pi-undo'" />
      <template v-if="lastResult.ok">
        {{ t('settings.updates.updated', { from: lastResult.from || '?', to: lastResult.to, when: absTime(lastResult.at) }) }}
      </template>
      <template v-else>
        {{ t('settings.updates.rolledBack', { to: lastResult.to, error: lastResult.error }) }}
      </template>
    </p>

    <details
      v-if="st?.available?.notes"
      class="notes"
      :open="st.updateAvailable"
    >
      <summary>{{ t('settings.updates.notes', { version: st.available.version }) }}</summary>
      <pre>{{ st.available.notes }}</pre>
      <a
        v-if="st.available.url"
        :href="st.available.url"
        target="_blank"
        rel="noopener"
      >{{ t('settings.updates.onGitHub') }}</a>
    </details>
  </SettingsPanel>

  <SettingsPanel
    v-if="s"
    :title="t('settings.updates.prefs')"
  >
    <SettingRow
      :title="t('settings.updates.channel')"
      :text="t('settings.updates.channelText')"
    >
      <SelectButton
        :model-value="s.channel"
        :options="channels"
        option-label="label"
        option-value="value"
        :allow-empty="false"
        :disabled="updates.busy"
        :aria-label="t('settings.updates.channel')"
        @update:model-value="(v: UpdateChannel) => setChannel(v)"
      />
    </SettingRow>
    <SettingRow
      :title="t('settings.updates.autoCheck')"
      :text="t('settings.updates.autoCheckText')"
    >
      <ToggleSwitch
        :model-value="s.autoCheck"
        :disabled="settings.saving > 0"
        :aria-label="t('settings.updates.autoCheck')"
        @update:model-value="(v: boolean) => save({ updates: { autoCheck: v } })"
      />
    </SettingRow>
    <SettingRow :title="t('settings.updates.interval')">
      <Select
        :model-value="s.intervalHours"
        :options="intervals"
        option-label="label"
        option-value="value"
        :disabled="!s.autoCheck || settings.saving > 0"
        :aria-label="t('settings.updates.interval')"
        @update:model-value="(v: number) => save({ updates: { intervalHours: v } })"
      />
    </SettingRow>
  </SettingsPanel>
</template>

<style scoped>
.status {
  display: inline-flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
}

.current,
.next {
  color: var(--iw-text);
}

.progress {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-top: 12px;
}

.progress :deep(.p-progressbar) {
  flex: 1;
}

.state,
.error,
.result {
  margin: 12px 0 0;
  font-size: calc(13px * var(--iw-fs, 1));
}

.error {
  color: var(--iw-danger);
}

.result:not(.error) {
  color: var(--iw-success);
}

.notes {
  margin-top: 14px;
}

.notes summary {
  cursor: pointer;
  font-weight: 600;
}

.notes pre {
  max-height: 320px;
  overflow: auto;
  margin: 8px 0;
  padding: 10px 12px;
  border-radius: var(--iw-radius-sm);
  background: var(--iw-elevated);
  font-size: calc(12px * var(--iw-fs, 1));
  white-space: pre-wrap;
}
</style>
