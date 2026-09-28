<script setup lang="ts">
import SelectButton from 'primevue/selectbutton'
import ToggleSwitch from 'primevue/toggleswitch'
import Select from 'primevue/select'
import Slider from 'primevue/slider'
import Button from 'primevue/button'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import { useSettingsStore } from '../../stores/settings'
import { useAppStore } from '../../stores/app'
import { useSave } from '../../lib/save'
import { contrast, effectiveColors, FONT_STACKS, theme, tokens } from '../../lib/appearance'
import type { Appearance, Colors, ThemeMode } from '../../api/types'

const { t } = useI18n()
const settings = useSettingsStore()
const app = useAppStore()
const save = useSave()
const a = computed(() => settings.doc?.settings.appearance)
const palettes = computed(() => settings.doc?.info.palettes ?? [])

const modes = computed(() => [
  { label: t('settings.dark'), value: 'dark', icon: 'pi pi-moon' },
  { label: t('settings.light'), value: 'light', icon: 'pi pi-sun' },
  { label: t('settings.system'), value: 'system', icon: 'pi pi-desktop' },
])
const fonts = computed(() => (Object.keys(FONT_STACKS) as Appearance['fontFamily'][]).map((f) => ({ label: t('settings.look.fonts.' + f), value: f })))
const densities = computed(() => (['compact', 'comfortable', 'spacious'] as const).map((d) => ({ label: t('settings.look.density.' + d), value: d })))

// --- custom colours: edit the mode currently shown; a draft previews live, Save validates on the server.
const KEYS = ['accent', 'background', 'surface', 'text'] as const
const editMode = computed<'dark' | 'light'>(() => theme.value)
const draft = ref<Colors | null>(null)
watch(
  [a, editMode],
  () => {
    if (a.value && palettes.value.length) draft.value = effectiveColors(a.value, editMode.value, palettes.value)
  },
  { immediate: true },
)
const hasCustom = computed(() => {
  const c = a.value?.custom?.[editMode.value]
  return !!c && KEYS.some((k) => c[k])
})
const draftContrast = computed(() => {
  const d = draft.value
  if (!d) return { surface: 21, background: 21 }
  return { surface: contrast(d.text, d.surface), background: contrast(d.text, d.background) }
})
const draftOk = computed(() => draftContrast.value.surface >= 4.5 && draftContrast.value.background >= 4.5)
const dirty = computed(() => {
  if (!a.value || !draft.value) return false
  const cur = effectiveColors(a.value, editMode.value, palettes.value)
  return KEYS.some((k) => cur[k] !== draft.value![k])
})
const previewStyle = computed(() => (draft.value ? tokens(draft.value, editMode.value) : {}))

