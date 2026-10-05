<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import Button from 'primevue/button'
import Checkbox from 'primevue/checkbox'
import InputNumber from 'primevue/inputnumber'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import Select from 'primevue/select'
import Skeleton from 'primevue/skeleton'
import ToggleSwitch from 'primevue/toggleswitch'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import PlatformIcon from '../../components/PlatformIcon.vue'
import ReleaseDialog from '../../components/ReleaseDialog.vue'
import ReleasePlan from '../../components/ReleasePlan.vue'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import { api, settingsErrorText, type Result } from '../../api/client'
import type {
  BuildProfile,
  ChangelogProfile,
  PlanTarget,
  ProfileDoc,
  ProjectAutopilot,
  PublishFile,
  PublishProfile,
  CheckResult,
  SmokeProfile,
  TargetProfile,
  VersionProfile,
} from '../../api/types'
import { platformName } from '../../lib/platforms'
import { releaseError, targetBlock } from '../../lib/release'
import { useAppStore } from '../../stores/app'
import { useSettingsStore } from '../../stores/settings'

// Autopilot of a GitHub code project (docs/AUTOPILOT.md → UI, Phase 1):
// master switch, «GitHub релиз», «Загружать на <платформу>» per linked mod page
// (disabled with the reason), the daily release cap; the publish profile
// editor + «Проверить настройку» (dry run, nothing public); Phase 2: the smoke
// block (factorio save / ticks / install, command) and «Публиковать без
// смоук-теста»; Phase 3: fix / push / auto release / reply / close switches,
// the release timer and diff limit, the accepted-risk notice. Auto triage
// comes with phase 4 (shown disabled).
const { t, te } = useI18n()
const app = useAppStore()
const settings = useSettingsStore()
const toast = useToast()

const codeProjects = computed(() => app.repos.filter((r) => r.platform === 'github' && !r.linkedTo))
const projectId = ref(0)
const doc = ref<ProfileDoc | null>(null)
const loading = ref(false)
const loadError = ref('')

/** The editor's copy: every block present (inputs bind to them). */
type Draft = PublishProfile & { build: BuildProfile; version: VersionProfile; changelog: ChangelogProfile; smoke: SmokeProfile; targets: Record<string, TargetProfile> }
const draft = ref<Draft>(editable({}))
const base = ref('')
const saving = ref('')

async function load(quiet = false) {
  const id = projectId.value
  if (!id) return
  if (!quiet) loading.value = true
  const r = await api.publishProfile(id)
  loading.value = false
  if (id !== projectId.value) return
  if (!r.ok) {
    loadError.value = releaseError(r)
    if (!quiet) doc.value = null
    return
  }
  loadError.value = ''
  setDoc(r.data, !quiet || !dirty.value)
}
function setDoc(d: ProfileDoc, resetDraft = true) {
  doc.value = d
  if (resetDraft) {
    draft.value = editable(d.publishProfile ?? {})
    base.value = JSON.stringify(clean(draft.value))
  }
}
watch(projectId, () => {
  doc.value = null
  check.value = null
  void load()
})
// Another tab / MCP changed settings: refresh (the draft stays while it has edits).
watch(
  () => settings.doc?.revision,
  (rev) => {
    if (doc.value && rev !== undefined && rev !== doc.value.revision) void load(true)
  },
)

/** A profile with every editor block present. */
function editable(p: PublishProfile): Draft {
  const c = JSON.parse(JSON.stringify(p ?? {})) as PublishProfile
  return { ...c, build: c.build ?? {}, version: c.version ?? {}, changelog: c.changelog ?? {}, smoke: c.smoke ?? {}, targets: c.targets ?? {} }
}
/** Drops empty fields (an empty target stays: it marks the target as configured). */
function clean(p: PublishProfile): PublishProfile {
  const strip = <T extends object>(o: T | undefined): T | undefined => {
    if (!o) return undefined
    const r = Object.fromEntries(
      Object.entries(o).filter(([, v]) => v !== '' && v !== undefined && v !== null && v !== false && v !== 0 && !(Array.isArray(v) && !v.length)),
    )
    return Object.keys(r).length ? (r as T) : undefined
  }
  const targets = Object.fromEntries(Object.entries(p.targets ?? {}).map(([k, v]) => [k, strip(v) ?? {}]))
  return {
    build: strip(p.build),
    steamContent: p.steamContent || undefined,
    version: strip(p.version),
    changelog: strip(p.changelog),
    smoke: strip(p.smoke),
    targets: Object.keys(targets).length ? targets : undefined,
  }
}
const dirty = computed(() => !!doc.value && JSON.stringify(clean(draft.value)) !== base.value)

