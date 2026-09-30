<script setup lang="ts">
import ToggleSwitch from 'primevue/toggleswitch'
import InputNumber from 'primevue/inputnumber'
import InputText from 'primevue/inputtext'
import MultiSelect from 'primevue/multiselect'
import Button from 'primevue/button'
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useToast } from 'primevue/usetoast'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import { useSettingsStore } from '../../stores/settings'
import { useAppStore } from '../../stores/app'
import { useSave } from '../../lib/save'
import { api } from '../../api/client'
import { platformName } from '../../lib/platforms'

const { t } = useI18n()
const settings = useSettingsStore()
const app = useAppStore()
const toast = useToast()
const save = useSave()
const n = computed(() => settings.doc?.settings.notifications)
const off = computed(() => !n.value?.enabled)

const kinds = computed(() => [
  { key: 'newIssue', title: t('settings.notif.newIssue'), icon: 'pi pi-inbox' },
  { key: 'newComment', title: t('settings.notif.newComment'), icon: 'pi pi-comment' },
  { key: 'closed', title: t('settings.notif.closed'), icon: 'pi pi-check-circle' },
] as const)

// Mutes are keyed by project key (platform:id): the same mod name on Nexus,
// CurseForge and Steam is three projects. Muted keys that are no longer synced
// stay selectable so they can be unmuted.
const projectOptions = computed(() => {
  const labels = new Map<string, string>()
  for (const r of app.repos) {
    const p = r.platform || 'github'
    labels.set(r.key || `github:${r.name}`, p === 'github' ? r.name : `${r.name} · ${platformName(p)}`)
  }
  for (const m of n.value?.mutedProjects ?? []) if (!labels.has(m)) labels.set(m, m)
  return [...labels].map(([value, label]) => ({ label, value })).sort((a, b) => a.label.localeCompare(b.label))
})

const testing = ref(false)
async function test() {
  testing.value = true
  const r = await api.testNotification()
  testing.value = false
  if (!r.ok) toast.add({ severity: 'error', summary: t('settings.notif.testFailed'), detail: r.error, life: 5000 })
}

function setTime(which: 'from' | 'to', v: string) {
  if (/^([01]\d|2[0-3]):[0-5]\d$/.test(v)) void save({ notifications: { quiet: { [which]: v } } })
}
</script>

<template>
  <template v-if="n">
    <SettingsPanel :title="t('settings.notif.title')">
      <template #actions>
        <span v-tooltip.top="off ? t('settings.notif.testOff') : undefined">
          <Button
            :label="t('settings.notif.test')"
            icon="pi pi-send"
            size="small"
            severity="secondary"
            :loading="testing"
            :disabled="off"
            @click="test"
          />
        </span>
      </template>
      <SettingRow
        :title="t('settings.notif.enabled')"
        :text="t('settings.notif.enabledText')"
      >
        <ToggleSwitch
          :model-value="n.enabled"
          :aria-label="t('settings.notif.enabled')"
          @update:model-value="(v: boolean) => save({ notifications: { enabled: v } })"
        />
      </SettingRow>
      <SettingRow
        v-for="k in kinds"
        :key="k.key"
        :title="k.title"
      >
        <ToggleSwitch
          :model-value="n[k.key]"
          :disabled="off"
          :aria-label="k.title"
          @update:model-value="(v: boolean) => save({ notifications: { [k.key]: v } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.notif.group')"
        :text="t('settings.notif.groupText')"
      >
        <ToggleSwitch
          :model-value="n.group"
          :disabled="off"
          :aria-label="t('settings.notif.group')"
          @update:model-value="(v: boolean) => save({ notifications: { group: v } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.notif.autoHide')"
        :text="t('settings.notif.autoHideText')"
      >
        <InputNumber
          :model-value="n.autoHideSeconds"
          :min="3"
          :max="120"
          show-buttons
          :suffix="' ' + t('settings.unit.sec')"
          :disabled="off"
          input-class="num-input"
          :aria-label="t('settings.notif.autoHide')"
          @update:model-value="(v: number | null) => v && save.later('autoHide', { notifications: { autoHideSeconds: v } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.notif.dnd')"
        :text="t('settings.notif.dndText')"
      >
        <ToggleSwitch
          :model-value="n.respectDnd"
          :disabled="off"
          :aria-label="t('settings.notif.dnd')"
          @update:model-value="(v: boolean) => save({ notifications: { respectDnd: v } })"
        />
      </SettingRow>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.notif.quiet')">
      <SettingRow
        :title="t('settings.notif.quietOn')"
        :text="t('settings.notif.quietText')"
      >
        <InputText
          type="time"
          :model-value="n.quiet.from"
          :disabled="off || !n.quiet.enabled"
          :aria-label="t('settings.notif.quietFrom')"
          class="time"
          @change="(e: Event) => setTime('from', (e.target as HTMLInputElement).value)"
        />
        <span class="muted">—</span>
        <InputText
          type="time"
          :model-value="n.quiet.to"
          :disabled="off || !n.quiet.enabled"
          :aria-label="t('settings.notif.quietTo')"
          class="time"
          @change="(e: Event) => setTime('to', (e.target as HTMLInputElement).value)"
        />
        <ToggleSwitch
          :model-value="n.quiet.enabled"
          :disabled="off"
          :aria-label="t('settings.notif.quietOn')"
          @update:model-value="(v: boolean) => save({ notifications: { quiet: { enabled: v } } })"
        />
      </SettingRow>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.notif.projects')">
      <SettingRow
        :title="t('settings.notif.muted')"
        :text="t('settings.notif.mutedText')"
        stack
      >
        <MultiSelect
          :model-value="n.mutedProjects"
          :options="projectOptions"
          option-label="label"
          option-value="value"
          filter
          display="chip"
          :placeholder="t('settings.notif.mutedNone')"
          :disabled="off"
          :aria-label="t('settings.notif.muted')"
          class="muted-select"
          @update:model-value="(v: string[]) => save({ notifications: { mutedProjects: v } })"
        />
      </SettingRow>
    </SettingsPanel>
  </template>
</template>

<style scoped>
.time {
  width: 116px;
}

.muted-select {
  width: 100%;
}

:deep(.num-input) {
  width: 90px;
}
</style>
