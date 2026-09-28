<script setup lang="ts">
import Button from 'primevue/button'
import { useI18n } from 'vue-i18n'
import SettingsPanel from '../../components/SettingsPanel.vue'

const { t } = useI18n()
</script>

<template>
  <SettingsPanel :title="t('settings.agents')">
    <template #actions>
      <span class="phase-badge">{{ t('common.phase', { n: '2 / 3' }) }}</span>
    </template>
    <p class="text">
      {{ t('settings.agentsText') }}
    </p>
    <ul class="list">
      <li>{{ t('settings.agentsPlan.roles') }}</li>
      <li>{{ t('settings.agentsPlan.cli') }}</li>
      <li>{{ t('settings.agentsPlan.prompts') }}</li>
      <li>{{ t('settings.agentsPlan.limits') }}</li>
    </ul>
    <Button
      as="router-link"
      to="/agents"
      :label="t('settings.agentsOpen')"
      icon="pi pi-arrow-right"
      icon-pos="right"
      severity="secondary"
      size="small"
      class="go"
    />
  </SettingsPanel>
</template>

<style scoped>
.text {
  margin: 4px 0 8px;
  color: var(--iw-muted);
}

.list {
  margin: 0 0 12px;
  padding-left: 20px;
  color: var(--iw-muted);
  font-size: 13px;
}

.go {
  align-self: flex-start;
}
</style>