/** A rejected PUT: validation reason, stale revision (reloaded), or agent_caller. */
function saveFailed(r: Extract<Result<unknown>, { ok: false }>) {
  const detail = r.status === 400 ? settingsErrorText(r.body, r.error) : r.status === 409 ? t('release.profile.conflict') : releaseError(r)
  toast.add({ severity: 'error', summary: t('settings.saveFailed'), detail, life: 8000 })
  if (r.status === 409) void load(true)
}

// --- autopilot switches (saved at once)
const ap = computed(() => doc.value?.autopilot ?? null)
async function saveAutopilot(patch: Partial<ProjectAutopilot>) {
  const d = doc.value
  if (!d || saving.value) return
  saving.value = 'autopilot'
  const r = await api.savePublishProfile(d.projectId, { revision: d.revision, autopilot: { ...d.autopilot, ...patch } })
  saving.value = ''
  if (!r.ok) {
    saveFailed(r)
    return
  }
  setDoc(r.data, !dirty.value)
}
function setPublish(key: string, on: boolean) {
  const cur = { ...(ap.value?.publish ?? {}) }
  if (on) cur[key] = true
  else delete cur[key]
  void saveAutopilot({ publish: cur })
}
/** Numeric autopilot fields: saved 700 ms after the last change, within their range. */
type NumKey = 'maxReleasesPerDay' | 'coalesceMinutes' | 'maxBatchAgeHours' | 'maxDiffLines'
const numTimers: Partial<Record<NumKey, number>> = {}
function setNum(key: NumKey, v: number | null, min: number, max: number) {
  window.clearTimeout(numTimers[key])
  if (!v || v < min || v > max) return
  numTimers[key] = window.setTimeout(() => void saveAutopilot({ [key]: v }), 700)
}
const setCap = (v: number | null) => setNum('maxReleasesPerDay', v, 1, 50)
/** Phase 3 numbers: fix push limit and the auto release timer. */
const NUMS: { key: NumKey; min: number; max: number }[] = [
  { key: 'coalesceMinutes', min: 1, max: 1440 },
  { key: 'maxBatchAgeHours', min: 1, max: 168 },
  { key: 'maxDiffLines', min: 1, max: 100000 },
]
/** Phase 3 switches (fix → push → release → reply → close). */
type SwitchKey = 'autoFix' | 'autoPush' | 'autoRelease' | 'autoReply' | 'autoClose'
const SWITCHES: SwitchKey[] = ['autoFix', 'autoPush', 'autoRelease', 'autoReply', 'autoClose']
function switchText(key: SwitchKey): string {
  const a = ap.value
  return t('release.autopilot.' + key + 'Text', { coalesce: a?.coalesceMinutes ?? 0, age: a?.maxBatchAgeHours ?? 0 })
}
/** Later phases: shown, not switchable yet. */
const LATER: { key: keyof ProjectAutopilot; phase: number }[] = [{ key: 'autoTriage', phase: 4 }]
/**
 * Accepted risk (docs/AUTOPILOT.md → Safety rails → Agent isolation): the fixing
 * agent runs as the owner while a publish target is on.
 */
const showRisk = computed(() => {
  const a = ap.value
  return !!a?.autoFix && (a.githubRelease || Object.values(a.publish ?? {}).some(Boolean))
})

const targets = computed<PlanTarget[]>(() => doc.value?.resolved?.targets ?? [])
/** «Загружать на …» can be switched on: uploader, key, profile entry. */
function publishBlock(tg: PlanTarget): string {
  return targetBlock(tg)
}

