<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import InputNumber from 'primevue/inputnumber'
import InputText from 'primevue/inputtext'
import Select from 'primevue/select'
import SelectButton from 'primevue/selectbutton'
import Textarea from 'primevue/textarea'
import ToggleSwitch from 'primevue/toggleswitch'
import Message from 'primevue/message'
import { useConfirm } from 'primevue/useconfirm'
import { useI18n } from 'vue-i18n'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import AutomationPanel from './AutomationPanel.vue'
import { api } from '../../api/client'
import type { AgentCLI, AgentProfile, Agents, DetectedCLI, SettingsPatch } from '../../api/types'
import { projectKeyOptions } from '../../lib/projectKey'
import { useSave } from '../../lib/save'
import { useAppStore } from '../../stores/app'
import { useSettingsStore } from '../../stores/settings'

const { t } = useI18n()
const settings = useSettingsStore()
const app = useAppStore()
const confirm = useConfirm()
const save = useSave()
const ag = computed<Agents | undefined>(() => settings.doc?.settings.agents)

// --- text drafts: typed text stays local until a pause, then one merge patch;
// the server's value comes back when nothing is being typed (an emptied prompt
// returns as the default text).
function useDraft(source: () => string | undefined) {
  const text = ref(source() ?? '')
  let dirty = false
  let timer: number | undefined
  let flush: (() => void) | null = null
  watch(source, (v) => {
    if (!dirty) text.value = v ?? ''
  })
  function input(v: string, patch: (v: string) => SettingsPatch) {
    text.value = v
    dirty = true
    window.clearTimeout(timer)
    flush = () => {
      flush = null
      void save(patch(v)).then(() => {
        if (text.value === v) {
          dirty = false
          text.value = source() ?? ''
        }
      })
    }
    timer = window.setTimeout(() => flush?.(), 800)
  }
  /** Drops local edits (the source switched to another project / prompt). */
  function sync() {
    flushNow()
    dirty = false
    text.value = source() ?? ''
  }
  function flushNow() {
    window.clearTimeout(timer)
    flush?.()
  }
  onBeforeUnmount(flushNow)
  return { text, input, sync }
}

// --- profiles ----------------------------------------------------------------
type Draft = Omit<AgentProfile, 'args'> & { argsText: string }
const editing = ref<Draft | null>(null)
const editIndex = ref(-1) // -1 = new profile
const editId = ref<string>() // the edited profile's id when the dialog opened (undefined = new)
const idTouched = ref(false)
const detected = ref<DetectedCLI[] | null>(null)
const detecting = ref(false)
const formError = ref('')

const cliOptions = [
  { label: 'Claude Code', value: 'claude' },
  { label: 'Codex', value: 'codex' },
]

function slug(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 32)
}

function openProfile(i: number) {
  const p = i >= 0 ? ag.value?.profiles[i] : undefined
  editIndex.value = i
  editId.value = p?.id
  idTouched.value = i >= 0
  detected.value = null
  formError.value = ''
  editing.value = p
    ? { ...p, argsText: p.args.join('\n') }
    : { id: '', name: '', cli: 'claude', path: '', model: '', argsText: '', timeoutMinutes: 30, maxParallel: 1, maxBudgetUsd: 0 }
}

function onName(v: string | undefined) {
  if (!editing.value) return
  editing.value.name = v ?? ''
  if (!idTouched.value) editing.value.id = slug(v ?? '')
}

async function detect() {
  if (!editing.value) return
  detecting.value = true
  const r = await api.detectAgents()
  detecting.value = false
  if (!r.ok) {
    formError.value = r.error
    return
  }
  detected.value = r.data
  const d = r.data.find((x) => x.cli === editing.value?.cli)
  if (d?.path && editing.value) editing.value.path = d.path
}
const found = computed(() => detected.value?.find((d) => d.cli === editing.value?.cli))

