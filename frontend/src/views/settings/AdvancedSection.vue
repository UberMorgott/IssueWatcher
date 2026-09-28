<script setup lang="ts">
import Button from 'primevue/button'
import Select from 'primevue/select'
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import { useSettingsStore } from '../../stores/settings'
import { liveConnected } from '../../api/live'
import type { Settings } from '../../api/types'

const { t } = useI18n()
const settings = useSettingsStore()
const confirm = useConfirm()
const toast = useToast()
const info = computed(() => settings.doc?.info)

const RESETTABLE = ['general', 'appearance', 'notifications', 'sync', 'projects'] as const
const section = ref<(typeof RESETTABLE)[number]>('appearance')
const sections = computed(() => RESETTABLE.map((s) => ({ label: t('settings.sections.' + s), value: s })))

function askReset() {
  const s = section.value
  confirm.require({
    header: t('settings.advanced.resetTitle'),
    message: t('settings.advanced.resetConfirm', { section: t('settings.sections.' + s) }),
    icon: 'pi pi-exclamation-triangle',
    acceptProps: { label: t('settings.advanced.reset'), severity: 'danger' },
    rejectProps: { label: t('common.cancel'), severity: 'secondary', outlined: true },
    accept: async () => {
      const err = await settings.reset(s as keyof Settings)
      toast.add(err
        ? { severity: 'error', summary: t('settings.saveFailed'), detail: err, life: 5000 }
        : { severity: 'success', summary: t('settings.advanced.resetDone'), life: 3000 })
    },
  })
}
</script>

<template>
  <template v-if="info">
    <SettingsPanel :title="t('settings.advanced.storage')">
      <SettingRow
        :title="t('settings.advanced.dataDir')"
        :text="t('settings.advanced.dataDirText')"
      >
        <span class="mono path">{{ info.dataDir }}</span>
      </SettingRow>
      <SettingRow :title="t('settings.advanced.configFile')">
        <span class="mono path">{{ info.configFile }}</span>
      </SettingRow>
      <SettingRow
        :title="t('settings.advanced.export')"
        :text="t('settings.advanced.exportText')"
      >
        <Button
          as="a"
          href="/api/settings/export"
          download="issuewatcher-settings.json"
          :label="t('settings.advanced.exportBtn')"
          icon="pi pi-download"
          size="small"
          severity="secondary"
        />
      </SettingRow>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.advanced.diagnostics')">
      <SettingRow :title="t('settings.version')">
        <span class="mono path">{{ info.version }}</span>
      </SettingRow>
      <SettingRow :title="t('settings.advanced.exe')">
        <span class="mono path">{{ info.exe }}</span>
      </SettingRow>
      <SettingRow :title="t('settings.advanced.revision')">
        <span class="mono path">{{ settings.doc?.revision }}</span>
      </SettingRow>
      <SettingRow
        :title="t('settings.live')"
        :text="t('settings.liveText')"
      >
        <span
          class="state"
          :class="liveConnected ? 'on' : 'warn'"
        >{{ liveConnected ? t('settings.connected') : t('settings.reconnecting') }}</span>
      </SettingRow>
      <SettingRow
        :title="t('settings.advanced.logs')"
        :text="t('settings.advanced.logsText')"
      >
        <span class="mono path">{{ info.dataDir }}\logs\issuewatcher.log</span>
      </SettingRow>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.advanced.resetTitle')">
      <SettingRow
        :title="t('settings.advanced.resetSection')"
        :text="t('settings.advanced.resetText')"
      >
        <Select
          v-model="section"
          :options="sections"
          option-label="label"
          option-value="value"
          :aria-label="t('settings.advanced.resetSection')"
        />
        <Button
          :label="t('settings.advanced.reset')"
          icon="pi pi-replay"
          size="small"
          severity="danger"
          outlined
          @click="askReset"
        />
      </SettingRow>
    </SettingsPanel>
  </template>
</template>

<style scoped>
.path {
  font-size: 12.5px;
  color: var(--iw-muted);
  overflow-wrap: anywhere;
  text-align: right;
}

.state.on {
  color: var(--iw-success);
}

.state.warn {
  color: var(--iw-warn);
}
</style>
