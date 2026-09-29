<script setup lang="ts">
import InputText from 'primevue/inputtext'
import InputNumber from 'primevue/inputnumber'
import Button from 'primevue/button'
import Tag from 'primevue/tag'
import Select from 'primevue/select'
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useToast } from 'primevue/usetoast'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import AutomationProjectPanel from './AutomationProjectPanel.vue'
import { useSettingsStore } from '../../stores/settings'
import { useAppStore } from '../../stores/app'
import { useSave } from '../../lib/save'
import { api } from '../../api/client'
import type { FolderRow, FolderSuggestion, RunMode } from '../../api/types'

const { t, te } = useI18n()
const settings = useSettingsStore()
const app = useAppStore()
const toast = useToast()
const save = useSave()
const p = computed(() => settings.doc?.settings.projects)

// --- roots
const newRoot = ref('')
async function addRoot() {
  const r = newRoot.value.trim()
  if (!r || !p.value) return
  if (await save({ projects: { roots: [...p.value.roots, r] } })) newRoot.value = ''
}
function removeRoot(i: number) {
  if (!p.value) return
  void save({ projects: { roots: p.value.roots.filter((_, j) => j !== i) } })
}
const newExclude = ref('')
async function addExclude() {
  const x = newExclude.value.trim()
  if (!x || !p.value || p.value.exclude.includes(x)) return
  if (await save({ projects: { exclude: [...p.value.exclude, x] } })) newExclude.value = ''
}
function removeExclude(x: string) {
  if (p.value) void save({ projects: { exclude: p.value.exclude.filter((e) => e !== x) } })
}

// --- mappings
const rows = ref<FolderRow[]>([])
const loaded = ref(false)
const edits = ref<Record<number, string>>({})
async function loadRows() {
  const r = await api.folders()
  if (r.ok) rows.value = r.data
  loaded.value = true
}
onMounted(loadRows)
watch(() => app.dataVersion, loadRows)

async function setPath(id: number, path: string) {
  const r = await api.setFolder(id, path.trim())
  if (!r.ok) {
    const code = (r.body as { code?: string } | undefined)?.code ?? ''
    const name = rows.value.find((x) => x.projectId === id)?.name ?? ''
    const detail = code && te('folder.error.' + code) ? t('folder.error.' + code, { name }) : r.error
    toast.add({ severity: 'error', summary: t('settings.folders.saveFailed'), detail, life: 6000 })
    return false
  }
  rows.value = rows.value.map((x) => (x.projectId === id ? r.data : x))
  delete edits.value[id]
  return true
}
const mapped = computed(() => rows.value.filter((r) => r.localPath).length)

// --- run mode of fix jobs per project (settings.agents.projects[key].mode, key = platform:id; "" = direct)
const modeOptions = computed(() => [
  { label: t('settings.folders.modeDirect'), value: 'direct' },
  { label: t('settings.folders.modeWorktree'), value: 'worktree-pr' },
])
const modeOf = (key: string): RunMode => settings.doc?.settings.agents.projects[key]?.mode || 'direct'
function setMode(key: string, mode: RunMode) {
  if (mode !== modeOf(key)) void save({ agents: { projects: { [key]: { mode } } } })
}

const STATUS: Record<FolderRow['status'], 'success' | 'warn' | 'danger' | 'secondary'> = {
  ok: 'success',
  none: 'secondary',
  missing: 'danger',
  notGit: 'warn',
  mismatch: 'warn',
}

// --- discovery
const scanning = ref(false)
const suggestions = ref<FolderSuggestion[] | null>(null)
const visited = ref(0)
async function discover() {
  scanning.value = true
  const r = await api.discoverFolders()
  scanning.value = false
  if (!r.ok) {
    toast.add({ severity: 'error', summary: t('settings.folders.scanFailed'), detail: r.error, life: 5000 })
    return
  }
  suggestions.value = r.data.suggestions
  visited.value = r.data.visited
}
async function accept(s: FolderSuggestion) {
  if (await setPath(s.projectId, s.path)) suggestions.value = suggestions.value?.filter((x) => x !== s) ?? null
}
async function acceptAll() {
  for (const s of [...(suggestions.value ?? [])]) await accept(s)
}
</script>