const saving = ref(false)
async function saveProfile() {
  const e = editing.value
  const cur = ag.value
  if (!e || !cur) return
  const { argsText, ...rest } = e
  const p: AgentProfile = {
    ...rest,
    id: rest.id.trim(),
    name: rest.name.trim(),
    path: rest.path.trim(),
    model: rest.model.trim(),
    args: argsText.split('\n').map((a) => a.trim()).filter(Boolean),
    maxBudgetUsd: rest.cli === 'claude' ? rest.maxBudgetUsd || 0 : 0,
  }
  // A function of the latest saved list (by profile id): the store rebuilds it
  // on a 409 retry, so a stale array never overwrites a newer save. A profile
  // deleted meanwhile (another tab) is a conflict: never re-added by an edit.
  const oldId = editId.value
  saving.value = true
  const ok = await save((s) => {
    const next = s.agents.profiles.slice()
    if (oldId === undefined) {
      next.push(p)
    } else {
      const k = next.findIndex((x) => x.id === oldId)
      if (k < 0) throw new Error(t('settings.agents.profileGone'))
      next[k] = p
    }
    return { agents: { profiles: next } }
  })
  saving.value = false
  if (ok) editing.value = null
}

function removeProfile(i: number) {
  const cur = ag.value
  const p = cur?.profiles[i]
  if (!cur || !p) return
  confirm.require({
    header: t('settings.agents.deleteTitle'),
    message: t('settings.agents.deleteText', { name: p.name }),
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', text: true },
    acceptProps: { label: t('settings.agents.delete'), severity: 'danger' },
    accept: () => {
      // Roles pointing at the profile are cleared in the same change (the server refuses dangling roles).
      void save((s) => {
        const roles: Partial<Agents['roles']> = {}
        for (const k of ['coder', 'responder', 'verifier'] as const) if (s.agents.roles[k] === p.id) roles[k] = ''
        return { agents: { profiles: s.agents.profiles.filter((x) => x.id !== p.id), roles } }
      })
    },
  })
}

function rolesOf(id: string): string[] {
  const r = ag.value?.roles
  if (!r) return []
  return (['coder', 'responder', 'verifier'] as const).filter((k) => r[k] === id).map((k) => t('settings.agents.role.' + k))
}

// --- roles -------------------------------------------------------------------
const profileOptions = computed(() => (ag.value?.profiles ?? []).map((p) => ({ label: p.name, value: p.id })))
const verifierOptions = computed(() => [{ label: t('settings.agents.noVerifier'), value: '' }, ...profileOptions.value])

// --- prompts -----------------------------------------------------------------
type PromptKey = 'system' | 'fixDirect' | 'fix' | 'reply' | 'review' | 'label' | 'triage'
const promptTab = ref<PromptKey>('system')
const promptTabs = computed(() => (['system', 'fixDirect', 'fix', 'reply', 'review', 'label', 'triage'] as const).map((k) => ({ label: t('settings.agents.prompt.' + k), value: k })))
const prompt = useDraft(() => ag.value?.prompts[promptTab.value])
watch(promptTab, () => prompt.sync())
const promptBox = ref<{ $el: HTMLTextAreaElement } | null>(null)

// Prompt variables (fixed names, not translated; {diff} only in the review prompt, {labels} only in the label prompt;
// the triage prompt is about the project, not one issue: {issues} and {topN} instead of the issue's).
const VARS = ['repo', 'issue.number', 'issue.title', 'issue.url', 'issue.body', 'comments', 'localPath', 'branch', 'diff', 'labels', 'issues', 'topN'] as const
const TRIAGE_VARS: readonly string[] = ['repo', 'localPath', 'issues', 'topN']
const vars = computed(() =>
  VARS.filter((v) =>
    promptTab.value === 'triage'
      ? TRIAGE_VARS.includes(v)
      : (v !== 'diff' || promptTab.value === 'review') && (v !== 'labels' || promptTab.value === 'label') && v !== 'issues' && v !== 'topN',
  ).map((v) => ({
    name: '{' + v + '}',
    hint: t('settings.agents.vars.' + v.replace('.', '_')),
  })),
)

