<script setup lang="ts">
import SelectButton from 'primevue/selectbutton'
import ToggleSwitch from 'primevue/toggleswitch'
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import { useSettingsStore } from '../../stores/settings'
import { useSave } from '../../lib/save'
import { LANGS } from '../../i18n'

const { t } = useI18n()
const settings = useSettingsStore()
const save = useSave()
const s = computed(() => settings.doc?.settings)
const langs = computed(() => LANGS.map((l) => ({ label: t('lang.' + l), value: l })))
</script>

<template>
  <template v-if="s">
    <SettingsPanel :title="t('settings.interface')">
      <SettingRow
        :title="t('settings.language')"
        :text="t('settings.languageText')"
      >
        <SelectButton
          :model-value="s.general.language"
          :options="langs"
          option-label="label"
          option-value="value"
          :allow-empty="false"
          :aria-label="t('settings.language')"
          @update:model-value="(v: 'ru' | 'en') => save({ general: { language: v } })"
        />
      </SettingRow>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.startup')">
      <SettingRow
        :title="t('settings.autostart')"
        :text="t('settings.autostartText')"
      >
        <ToggleSwitch
          :model-value="s.general.startWithWindows"
          :disabled="settings.saving > 0"
          :aria-label="t('settings.autostart')"
          @update:model-value="(v: boolean) => save({ general: { startWithWindows: v } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.minimized')"
        :text="t('settings.minimizedText')"
      >
        <ToggleSwitch
          :model-value="s.general.startMinimized"
          :disabled="settings.saving > 0"
          :aria-label="t('settings.minimized')"
          @update:model-value="(v: boolean) => save({ general: { startMinimized: v } })"
        />
      </SettingRow>
      <p class="hint">
        <i class="pi pi-info-circle" /> {{ t('settings.appHint') }}
      </p>
    </SettingsPanel>
  </template>
</template>

<style scoped>
.hint {
  margin: 12px 0 0;
  font-size: 13px;
  color: var(--iw-muted);
}
</style>