<template>
  <template v-if="p">
    <SettingsPanel
      :title="t('settings.folders.roots')"
      :text="t('settings.folders.rootsText')"
    >
      <div
        v-for="(r, i) in p.roots"
        :key="r"
        class="root"
      >
        <i class="pi pi-folder muted" />
        <span class="mono path">{{ r }}</span>
        <Button
          icon="pi pi-times"
          text
          rounded
          severity="secondary"
          size="small"
          :aria-label="t('settings.folders.removeRoot')"
          @click="removeRoot(i)"
        />
      </div>
      <form
        class="add"
        @submit.prevent="addRoot"
      >
        <InputText
          v-model="newRoot"
          :placeholder="t('settings.folders.rootPlaceholder')"
          :aria-label="t('settings.folders.addRoot')"
          class="grow mono"
        />
        <Button
          type="submit"
          :label="t('settings.folders.addRoot')"
          icon="pi pi-plus"
          severity="secondary"
          :disabled="!newRoot.trim()"
        />
      </form>
      <SettingRow
        :title="t('settings.folders.depth')"
        :text="t('settings.folders.depthText')"
      >
        <InputNumber
          :model-value="p.scanDepth"
          :min="1"
          :max="6"
          show-buttons
          input-class="num-input"
          :aria-label="t('settings.folders.depth')"
          @update:model-value="(v: number | null) => v && save.later('projects.depth', { projects: { scanDepth: v } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.folders.exclude')"
        :text="t('settings.folders.excludeText')"
        stack
      >
        <Tag
          v-for="x in p.exclude"
          :key="x"
          severity="secondary"
          class="chip"
        >
          <span class="mono">{{ x }}</span>
          <button
            type="button"
            class="chip-x"
            :aria-label="t('settings.folders.removeExclude', { name: x })"
            @click="removeExclude(x)"
          >
            <i class="pi pi-times" />
          </button>
        </Tag>
        <form
          class="add small"
          @submit.prevent="addExclude"
        >
          <InputText
            v-model="newExclude"
            size="small"
            :placeholder="t('settings.folders.excludePlaceholder')"
            :aria-label="t('settings.folders.exclude')"
          />
        </form>
      </SettingRow>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.folders.discover')">
      <template #actions>
        <Button
          :label="t('settings.folders.scan')"
          icon="pi pi-search"
          size="small"
          :loading="scanning"
          :disabled="!p.roots.length"
          @click="discover"
        />
      </template>
      <p
        v-if="!p.roots.length"
        class="hint"
      >
        {{ t('settings.folders.noRoots') }}
      </p>
      <template v-else-if="suggestions">
        <p class="hint">
          {{ t('settings.folders.found', { n: suggestions.length, dirs: visited }) }}
        </p>
        <div
          v-for="s in suggestions"
          :key="s.projectId"
          class="sugg"
        >
          <div class="sugg-main">
            <div class="sugg-name">
              {{ s.name }}
            </div>
            <div class="mono path muted">
              {{ s.path }}
            </div>
          </div>
          <Button
            :label="t('settings.folders.accept')"
            icon="pi pi-check"
            size="small"
            severity="secondary"
            @click="accept(s)"
          />
        </div>
        <Button
          v-if="suggestions.length > 1"
          :label="t('settings.folders.acceptAll', { n: suggestions.length })"
          icon="pi pi-check-square"
          size="small"
          class="accept-all"
          @click="acceptAll"
        />
      </template>
      <p
        v-else
        class="hint"
      >
        {{ t('settings.folders.discoverText') }}
      </p>
    </SettingsPanel>

    <SettingsPanel
      :title="t('settings.folders.mapping', { n: mapped, total: rows.length })"
      :text="t('settings.folders.modeHint')"
    >
      <p
        v-if="loaded && !rows.length"
        class="hint"
      >
        {{ t('settings.folders.noProjects') }}
      </p>
      <div
        v-for="r in rows"
        :key="r.projectId"
        class="map-row"
      >
        <div class="map-name">
          <span class="name">{{ r.name }}</span>
          <Tag
            :severity="STATUS[r.status]"
            :value="t('settings.folders.status.' + r.status)"
            class="status"
          />
        </div>
        <form
          class="add"
          @submit.prevent="setPath(r.projectId, edits[r.projectId] ?? r.localPath)"
        >
          <InputText
            :model-value="edits[r.projectId] ?? r.localPath"
            :placeholder="t('settings.folders.pathPlaceholder')"
            :aria-label="t('settings.folders.pathFor', { name: r.name })"
            class="grow mono"
            size="small"
            @update:model-value="(v) => (edits[r.projectId] = v ?? '')"
          />
          <Button
            v-if="edits[r.projectId] !== undefined && edits[r.projectId] !== r.localPath"
            type="submit"
            icon="pi pi-check"
            size="small"
            :aria-label="t('common.save')"
          />
          <Button
            v-else-if="r.localPath"
            icon="pi pi-times"
            size="small"
            text
            severity="secondary"
            :aria-label="t('settings.folders.unmap')"
            @click="setPath(r.projectId, '')"
          />
        </form>
        <Select
          :model-value="modeOf(r.key)"
          :options="modeOptions"
          option-label="label"
          option-value="value"
          size="small"
          :aria-label="t('settings.folders.modeFor', { name: r.name })"
          class="mode"
          @update:model-value="(v: RunMode) => setMode(r.key, v)"
        />
      </div>
    </SettingsPanel>

    <AutomationProjectPanel />
  </template>
</template>

<style scoped>
.hint {
  margin: 8px 0;
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.root {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 0;
}

.path {
  font-size: calc(12.5px * var(--iw-fs, 1));
  overflow-wrap: anywhere;
}

.root .path {
  flex: 1;
}

.add {
  display: flex;
  gap: 8px;
  padding: 8px 0;
}

.grow {
  flex: 1;
  min-width: 0;
}

.chip {
  gap: 4px;
}

.chip-x {
  border: 0;
  background: none;
  color: inherit;
  cursor: pointer;
  padding: 0 2px;
  font-size: calc(10px * var(--iw-fs, 1));
}

.sugg {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 0;
  border-bottom: 1px solid var(--iw-border);
}

.sugg-main {
  flex: 1;
  min-width: 0;
}

.sugg-name,
.name {
  font-weight: 500;
}

.accept-all {
  align-self: flex-start;
  margin-top: 12px;
}

.map-row {
  display: grid;
  grid-template-columns: minmax(200px, 1fr) minmax(0, 1.4fr) 190px;
  align-items: center;
  gap: 12px;
  padding: 4px 0;
  border-bottom: 1px solid var(--iw-border);
}

.map-row:last-child {
  border-bottom: 0;
}

.map-name {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.map-name .name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mode {
  width: 100%;
}

.status {
  flex: none;
  font-size: calc(11px * var(--iw-fs, 1));
}

:deep(.num-input) {
  width: 80px;
}

@media (width <= 767px) {
  .map-row {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