function setColor(k: (typeof KEYS)[number], v: string) {
  if (!draft.value) return
  const hex = v.startsWith('#') ? v : '#' + v
  if (/^#[0-9a-fA-F]{6}$/.test(hex)) draft.value = { ...draft.value, [k]: hex.toLowerCase() }
}
async function saveCustom() {
  if (!draft.value || !a.value) return
  const p = palettes.value.find((x) => x.id === a.value!.paletteId) ?? palettes.value[0]
  // Store only what differs from the palette, so switching presets later keeps working.
  const custom: Partial<Colors> = {}
  for (const k of KEYS) custom[k] = draft.value[k] === p[editMode.value][k] ? '' : draft.value[k]
  await save({ appearance: { custom: { [editMode.value]: custom } } })
}
function resetCustom() {
  void save({ appearance: { custom: { [editMode.value]: { accent: '', background: '', surface: '', text: '' } } } })
}
function pickPalette(id: string) {
  void save({ appearance: { paletteId: id, custom: { dark: { accent: '', background: '', surface: '', text: '' }, light: { accent: '', background: '', surface: '', text: '' } } } })
}
function swatch(c: Colors) {
  return { background: c.background, borderColor: c.surface }
}
</script>

<template>
  <template v-if="a">
    <SettingsPanel :title="t('settings.theme')">
      <SettingRow
        :title="t('settings.mode')"
        :text="t('settings.modeText')"
      >
        <SelectButton
          :model-value="a.mode"
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
      <SettingRow
        :title="t('settings.look.palette')"
        :text="t('settings.look.paletteText')"
        stack
      >
        <div
          class="palettes"
          role="radiogroup"
          :aria-label="t('settings.look.palette')"
        >
          <button
            v-for="p in palettes"
            :key="p.id"
            type="button"
            role="radio"
            class="pal"
            :class="{ active: a.paletteId === p.id }"
            :aria-checked="a.paletteId === p.id"
            @click="pickPalette(p.id)"
          >
            <span class="pal-sw">
              <span
                class="half"
                :style="swatch(p.dark)"
              ><i :style="{ background: p.dark.accent }" /><b :style="{ background: p.dark.text }" /></span>
              <span
                class="half"
                :style="swatch(p.light)"
              ><i :style="{ background: p.light.accent }" /><b :style="{ background: p.light.text }" /></span>
            </span>
            <span class="pal-name">{{ t('settings.look.palettes.' + p.id) }}</span>
          </button>
        </div>
      </SettingRow>
    </SettingsPanel>

    <SettingsPanel
      v-if="draft"
      :title="t('settings.look.custom', { mode: t('settings.' + editMode) })"
      :text="t('settings.look.customText')"
    >
      <div class="custom">
        <div class="fields">
          <label
            v-for="k in KEYS"
            :key="k"
            class="field"
          >
            <span class="field-name">{{ t('settings.look.colors.' + k) }}</span>
            <span class="field-input">
              <input
                type="color"
                :value="draft[k]"
                :aria-label="t('settings.look.colors.' + k)"
                @input="setColor(k, ($event.target as HTMLInputElement).value)"
              >
              <input
                class="hex mono"
                :value="draft[k]"
                maxlength="7"
                spellcheck="false"
                :aria-label="t('settings.look.colors.' + k) + ' (hex)'"
                @change="setColor(k, ($event.target as HTMLInputElement).value)"
              >
            </span>
          </label>
          <p
            class="contrast"
            :class="{ bad: !draftOk }"
          >
            <i :class="draftOk ? 'pi pi-check-circle' : 'pi pi-exclamation-triangle'" />
            {{ t('settings.look.contrast', { s: draftContrast.surface.toFixed(1), b: draftContrast.background.toFixed(1) }) }}
          </p>
          <div class="actions">
            <Button
              :label="t('common.save')"
              icon="pi pi-check"
              size="small"
              :disabled="!dirty || !draftOk"
              @click="saveCustom"
            />
            <Button
              :label="t('settings.look.reset')"
              icon="pi pi-replay"
              size="small"
              severity="secondary"
              outlined
              :disabled="!hasCustom && !dirty"
              @click="dirty && !hasCustom ? (draft = effectiveColors(a, editMode, palettes)) : resetCustom()"
            />
          </div>
        </div>
        <div
          class="preview"
          :style="previewStyle"
          :aria-label="t('settings.look.preview')"
        >
          <div class="pv-bar">
            <span class="pv-dot" /> IssueWatcher
          </div>
          <div class="pv-card">
            <div class="pv-title">
              {{ t('settings.look.sampleTitle') }}
            </div>
            <div class="pv-muted">
              octo/app#42 · {{ t('settings.look.sampleMeta') }}
            </div>
            <div class="pv-row">
              <span class="pv-pill">{{ t('item.open') }}</span>
              <span class="pv-btn">{{ t('settings.look.sampleButton') }}</span>
            </div>
          </div>
        </div>
      </div>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.look.text')">
      <SettingRow :title="t('settings.look.font')">
        <Select
          :model-value="a.fontFamily"
          :options="fonts"
          option-label="label"
          option-value="value"
          :aria-label="t('settings.look.font')"
          class="font-select"
          @update:model-value="(v: Appearance['fontFamily']) => save({ appearance: { fontFamily: v } })"
        >
          <template #option="{ option }">
            <span :style="{ fontFamily: FONT_STACKS[option.value as Appearance['fontFamily']] }">{{ option.label }}</span>
          </template>
        </Select>
      </SettingRow>
      <SettingRow
        :title="t('settings.look.scale')"
        :text="t('settings.look.scaleText')"
      >
        <Slider
          :model-value="Math.round(a.fontScale * 100)"
          :min="85"
          :max="130"
          :step="5"
          class="scale"
          :aria-label="t('settings.look.scale')"
          @update:model-value="(v: number | number[]) => save.later('fontScale', { appearance: { fontScale: (Array.isArray(v) ? v[0] : v) / 100 } }, 400)"
        />
        <span class="mono scale-value">{{ Math.round(a.fontScale * 100) }}%</span>
      </SettingRow>
      <SettingRow
        :title="t('settings.look.densityTitle')"
        :text="t('settings.look.densityText')"
      >
        <SelectButton
          :model-value="a.density"
          :options="densities"
          option-label="label"
          option-value="value"
          :allow-empty="false"
          :aria-label="t('settings.look.densityTitle')"
          @update:model-value="(v: Appearance['density']) => save({ appearance: { density: v } })"
        />
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
</template>

<style scoped>
.palettes {
  width: 100%;
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(132px, 1fr));
  gap: 10px;
}