// --- global kill switch
async function setPaused(paused: boolean) {
  saving.value = 'pause'
  const r = await api.pauseAutopilot(paused)
  saving.value = ''
  if (!r.ok) {
    toast.add({ severity: 'error', summary: t('settings.saveFailed'), detail: releaseError(r), life: 8000 })
    return
  }
  void load(true)
}

// --- publish profile editor
const kinds = computed(() => doc.value?.kinds ?? { version: [], changelog: [], smoke: [] })
/** factorio-info → release.profile.versionKind.factorioInfo (unknown kinds as is). */
function kindLabel(group: string, k: string): string {
  const key = 'release.profile.' + group + 'Kind.' + k.replace(/-([a-z])/g, (_, c: string) => c.toUpperCase())
  return te(key) ? t(key) : k
}
const kindOptions = (list: string[], group: string) => list.map((k) => ({ label: kindLabel(group, k), value: k }))
const buildMode = computed(() => (draft.value.build?.path ? 'path' : 'command'))
const buildModes = computed(() => [
  { label: t('release.profile.buildCommandMode'), value: 'command' },
  { label: t('release.profile.buildPathMode'), value: 'path' },
])
const pathModeRef = ref(false)
watch(doc, () => (pathModeRef.value = !!draft.value.build?.path))
const showPath = computed(() => pathModeRef.value || buildMode.value === 'path')
function setBuildMode(m: string) {
  pathModeRef.value = m === 'path'
  draft.value.build = m === 'path' ? { path: draft.value.build?.path ?? '' } : { command: draft.value.build?.command ?? '', output: draft.value.build?.output ?? '' }
}
const CATEGORIES = ['main', 'optional', 'miscellaneous']
const categoryOptions = computed(() => CATEGORIES.map((c) => ({ label: t('publish.category.' + c), value: c })))

function targetOf(key: string): TargetProfile | undefined {
  return draft.value.targets?.[key]
}
function setIncluded(tg: PlanTarget, on: boolean) {
  const next = { ...(draft.value.targets ?? {}) }
  if (on) next[tg.key] = next[tg.key] ?? (tg.platform === 'nexus' ? { category: 'main', archivePrevious: true } : {})
  else delete next[tg.key]
  draft.value.targets = next
  if (on && tg.platform === 'nexus') void loadFiles(tg)
}
// Nexus: the file groups of the page (the version is added to one of them).
const files = ref<Record<number, PublishFile[] | 'loading' | 'error'>>({})
async function loadFiles(tg: PlanTarget) {
  if (files.value[tg.projectId] && files.value[tg.projectId] !== 'error') return
  files.value = { ...files.value, [tg.projectId]: 'loading' }
  const r = await api.publishTargets(tg.projectId)
  files.value = { ...files.value, [tg.projectId]: r.ok ? (r.data.files ?? []).filter((f) => f.active !== false) : 'error' }
}
watch(targets, (l) => {
  for (const tg of l) if (tg.platform === 'nexus' && targetOf(tg.key)) void loadFiles(tg)
})
function fileOptions(tg: PlanTarget) {
  const f = files.value[tg.projectId]
  return Array.isArray(f) ? f.map((x) => ({ label: `${x.name} (${x.versionsCount})`, value: x.id })) : []
}

async function saveProfile() {
  const d = doc.value
  if (!d || saving.value) return
  saving.value = 'profile'
  const r = await api.savePublishProfile(d.projectId, { revision: d.revision, publishProfile: clean(draft.value) })
  saving.value = ''
  if (!r.ok) {
    saveFailed(r)
    return
  }
  setDoc(r.data)
  toast.add({ severity: 'success', summary: t('release.profile.saved'), life: 2500 })
}
function revert() {
  if (doc.value) setDoc(doc.value)
}

// «Проверить настройку»: the saved profile's full dry run.
const check = ref<CheckResult | null>(null)
const checking = ref(false)
async function runCheck() {
  const d = doc.value
  if (!d) return
  checking.value = true
  const r = await api.checkPublishProfile(d.projectId)
  checking.value = false
  if (!r.ok) {
    toast.add({ severity: 'error', summary: t('release.profile.checkFailed'), detail: releaseError(r), life: 8000 })
    return
  }
  check.value = r.data
}

