<script setup lang="ts">
import SelectButton from 'primevue/selectbutton'
import ToggleSwitch from 'primevue/toggleswitch'
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import { useSettingsStore } from '../../stores/settings'
import { useAppStore } from '../../stores/app'
import { useSave } from '../../lib/save'
import type { ThemeMode } from '../../api/types'

const { t } = useI18n()
const settings = useSettingsStore()
const app = useAppStore()
const save = useSave()
const s = computed(() => settings.doc?.settings)
const modes = computed(() => [
  { label: t('settings.dark'), value: 'dark', icon: 'pi pi-moon' },
  { label: t('settings.light'), value: 'light', icon: 'pi pi-sun' },
  { label: t('settings.system'), value: 'system', icon: 'pi pi-desktop' },
])
</script>

<template>
  <SettingsPanel
    v-if="s"
    :title="t('settings.theme')"
  >
    <SettingRow
      :title="t('settings.mode')"
      :text="t('settings.modeText')"
    >
      <SelectButton
        :model-value="s.appearance.mode"
        :options="modes"
        option-label="label"
        option-value="value"
        :allow-empty="false"
        :aria-label="t('settings.mode')"
        @update:model-value="(v: ThemeMode) => save({ appearance: { mode: v } })"
      >
        <template #option="{ option }">
          <i :class="option.icon" /> {{ option.label }}
        </template>
      </SelectButton>
    </SettingRow>
    <SettingRow :title="t('settings.compact')">
      <template #text>
        <i18n-t
          keypath="settings.compactText"
          scope="global"
        >
          <template #key>
            <kbd class="mono">[</kbd>
          </template>
        </i18n-t>
      </template>
      <ToggleSwitch
        :model-value="app.sidebarCollapsed"
        :aria-label="t('settings.compact')"
        @update:model-value="app.toggleSidebar()"
      />
    </SettingRow>
  </SettingsPanel>
</template>