.pal {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 8px;
  border: 1px solid var(--iw-border);
  border-radius: var(--iw-radius-sm);
  background: var(--iw-surface);
  color: var(--iw-text);
  font: inherit;
  cursor: pointer;
  text-align: left;
}

.pal:hover {
  border-color: var(--iw-border-strong);
}

.pal.active {
  border-color: var(--iw-primary);
  box-shadow: 0 0 0 2px var(--iw-primary-soft);
}

.pal-sw {
  display: flex;
  height: 44px;
  border-radius: 6px;
  overflow: hidden;
}

.half {
  flex: 1;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 5px;
  padding: 0 8px;
  border: 1px solid;
}

.half i {
  width: 60%;
  height: 7px;
  border-radius: 4px;
}

.half b {
  width: 80%;
  height: 4px;
  border-radius: 2px;
  opacity: 0.8;
}

.pal-name {
  font-size: calc(13px * var(--iw-fs, 1));
  font-weight: 500;
}

.custom {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 24px;
  padding-top: 8px;
}

.fields {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.field {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.field-input {
  display: flex;
  align-items: center;
  gap: 8px;
}

.field-input input[type='color'] {
  width: 34px;
  height: 30px;
  padding: 0;
  border: 1px solid var(--iw-border);
  border-radius: 6px;
  background: none;
  cursor: pointer;
}

.hex {
  width: 92px;
  padding: 5px 8px;
  border: 1px solid var(--iw-border);
  border-radius: 6px;
  background: var(--iw-bg);
  color: var(--iw-text);
  font-size: calc(13px * var(--iw-fs, 1));
}

.contrast {
  display: flex;
  align-items: center;
  gap: 6px;
  margin: 4px 0 0;
  font-size: calc(12.5px * var(--iw-fs, 1));
  color: var(--iw-success);
}

.contrast.bad {
  color: var(--iw-danger);
}

.actions {
  display: flex;
  gap: 8px;
}

.preview {
  display: flex;
  flex-direction: column;
  overflow: hidden;
  border: 1px solid var(--iw-border);
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  color: var(--iw-text);
}

.pv-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  border-bottom: 1px solid var(--iw-border);
  font-weight: 600;
}

.pv-dot {
  width: 10px;
  height: 10px;
  border-radius: 50%;
  background: var(--iw-primary);
}

.pv-card {
  margin: 14px;
  padding: 14px;
  border: 1px solid var(--iw-border);
  border-radius: var(--iw-radius-sm);
  background: var(--iw-surface);
}

.pv-title {
  font-weight: 600;
}

.pv-muted {
  margin-top: 2px;
  font-size: calc(12.5px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.pv-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 12px;
}

.pv-pill {
  padding: 2px 10px;
  border-radius: 999px;
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.pv-btn {
  padding: 5px 12px;
  border-radius: 6px;
  font-size: calc(12.5px * var(--iw-fs, 1));
  font-weight: 600;
  color: var(--iw-on-primary);
  background: var(--iw-primary);
}

.font-select {
  min-width: 200px;
}

.scale {
  width: 180px;
}

.scale-value {
  width: 44px;
  text-align: right;
  color: var(--iw-muted);
}

@media (width <= 767px) {
  .custom {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