function patchPrompt(key: PromptKey) {
  return (v: string): SettingsPatch => ({ agents: { prompts: { [key]: v } } })
}
function insertVar(name: string) {
  const el = promptBox.value?.$el
  const s = prompt.text.value
  const at = el ? el.selectionStart : s.length
  const end = el ? el.selectionEnd : s.length
  prompt.input(s.slice(0, at) + name + s.slice(end), patchPrompt(promptTab.value))
  requestAnimationFrame(() => {
    el?.focus()
    el?.setSelectionRange(at + name.length, at + name.length)
  })
}
function resetPrompt() {
  const key = promptTab.value
  confirm.require({
    header: t('settings.agents.resetPromptTitle'),
    message: t('settings.agents.resetPromptText', { name: t('settings.agents.prompt.' + key) }),
    rejectProps: { label: t('common.cancel'), severity: 'secondary', text: true },
    acceptProps: { label: t('settings.agents.resetPrompt') },
    accept: () => {
      prompt.sync()
      void save({ agents: { prompts: { [key]: '' } } }) // "" → the server restores the default
    },
  })
}

// --- per project -------------------------------------------------------------
const projectName = ref('')
// Projects are keyed platform:id (internal/config ProjectKey); labels show the project name.
const projectOptions = computed(() =>
  projectKeyOptions(app.repos, Object.keys(ag.value?.projects ?? {})).map((o) => ({ ...o, label: o.label + (ag.value?.projects[o.value] ? ' •' : '') })),
)
const proj = computed(() => (projectName.value ? ag.value?.projects[projectName.value] : undefined))
const projPrompt = useDraft(() => proj.value?.prompt ?? '')
const projVerify = useDraft(() => proj.value?.verify ?? '')
const projTriagePrompt = useDraft(() => proj.value?.triagePrompt ?? '')
watch(projectName, () => {
  projPrompt.sync()
  projVerify.sync()
  projTriagePrompt.sync()
})
function patchProject(field: 'prompt' | 'verify' | 'triagePrompt') {
  const name = projectName.value
  return (v: string): SettingsPatch => ({ agents: { projects: { [name]: { [field]: v } } } })
}
// jobMcp: inherit (unset) / on / off; null in a merge patch removes the field.
const jobMcpOptions = computed(() => [
  {
    label: t('settings.automation.inherit', { value: t(ag.value?.jobMcp ? 'settings.automation.on' : 'settings.automation.off') }),
    value: 'inherit',
  },
  { label: t('settings.automation.on'), value: 'on' },
  { label: t('settings.automation.off'), value: 'off' },
])
const projJobMcp = computed(() => {
  const v = proj.value?.jobMcp
  return v === undefined || v === null ? 'inherit' : v ? 'on' : 'off'
})
function setProjJobMcp(v: string) {
  const value = v === 'inherit' ? null : v === 'on'
  void save({ agents: { projects: { [projectName.value]: { jobMcp: value } } } } as unknown as SettingsPatch)
}
// modPush (mod pages only, key not github:): inherit / on / off.
const isModPage = computed(() => !!projectName.value && !projectName.value.startsWith('github:'))
const modPushOptions = computed(() => [
  {
    label: t('settings.automation.inherit', { value: t(ag.value?.modPush ? 'settings.automation.on' : 'settings.automation.off') }),
    value: 'inherit',
  },
  { label: t('settings.automation.on'), value: 'on' },
  { label: t('settings.automation.off'), value: 'off' },
])
const projModPush = computed(() => {
  const v = proj.value?.modPush
  return v === undefined || v === null ? 'inherit' : v ? 'on' : 'off'
})
function setProjModPush(v: string) {
  const value = v === 'inherit' ? null : v === 'on'
  void save({ agents: { projects: { [projectName.value]: { modPush: value } } } } as unknown as SettingsPatch)
}
// triageTopN: empty = inherit (null removes the override).
function setProjTopN(v: number | null) {
  const name = projectName.value
  save.later('projTopN:' + name, { agents: { projects: { [name]: { triageTopN: v ?? null } } } } as unknown as SettingsPatch)
}
</script>