// «Выпустить релиз» from here.
const releaseOpen = ref(false)
const releaseProject = computed(() => {
  const r = codeProjects.value.find((x) => x.id === projectId.value)
  return r ? { id: r.id, name: r.name } : null
})
const name = (k: string) => platformName(k)
</script>

<template>
  <SettingsPanel
    :title="t('release.autopilot.title')"
    :text="t('release.autopilot.text')"
  >
    <Select
      v-model="projectId"
      :options="codeProjects"
      option-label="name"
      option-value="id"
      filter
      :placeholder="t('settings.agents.pickProject')"
      :aria-label="t('settings.agents.pickProject')"
      class="project-select"
    />
    <template v-if="projectId">
      <Skeleton
        v-if="loading"
        height="120px"
      />
      <Message
        v-else-if="loadError && !doc"
        severity="error"
        :closable="false"
      >
        {{ loadError }}
      </Message>
      <template v-else-if="doc && ap">
        <SettingRow
          :title="t('release.autopilot.paused')"
          :text="t('release.autopilot.pausedText')"
        >
          <ToggleSwitch
            :model-value="doc.global.paused"
            :disabled="!!saving"
            :aria-label="t('release.autopilot.paused')"
            @update:model-value="(v: boolean) => setPaused(v)"
          />
        </SettingRow>
        <SettingRow
          :title="t('release.autopilot.enabled')"
          :text="t('release.autopilot.enabledText')"
        >
          <ToggleSwitch
            :model-value="ap.enabled"
            :disabled="!!saving"
            :aria-label="t('release.autopilot.enabled')"
            @update:model-value="(v: boolean) => saveAutopilot({ enabled: v })"
          />
        </SettingRow>
        <SettingRow
          :title="t('release.autopilot.githubRelease')"
          :text="t('release.autopilot.githubReleaseText')"
        >
          <ToggleSwitch
            :model-value="ap.githubRelease"
            :disabled="!!saving"
            :aria-label="t('release.autopilot.githubRelease')"
            @update:model-value="(v: boolean) => saveAutopilot({ githubRelease: v })"
          />
        </SettingRow>
        <SettingRow
          :title="t('release.autopilot.publish')"
          :text="targets.length ? t('release.autopilot.publishText') : t('release.target.none')"
          stack
        >
          <div
            v-for="tg in targets"
            :key="tg.key"
            class="pub"
          >
            <Checkbox
              :model-value="!!ap.publish?.[tg.key]"
              binary
              :input-id="'pub-' + tg.key"
              :disabled="!!saving || (!ap.publish?.[tg.key] && !!publishBlock(tg))"
              @update:model-value="(v: boolean) => setPublish(tg.key, v)"
            />
            <PlatformIcon
              :platform="tg.platform"
              :size="14"
            />
            <label
              :for="'pub-' + tg.key"
              class="pub-main"
            >
              <span>{{ t('release.autopilot.publishTo', { platform: name(tg.platform) }) }} <span class="muted">«{{ tg.name }}»</span></span>
              <span
                v-if="publishBlock(tg)"
                class="reason"
              >{{ publishBlock(tg) }}</span>
            </label>
          </div>
          <RouterLink
            v-if="!targets.length"
            to="/projects"
            class="small"
          >
            {{ t('release.autopilot.linkPages') }}
          </RouterLink>
        </SettingRow>
        <SettingRow
          :title="t('release.autopilot.maxReleases')"
          :text="t('release.autopilot.maxReleasesText', { global: doc.global.maxReleasesPerDay })"
        >
          <InputNumber
            :model-value="ap.maxReleasesPerDay"
            :min="1"
            :max="50"
            show-buttons
            input-class="num-input"
            :aria-label="t('release.autopilot.maxReleases')"
            @update:model-value="setCap"
          />
        </SettingRow>
        <SettingRow
          :title="t('release.autopilot.publishWithoutSmoke')"
          :text="t('release.autopilot.publishWithoutSmokeText')"
        >
          <ToggleSwitch
            :model-value="ap.publishWithoutSmoke"
            :disabled="!!saving"
            :aria-label="t('release.autopilot.publishWithoutSmoke')"
            @update:model-value="(v: boolean) => saveAutopilot({ publishWithoutSmoke: v })"
          />
        </SettingRow>
        <SettingRow
          v-for="k in SWITCHES"
          :key="k"
          :title="t('release.autopilot.' + k)"
          :text="switchText(k)"
        >
          <ToggleSwitch
            :model-value="!!ap[k]"
            :disabled="!!saving"
            :aria-label="t('release.autopilot.' + k)"
            @update:model-value="(v: boolean) => saveAutopilot({ [k]: v })"
          />
        </SettingRow>
        <Message
          v-if="showRisk"
          severity="warn"
          :closable="false"
          class="risk"
        >
          <b>{{ t('release.autopilot.riskTitle') }}</b> {{ t('release.autopilot.riskText') }}
        </Message>
        <SettingRow
          v-for="n in NUMS"
          :key="n.key"
          :title="t('release.autopilot.' + n.key)"
          :text="t('release.autopilot.' + n.key + 'Text')"
        >
          <InputNumber
            :model-value="ap[n.key]"
            :min="n.min"
            :max="n.max"
            show-buttons
            input-class="num-input"
            :aria-label="t('release.autopilot.' + n.key)"
            @update:model-value="(v: number | null) => setNum(n.key, v, n.min, n.max)"
          />
        </SettingRow>
        <SettingRow
          v-for="l in LATER"
          :key="l.key"
          :title="t('release.autopilot.' + l.key)"
          :text="t('release.autopilot.phase', { n: l.phase })"
        >
          <ToggleSwitch
            :model-value="!!ap[l.key]"
            disabled
            :aria-label="t('release.autopilot.' + l.key)"
          />
        </SettingRow>
      </template>
    </template>
    <template #actions>
      <Button
        v-if="doc"
        :label="t('release.action')"
        icon="pi pi-send"
        size="small"
        severity="secondary"
        @click="releaseOpen = true"
      />
    </template>
    <ReleaseDialog
      v-model:visible="releaseOpen"
      :project="releaseProject"
    />
  </SettingsPanel>

  <SettingsPanel
    v-if="doc"
    :title="t('release.profile.title', { name: doc.project })"
    :text="t('release.profile.text')"
  >
    <template #actions>
      <Button
        :label="t('release.profile.check')"
        icon="pi pi-search"
        size="small"
        severity="secondary"
        :loading="checking"
        :disabled="dirty"
        @click="runCheck"
      />
    </template>

    <!-- build -->
    <SettingRow
      :title="t('release.profile.build')"
      :text="t('release.profile.buildText')"
      stack
    >
      <Select
        :model-value="showPath ? 'path' : 'command'"
        :options="buildModes"
        option-label="label"
        option-value="value"
        size="small"
        class="mode"
        :aria-label="t('release.profile.build')"
        @update:model-value="setBuildMode"
      />
      <template v-if="!showPath">
        <InputText
          v-model="draft.build.command"
          class="mono wide"
          size="small"
          placeholder="pwsh -File build.ps1"
          :aria-label="t('release.profile.buildCommand')"
        />
        <InputText
          v-model="draft.build.output"
          class="mono wide"
          size="small"
          placeholder="dist/{name}_{version}.zip"
          :aria-label="t('release.profile.buildOutput')"
        />
      </template>
      <InputText
        v-else
        v-model="draft.build.path"
        class="mono wide"
        size="small"
        placeholder="dist/my-mod.zip"
        :aria-label="t('release.profile.buildPath')"
      />
    </SettingRow>

    <!-- version -->
    <SettingRow
      :title="t('release.profile.version')"
      :text="t('release.profile.versionText')"
      stack
    >
      <Select
        v-model="draft.version.kind"
        :options="kindOptions(kinds.version, 'version')"
        option-label="label"
        option-value="value"
        size="small"
        class="mode"
        :placeholder="t('release.profile.pick')"
        :aria-label="t('release.profile.version')"
      />
      <InputText
        v-if="draft.version.kind && draft.version.kind !== 'git-tag'"
        v-model="draft.version.path"
        class="mono wide"
        size="small"
        :placeholder="draft.version.kind === 'factorio-info' ? 'info.json' : t('release.profile.filePath')"
        :aria-label="t('release.profile.filePath')"
      />
      <InputText
        v-if="draft.version.kind === 'json'"
        v-model="draft.version.key"
        class="mono wide"
        size="small"
        placeholder="version"
        :aria-label="t('release.profile.jsonKey')"
      />
      <InputText
        v-if="draft.version.kind === 'regex'"
        v-model="draft.version.pattern"
        class="mono wide"
        size="small"
        placeholder="Version = &quot;(\d+\.\d+\.\d+)&quot;"
        :aria-label="t('release.profile.pattern')"
      />
    </SettingRow>

    <!-- changelog -->
    <SettingRow
      :title="t('release.profile.changelog')"
      :text="t('release.profile.changelogText')"
      stack
    >
      <Select
        v-model="draft.changelog.kind"
        :options="kindOptions(kinds.changelog, 'changelog')"
        option-label="label"
        option-value="value"
        size="small"
        class="mode"
        :placeholder="t('release.profile.pick')"
        :aria-label="t('release.profile.changelog')"
      />
      <InputText
        v-if="draft.changelog.kind && draft.changelog.kind !== 'commits'"
        v-model="draft.changelog.path"
        class="mono wide"
        size="small"
        :placeholder="draft.changelog.kind === 'factorio' ? 'changelog.txt' : 'CHANGELOG.md'"
        :aria-label="t('release.profile.filePath')"
      />
    </SettingRow>

    <!-- smoke: factorio (save / ticks / install) | command | none (held unless «Публиковать без смоук-теста») -->
    <SettingRow
      :title="t('release.profile.smoke')"
      :text="t('release.profile.smokeText')"
      stack
    >
      <Select
        v-model="draft.smoke.kind"
        :options="kindOptions(kinds.smoke, 'smoke')"
        option-label="label"
        option-value="value"
        size="small"
        class="mode"
        :placeholder="t('release.profile.pick')"
        :aria-label="t('release.profile.smoke')"
      />
      <template v-if="draft.smoke.kind === 'factorio'">
        <label
          class="field"
          for="smoke-save"
        >{{ t('release.profile.smokeSave') }} <span class="muted">· {{ t('release.profile.smokeSaveHint') }}</span></label>
        <InputText
          id="smoke-save"
          v-model="draft.smoke.save"
          class="mono wide"
          size="small"
          placeholder="E:/Saves/test.zip"
        />
        <label
          class="field"
          for="smoke-ticks"
        >{{ t('release.profile.smokeTicks') }} <span class="muted">· {{ t('release.profile.smokeTicksHint') }}</span></label>
        <InputNumber
          v-model="draft.smoke.ticks"
          input-id="smoke-ticks"
          :min="1"
          :max="100000"
          size="small"
          placeholder="600"
          input-class="num-input"
        />
        <label
          class="field"
          for="smoke-install"
        >{{ t('release.profile.smokeInstall') }} <span class="muted">· {{ t('release.profile.smokeInstallHint') }}</span></label>
        <InputText
          id="smoke-install"
          v-model="draft.smoke.install"
          class="mono wide"
          size="small"
          placeholder="C:/Program Files (x86)/Steam/steamapps/common/Factorio"
        />
      </template>
      <template v-else-if="draft.smoke.kind === 'command'">
        <label
          class="field"
          for="smoke-command"
        >{{ t('release.profile.smokeCommand') }} <span class="muted">· {{ t('release.profile.smokeCommandHint') }}</span></label>
        <InputText
          id="smoke-command"
          v-model="draft.smoke.command"
          class="mono wide"
          size="small"
          :invalid="!draft.smoke.command?.trim()"
          placeholder="pwsh -File smoke.ps1"
        />
      </template>
      <span
        v-else
        class="small"
      >{{ ap?.publishWithoutSmoke ? t('release.profile.smokeNoneOn') : t('release.profile.smokeNoneHeld') }}</span>
    </SettingRow>

    <!-- targets -->
    <SettingRow
      :title="t('release.profile.targets')"
      :text="targets.length ? t('release.profile.targetsText') : t('release.target.none')"
      stack
    >
      <div
        v-for="tg in targets"
        :key="tg.key"
        class="tp"
      >
        <div class="pub">
          <Checkbox
            :model-value="!!targetOf(tg.key)"
            binary
            :input-id="'tp-' + tg.key"
            :disabled="!tg.publishable && !targetOf(tg.key)"
            @update:model-value="(v: boolean) => setIncluded(tg, v)"
          />
          <PlatformIcon
            :platform="tg.platform"
            :size="14"
          />
          <label
            :for="'tp-' + tg.key"
            class="pub-main"
          >
            <span>{{ name(tg.platform) }} <span class="muted">«{{ tg.name }}»</span></span>
            <span
              v-if="!tg.publishable"
              class="reason"
            >{{ t('release.target.notPublishable', { platform: name(tg.platform) }) }}</span>
          </label>
        </div>
        <div
          v-if="targetOf(tg.key) && tg.platform === 'nexus'"
          class="tp-fields"
        >
          <Select
            v-model="draft.targets[tg.key].fileId"
            :options="fileOptions(tg)"
            option-label="label"
            option-value="value"
            editable
            size="small"
            class="wide"
            :loading="files[tg.projectId] === 'loading'"
            :placeholder="t('release.profile.nexusFile')"
            :aria-label="t('release.profile.nexusFile')"
          />
          <Select
            v-model="draft.targets[tg.key].category"
            :options="categoryOptions"
            option-label="label"
            option-value="value"
            size="small"
            class="mode"
            :aria-label="t('release.profile.nexusCategory')"
          />
          <span class="pub">
            <Checkbox
              v-model="draft.targets[tg.key].archivePrevious"
              binary
              :input-id="'ap-' + tg.key"
            />
            <label :for="'ap-' + tg.key">{{ t('release.profile.archivePrevious') }}</label>
          </span>
        </div>
      </div>
    </SettingRow>

    <div class="foot">
      <Button
        :label="t('common.save')"
        icon="pi pi-check"
        size="small"
        :loading="saving === 'profile'"
        :disabled="!dirty || !!saving"
        @click="saveProfile"
      />
      <Button
        v-if="dirty"
        :label="t('release.profile.revert')"
        size="small"
        severity="secondary"
        text
        @click="revert"
      />
      <span
        v-if="dirty"
        class="muted small"
      >{{ t('release.profile.unsaved') }}</span>
    </div>

    <p
      v-if="checking"
      class="muted small"
    >
      <i class="pi pi-spin pi-spinner" /> {{ t('release.profile.checking') }}
    </p>
    <div
      v-if="check"
      class="check"
    >
      <div class="check-title">
        {{ t('release.profile.checkTitle') }}
      </div>
      <ReleasePlan :plan="check" />
    </div>
  </SettingsPanel>
</template>

<style scoped>
.project-select {
  width: 100%;
  max-width: 420px;
  margin: 6px 0 8px;
}

.pub {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 3px 0;
}

.pub > :deep(.p-checkbox),
.pub > :deep(svg) {
  flex: none;
  margin-top: 2px;
}

.pub-main {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.reason,
.small {
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.field {
  font-size: calc(12.5px * var(--iw-fs, 1));
  margin-top: 4px;
}

.mode {
  width: 260px;
  max-width: 100%;
}

.wide {
  width: 100%;
  max-width: 520px;
}

.tp {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 4px 0;
}

.tp-fields {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  padding-left: 28px;
}

.foot {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 0 4px;
}

.check {
  margin-top: 10px;
  padding-top: 12px;
  border-top: 1px solid var(--iw-border);
}

.check-title {
  font-weight: 600;
  margin-bottom: 8px;
}

:deep(.num-input) {
  width: 90px;
}

.risk {
  margin: 6px 0;
}
</style>
