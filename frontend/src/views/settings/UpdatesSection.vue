<script setup lang="ts">
import Button from 'primevue/button'
import { useI18n } from 'vue-i18n'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import { useAppStore } from '../../stores/app'

const { t } = useI18n()
const app = useAppStore()
</script>

<template>
  <SettingsPanel :title="t('settings.sections.updates')">
    <SettingRow :title="t('settings.version')">
      <span class="mono value">{{ app.version || 'dev' }}</span>
    </SettingRow>
    <SettingRow
      :title="t('settings.updates.check')"
      :text="t('settings.updates.soon')"
    >
      <span v-tooltip.left="t('settings.updates.soon')">
        <Button
          :label="t('settings.updates.checkNow')"
          icon="pi pi-refresh"
          size="small"
          severity="secondary"
          disabled
        />
      </span>
    </SettingRow>
  </SettingsPanel>
</template>

<style scoped>
.value {
  color: var(--iw-muted);
}
</style>