<template>
  <template v-if="ag">
    <SettingsPanel
      :title="t('settings.agents.profiles')"
      :text="t('settings.agents.profilesText')"
    >
      <template #actions>
        <Button
          :label="t('settings.agents.add')"
          icon="pi pi-plus"
          size="small"
          severity="secondary"
          @click="openProfile(-1)"
        />
      </template>
      <p
        v-if="!ag.profiles.length"
        class="muted"
      >
        {{ t('settings.agents.noProfiles') }}
      </p>
      <div
        v-for="(p, i) in ag.profiles"
        :key="p.id"
        class="profile"
      >
        <span
          class="cli-mark"
          :class="p.cli"
        >{{ p.cli === 'claude' ? 'C' : 'X' }}</span>
        <div class="p-main">
          <div class="p-line">
            <b>{{ p.name }}</b>
            <span class="mono muted small">{{ p.id }}</span>
            <span
              v-for="r in rolesOf(p.id)"
              :key="r"
              class="role-tag"
            >{{ r }}</span>
          </div>
          <div class="muted small p-meta">
            <span>{{ p.cli === 'claude' ? 'Claude Code' : 'Codex' }}</span>
            <span>{{ p.model || t('settings.agents.defaultModel') }}</span>
            <span class="mono">{{ p.path || t('settings.agents.onPath') }}</span>
            <span>{{ t('settings.agents.limits', { min: p.timeoutMinutes, n: p.maxParallel }) }}</span>
            <span v-if="p.cli === 'claude' && p.maxBudgetUsd">{{ t('settings.agents.budgetShort', { usd: p.maxBudgetUsd }) }}</span>
          </div>
        </div>
        <Button
          icon="pi pi-pencil"
          text
          rounded
          severity="secondary"
          :aria-label="t('settings.agents.edit')"
          @click="openProfile(i)"
        />
        <Button
          icon="pi pi-trash"
          text
          rounded
          severity="danger"
          :aria-label="t('settings.agents.delete')"
          @click="removeProfile(i)"
        />
      </div>
    </SettingsPanel>

    <SettingsPanel :title="t('settings.agents.rolesTitle')">
      <SettingRow
        v-for="k in (['coder', 'responder'] as const)"
        :key="k"
        :title="t('settings.agents.role.' + k)"
        :text="t('settings.agents.roleText.' + k)"
      >
        <Select
          :model-value="ag.roles[k]"
          :options="profileOptions"
          option-label="label"
          option-value="value"
          :placeholder="t('settings.agents.pickProfile')"
          :aria-label="t('settings.agents.role.' + k)"
          class="role-select"
          @update:model-value="(v: string) => save({ agents: { roles: { [k]: v } } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.agents.role.verifier')"
        :text="t('settings.agents.roleText.verifier')"
      >
        <Select
          :model-value="ag.roles.verifier"
          :options="verifierOptions"
          option-label="label"
          option-value="value"
          :aria-label="t('settings.agents.role.verifier')"
          class="role-select"
          @update:model-value="(v: string) => save({ agents: { roles: { verifier: v } } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.agents.maxParallel')"
        :text="t('settings.agents.maxParallelText')"
      >
        <InputNumber
          :model-value="ag.maxParallel"
          :min="1"
          :max="8"
          show-buttons
          input-class="num-input"
          :aria-label="t('settings.agents.maxParallel')"
          @update:model-value="(v: number | null) => v && save.later('agentsParallel', { agents: { maxParallel: v } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.agents.jobMcp')"
        :text="t('settings.agents.jobMcpText')"
      >
        <ToggleSwitch
          :model-value="ag.jobMcp"
          :aria-label="t('settings.agents.jobMcp')"
          @update:model-value="(v: boolean) => save({ agents: { jobMcp: v } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.agents.modPush')"
        :text="t('settings.agents.modPushText')"
      >
        <ToggleSwitch
          :model-value="ag.modPush"
          :aria-label="t('settings.agents.modPush')"
          @update:model-value="(v: boolean) => save({ agents: { modPush: v } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.agents.triageTopN')"
        :text="t('settings.agents.triageTopNText')"
      >
        <InputNumber
          :model-value="ag.triageTopN"
          :min="1"
          :max="20"
          show-buttons
          input-class="num-input"
          :aria-label="t('settings.agents.triageTopN')"
          @update:model-value="(v: number | null) => v && save.later('triageTopN', { agents: { triageTopN: v } })"
        />
      </SettingRow>
    </SettingsPanel>

    <SettingsPanel
      :title="t('settings.agents.promptsTitle')"
      :text="t('settings.agents.promptsText')"
    >
      <template #actions>
        <Button
          :label="t('settings.agents.resetPrompt')"
          icon="pi pi-undo"
          size="small"
          severity="secondary"
          text
          @click="resetPrompt"
        />
      </template>
      <SelectButton
        v-model="promptTab"
        :options="promptTabs"
        option-label="label"
        option-value="value"
        :allow-empty="false"
        :aria-label="t('settings.agents.promptsTitle')"
        class="tabs"
      />
      <p class="muted small hint">
        {{ t('settings.agents.promptHint.' + promptTab) }}
      </p>
      <Textarea
        ref="promptBox"
        :model-value="prompt.text.value"
        rows="12"
        auto-resize
        class="mono prompt"
        :aria-label="t('settings.agents.prompt.' + promptTab)"
        fluid
        @update:model-value="(v: string | undefined) => prompt.input(v ?? '', patchPrompt(promptTab))"
      />
      <div class="vars">
        <span class="muted small">{{ t('settings.agents.varsTitle') }}</span>
        <button
          v-for="v in vars"
          :key="v.name"
          v-tooltip.top="v.hint"
          type="button"
          class="var mono"
          @click="insertVar(v.name)"
        >
          {{ v.name }}
        </button>
      </div>
      <p class="muted small hint">
        {{ t('settings.agents.untrustedHint') }}
      </p>
    </SettingsPanel>

    <SettingsPanel
      :title="t('settings.agents.projectTitle')"
      :text="t('settings.agents.projectText')"
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
          :title="t('settings.agents.projectPrompt')"
          :text="t('settings.agents.projectPromptText')"
          stack
        >
          <Textarea
            :model-value="projPrompt.text.value"
            rows="5"
            auto-resize
            class="mono prompt"
            fluid
            :aria-label="t('settings.agents.projectPrompt')"
            @update:model-value="(v: string | undefined) => projPrompt.input(v ?? '', patchProject('prompt'))"
          />
        </SettingRow>
        <SettingRow
          :title="t('settings.agents.verify')"
          :text="t('settings.agents.verifyText')"
          stack
        >
          <InputText
            :model-value="projVerify.text.value"
            class="mono"
            fluid
            :placeholder="t('settings.agents.verifyPlaceholder')"
            :aria-label="t('settings.agents.verify')"
            @update:model-value="(v: string | undefined) => projVerify.input(v ?? '', patchProject('verify'))"
          />
        </SettingRow>
        <SettingRow
          :title="t('settings.agents.noAegis')"
          :text="t('settings.agents.noAegisText')"
        >
          <ToggleSwitch
            :model-value="proj?.noAegis ?? false"
            :aria-label="t('settings.agents.noAegis')"
            @update:model-value="(v: boolean) => save({ agents: { projects: { [projectName]: { noAegis: v } } } })"
          />
        </SettingRow>
        <SettingRow
          :title="t('settings.agents.jobMcp')"
          :text="t('settings.agents.projectJobMcpText')"
        >
          <Select
            :model-value="projJobMcp"
            :options="jobMcpOptions"
            option-label="label"
            option-value="value"
            :aria-label="t('settings.agents.jobMcp')"
            class="role-select"
            @update:model-value="setProjJobMcp"
          />
        </SettingRow>
        <SettingRow
          v-if="isModPage"
          :title="t('settings.agents.modPush')"
          :text="t('settings.agents.projectModPushText')"
        >
          <Select
            :model-value="projModPush"
            :options="modPushOptions"
            option-label="label"
            option-value="value"
            :aria-label="t('settings.agents.modPush')"
            class="role-select"
            @update:model-value="setProjModPush"
          />
        </SettingRow>
        <SettingRow
          :title="t('settings.agents.triageTopN')"
          :text="t('settings.agents.projectTriageTopNText', { n: ag.triageTopN })"
        >
          <InputNumber
            :model-value="proj?.triageTopN ?? null"
            :min="1"
            :max="20"
            show-buttons
            input-class="num-input"
            :placeholder="String(ag.triageTopN)"
            :aria-label="t('settings.agents.triageTopN')"
            @update:model-value="setProjTopN"
          />
        </SettingRow>
        <SettingRow
          :title="t('settings.agents.projectTriagePrompt')"
          :text="t('settings.agents.projectTriagePromptText')"
          stack
        >
          <Textarea
            :model-value="projTriagePrompt.text.value"
            rows="3"
            auto-resize
            class="mono prompt"
            fluid
            :aria-label="t('settings.agents.projectTriagePrompt')"
            @update:model-value="(v: string | undefined) => projTriagePrompt.input(v ?? '', patchProject('triagePrompt'))"
          />
        </SettingRow>
      </template>
    </SettingsPanel>

    <AutomationPanel />

    <Dialog
      :visible="!!editing"
      :header="editIndex >= 0 ? t('settings.agents.editTitle') : t('settings.agents.addTitle')"
      modal
      :style="{ width: '620px' }"
      :breakpoints="{ '680px': '94vw' }"
      @update:visible="(v: boolean) => !v && (editing = null)"
    >
      <form
        v-if="editing"
        class="form"
        @submit.prevent="saveProfile"
      >
        <label class="field">
          <span>{{ t('settings.fields.name') }}</span>
          <InputText
            :model-value="editing.name"
            maxlength="64"
            fluid
            autofocus
            @update:model-value="onName"
          />
        </label>
        <label class="field">
          <span>{{ t('settings.fields.id') }}</span>
          <InputText
            v-model="editing.id"
            class="mono"
            maxlength="32"
            :disabled="editIndex >= 0"
            fluid
            @input="idTouched = true"
          />
          <small class="muted">{{ editIndex >= 0 ? t('settings.agents.idFixed') : t('settings.agents.idText') }}</small>
        </label>
        <div class="field">
          <span>{{ t('settings.fields.cli') }}</span>
          <SelectButton
            v-model="editing.cli"
            :options="cliOptions"
            option-label="label"
            option-value="value"
            :allow-empty="false"
            @update:model-value="(v: AgentCLI) => v !== 'claude' && editing && (editing.maxBudgetUsd = 0)"
          />
        </div>
        <div class="field">
          <span>{{ t('settings.agents.path') }}</span>
          <div class="path-row">
            <InputText
              v-model="editing.path"
              class="mono"
              :placeholder="t('settings.agents.pathPlaceholder')"
              :aria-label="t('settings.agents.path')"
              fluid
            />
            <Button
              :label="t('settings.agents.detect')"
              icon="pi pi-search"
              severity="secondary"
              :loading="detecting"
              @click="detect"
            />
          </div>
          <small
            v-if="found"
            :class="found.path ? 'ok' : 'warn'"
          >{{ found.path ? t('settings.agents.found', { path: found.path, version: found.version || '?' }) : t('settings.agents.notFound', { cli: editing.cli }) }}</small>
        </div>
        <label class="field">
          <span>{{ t('settings.agents.model') }}</span>
          <InputText
            v-model="editing.model"
            class="mono"
            :placeholder="t('settings.agents.defaultModel')"
            fluid
          />
        </label>
        <label class="field">
          <span>{{ t('settings.agents.args') }}</span>
          <Textarea
            v-model="editing.argsText"
            rows="3"
            auto-resize
            class="mono"
            fluid
          />
          <small class="muted">{{ t('settings.agents.argsText') }}</small>
        </label>
        <div class="nums">
          <label class="field">
            <span>{{ t('settings.fields.timeoutMinutes') }}</span>
            <InputNumber
              v-model="editing.timeoutMinutes"
              :min="1"
              :max="480"
              :suffix="' ' + t('settings.unit.min')"
              show-buttons
              fluid
            />
          </label>
          <label class="field">
            <span>{{ t('settings.fields.maxParallel') }}</span>
            <InputNumber
              v-model="editing.maxParallel"
              :min="1"
              :max="8"
              show-buttons
              fluid
            />
          </label>
          <label
            v-if="editing.cli === 'claude'"
            class="field"
          >
            <span>{{ t('settings.fields.maxBudgetUsd') }}</span>
            <InputNumber
              v-model="editing.maxBudgetUsd"
              :min="0"
              :max="1000"
              :min-fraction-digits="0"
              :max-fraction-digits="2"
              prefix="$ "
              fluid
            />
            <small class="muted">{{ t('settings.agents.budgetText') }}</small>
          </label>
        </div>
        <Message
          v-if="formError"
          severity="error"
          size="small"
        >
          {{ formError }}
        </Message>
      </form>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          text
          @click="editing = null"
        />
        <Button
          :label="t('common.save')"
          icon="pi pi-check"
          :loading="saving"
          :disabled="!editing?.name.trim() || !editing?.id.trim()"
          @click="saveProfile"
        />
      </template>
    </Dialog>
  </template>
