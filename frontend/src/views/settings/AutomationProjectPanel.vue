<script setup lang="ts">
import { computed, ref } from 'vue'
import InputNumber from 'primevue/inputnumber'
import Select from 'primevue/select'
import { useI18n } from 'vue-i18n'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import type { ProjectAutomation, SettingsPatch } from '../../api/types'
import { useSave } from '../../lib/save'
import { useAppStore } from '../../stores/app'
import { useSettingsStore } from '../../stores/settings'

// Per-project automation overrides (settings.agents.projects[name].automation):
// an unset field inherits the global default of Settings › Агенты › Автоматизация.
const { t } = useI18n()
const settings = useSettingsStore()
const app = useAppStore()
const save = useSave()
const ag = computed(() => settings.doc?.settings.agents)

const projectName = ref('')
const projectOptions = computed(() => {
  const names = new Set(app.repos.map((r) => r.name))
  for (const [n, p] of Object.entries(ag.value?.projects ?? {})) if (p.automation && Object.keys(p.automation).length) names.add(n)
  return [...names]
    .sort((a, b) => a.localeCompare(b))
    .map((n) => ({ label: n + (overridden(n) ? ' •' : ''), value: n }))
})
function overridden(name: string): boolean {
  const o = ag.value?.projects[name]?.automation
  return !!o && Object.values(o).some((v) => v !== undefined && v !== null)
}
const over = computed<ProjectAutomation>(() => (projectName.value ? ag.value?.projects[projectName.value]?.automation : undefined) ?? {})

type BoolKey = 'enabled' | 'allowAutoFix' | 'autoApplyLabels'
type NumKey = 'maxPerDay' | 'maxAttempts'
const BOOLS: BoolKey[] = ['enabled', 'allowAutoFix', 'autoApplyLabels']
// maxPerDay is not inherited: the global value is the total over all projects,
// a project value is an extra cap (empty = none).
const NUMS: { key: NumKey; max: number; title: string; inherits: boolean }[] = [
  { key: 'maxPerDay', max: 500, title: 'projectMaxPerDay', inherits: false },
  { key: 'maxAttempts', max: 10, title: 'maxAttempts', inherits: true },
]
function numText(n: (typeof NUMS)[number]): string {
  const value = ag.value?.automation[n.key]
  return n.inherits
    ? t('settings.automation.inheritNumber', { value })
    : t('settings.automation.projectMaxPerDayText', { total: value })
}

/** null in a merge patch removes the field = inherit. */
function patch(field: keyof ProjectAutomation, v: boolean | number | null): SettingsPatch {
  return { agents: { projects: { [projectName.value]: { automation: { [field]: v } } } } } as unknown as SettingsPatch
}

const onOff = (v: boolean | undefined) => t(v ? 'settings.automation.on' : 'settings.automation.off')
function boolOptions(k: BoolKey) {
  return [
    { label: t('settings.automation.inherit', { value: onOff(ag.value?.automation[k]) }), value: 'inherit' },
    { label: t('settings.automation.on'), value: 'on' },
    { label: t('settings.automation.off'), value: 'off' },
  ]
}
function boolValue(k: BoolKey): string {
  const v = over.value[k]
  return v === undefined || v === null ? 'inherit' : v ? 'on' : 'off'
}
function setBool(k: BoolKey, v: string) {
  void save(patch(k, v === 'inherit' ? null : v === 'on'))
}
function setNum(k: NumKey, v: number | null) {
  save.later('automation.' + projectName.value + '.' + k, patch(k, v || null))
}
</script>

<template>
  <SettingsPanel
    v-if="ag"
    :title="t('settings.automation.projectTitle')"
    :text="t('settings.automation.projectText')"
  >
    <Select
      v-model="projectName"
      :options="projectOptions"
      option-label="label"
      option-value="value"
      filter
      :placeholder="t('settings.agents.pickProject')"
      :aria-label="t('settings.agents.pickProject')"
      class="project-select"
    />
    <template v-if="projectName">
      <SettingRow
        v-for="k in BOOLS"
        :key="k"
        :title="t('settings.automation.' + k)"
      >
        <Select
          :model-value="boolValue(k)"
          :options="boolOptions(k)"
          option-label="label"
          option-value="value"
          :aria-label="t('settings.automation.' + k)"
          class="tri"
          @update:model-value="(v: string) => setBool(k, v)"
        />
      </SettingRow>
      <SettingRow
        v-for="n in NUMS"
        :key="n.key"
        :title="t('settings.automation.' + n.title)"
        :text="numText(n)"
      >
        <InputNumber
          :model-value="over[n.key] ?? null"
          :min="1"
          :max="n.max"
          :placeholder="n.inherits ? String(ag.automation[n.key]) : '—'"
          show-buttons
          input-class="num-input"
          :aria-label="t('settings.automation.' + n.title)"
          @update:model-value="(v: number | null) => setNum(n.key, v)"
        />
      </SettingRow>
    </template>
  </SettingsPanel>
</template>

<style scoped>
.project-select {
  width: 100%;
  max-width: 420px;
  margin: 6px 0 8px;
}

.tri {
  width: 240px;
}

:deep(.num-input) {
  width: 80px;
}
</style>