</template>

<style scoped>
.small {
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.profile {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 0;
  border-bottom: 1px solid var(--iw-border);
}

.profile:last-child {
  border-bottom: 0;
}

.cli-mark {
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  flex: none;
  border-radius: 9px;
  font-weight: 700;
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.cli-mark.codex {
  color: var(--iw-success);
  background: var(--iw-success-soft);
}

.p-main {
  flex: 1;
  min-width: 0;
}

.p-line {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.p-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 14px;
  margin-top: 2px;
}

.p-meta .mono {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 320px;
}

.role-tag {
  padding: 0 8px;
  border-radius: 999px;
  font-size: calc(11px * var(--iw-fs, 1));
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.role-select {
  width: 240px;
}

.tabs {
  margin: 6px 0 4px;
}

.hint {
  margin: 6px 0;
}

.prompt {
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.vars {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  margin-top: 8px;
}

.var {
  padding: 1px 8px;
  border-radius: 6px;
  border: 1px solid var(--iw-border);
  background: var(--iw-elevated);
  color: var(--iw-text);
  font-size: calc(12px * var(--iw-fs, 1));
  cursor: pointer;
}

.var:hover {
  border-color: var(--iw-primary);
}

.project-select {
  width: 100%;
  max-width: 420px;
  margin: 6px 0 8px;
}

.form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}

.field > span {
  font-weight: 500;
}

.path-row {
  display: flex;
  gap: 8px;
}

.nums {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 12px;
}

.ok {
  color: var(--iw-success);
  overflow-wrap: anywhere;
}

.warn {
  color: var(--iw-warn);
}

:deep(.num-input) {
  width: 70px;
}

@media (width <= 640px) {
  .nums {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
