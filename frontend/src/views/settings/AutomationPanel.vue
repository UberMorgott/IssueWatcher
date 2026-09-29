<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import InputNumber from 'primevue/inputnumber'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import Select from 'primevue/select'
import SelectButton from 'primevue/selectbutton'
import Tag from 'primevue/tag'
import ToggleSwitch from 'primevue/toggleswitch'
import { useConfirm } from 'primevue/useconfirm'
import { useI18n } from 'vue-i18n'
import SettingRow from '../../components/SettingRow.vue'
import SettingsPanel from '../../components/SettingsPanel.vue'
import { api } from '../../api/client'
import type { Automation, AutomationEntry, AutomationRule } from '../../api/types'
import { relTime } from '../../lib/format'
import { useSave } from '../../lib/save'
import { useAppStore } from '../../stores/app'
import { useSettingsStore } from '../../stores/settings'

// Settings › Агенты › Автоматизация: global defaults, ordered rules, decision log
// (docs/ARCHITECTURE.md → Automation). Per-project overrides live in Settings › Проекты и папки.
const { t } = useI18n()
const settings = useSettingsStore()
const app = useAppStore()
const confirm = useConfirm()
const save = useSave()
const au = computed<Automation | undefined>(() => settings.doc?.settings.agents.automation)
const profiles = computed(() => settings.doc?.settings.agents.profiles ?? [])

function saveRules(rules: AutomationRule[]) {
  return save({ agents: { automation: { rules } } })
}

// --- rules -------------------------------------------------------------------
type Draft = Omit<AutomationRule, 'labelsAny'> & { labelsText: string }
const editing = ref<Draft | null>(null)
const editIndex = ref(-1) // -1 = new rule
const savingRule = ref(false)

const eventOptions = computed(() => (['new_issue', 'new_comment'] as const).map((v) => ({ label: t('settings.automation.event.' + v), value: v })))
const flowOptions = computed(() => (['label', 'reply', 'fix'] as const).map((v) => ({ label: t('jobs.flow.' + v), value: v })))
const profileOptions = computed(() => [{ label: t('settings.automation.roleProfile'), value: '' }, ...profiles.value.map((p) => ({ label: p.name, value: p.id }))])
const projectOptions = computed(() => {
  const names = new Set(app.repos.map((r) => r.name))
  for (const r of au.value?.rules ?? []) names.add(r.project)
  return [...names].sort((a, b) => a.localeCompare(b)).map((n) => ({ label: n, value: n }))
})

function nextId(): string {
  const ids = new Set((au.value?.rules ?? []).map((r) => r.id))
  let n = ids.size + 1
  while (ids.has('rule-' + n)) n++
  return 'rule-' + n
}

function openRule(i: number) {
  const r = i >= 0 ? au.value?.rules[i] : undefined
  editIndex.value = i
  editing.value = r
    ? { ...r, labelsText: r.labelsAny.join(', ') }
    : { id: nextId(), enabled: true, project: projectOptions.value[0]?.value ?? '', event: 'new_issue', labelsText: '', flow: 'label', profileId: '', maxPerDay: 0 }
}

async function saveRule() {
  const e = editing.value
  const cur = au.value
  if (!e || !cur) return
  const { labelsText, ...rest } = e
  const rule: AutomationRule = {
    ...rest,
    id: rest.id.trim(),
    project: rest.project.trim(),
    labelsAny: [...new Set(labelsText.split(',').map((l) => l.trim()).filter(Boolean))],
    maxPerDay: rest.maxPerDay || 0,
  }
  const next = cur.rules.slice()
  if (editIndex.value >= 0) next[editIndex.value] = rule
  else next.push(rule)
  savingRule.value = true
  const ok = await saveRules(next)
  savingRule.value = false
  if (ok) editing.value = null
}

function toggleRule(i: number, enabled: boolean) {
  const cur = au.value
  if (cur) void saveRules(cur.rules.map((r, j) => (j === i ? { ...r, enabled } : r)))
}

function moveRule(i: number, d: -1 | 1) {
  const cur = au.value
  if (!cur || i + d < 0 || i + d >= cur.rules.length) return
  const next = cur.rules.slice()
  ;[next[i], next[i + d]] = [next[i + d]!, next[i]!]
  void saveRules(next)
}

function removeRule(i: number) {
  const cur = au.value
  const r = cur?.rules[i]
  if (!cur || !r) return
  confirm.require({
    header: t('settings.automation.deleteTitle'),
    message: t('settings.automation.deleteText', { id: r.id }),
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', text: true },
    acceptProps: { label: t('settings.agents.delete'), severity: 'danger' },
    accept: () => void saveRules(cur.rules.filter((_, j) => j !== i)),
  })
}

function profileName(id: string): string {
  if (!id) return t('settings.automation.roleProfile')
  return profiles.value.find((p) => p.id === id)?.name ?? id
}

// --- decision log ------------------------------------------------------------
const log = ref<AutomationEntry[]>([])
const logCursor = ref('')
const logMore = ref(false)
const logLoaded = ref(false)
const logLoading = ref(false)
async function loadLog(more = false) {
  logLoading.value = true
  const r = await api.automationLog(more ? logCursor.value : '', 50)
  logLoading.value = false
  logLoaded.value = true
  if (!r.ok) return
  log.value = more ? [...log.value, ...r.data.items] : r.data.items
  logCursor.value = r.data.nextCursor
  logMore.value = r.data.more
}
onMounted(() => void loadLog())
watch(() => app.dataVersion, () => void loadLog())

function reasonText(e: AutomationEntry): string {
  return e.decision === 'queued' ? t('settings.automation.queued') : t('settings.automation.reason.' + e.reason)
}
</script>

<template>
  <template v-if="au">
    <SettingsPanel
      :title="t('settings.automation.title')"
      :text="t('settings.automation.text')"
    >
      <SettingRow
        :title="t('settings.automation.enabled')"
        :text="t('settings.automation.enabledText')"
      >
        <ToggleSwitch
          :model-value="au.enabled"
          :aria-label="t('settings.automation.enabled')"
          @update:model-value="(v: boolean) => save({ agents: { automation: { enabled: v } } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.automation.maxPerDay')"
        :text="t('settings.automation.maxPerDayText')"
      >
        <InputNumber
          :model-value="au.maxPerDay"
          :min="1"
          :max="500"
          show-buttons
          input-class="num-input"
          :aria-label="t('settings.automation.maxPerDay')"
          @update:model-value="(v: number | null) => v && save.later('automation.maxPerDay', { agents: { automation: { maxPerDay: v } } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.automation.maxAttempts')"
        :text="t('settings.automation.maxAttemptsText')"
      >
        <InputNumber
          :model-value="au.maxAttempts"
          :min="1"
          :max="10"
          show-buttons
          input-class="num-input"
          :aria-label="t('settings.automation.maxAttempts')"
          @update:model-value="(v: number | null) => v && save.later('automation.maxAttempts', { agents: { automation: { maxAttempts: v } } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.automation.allowAutoFix')"
        :text="t('settings.automation.allowAutoFixText')"
      >
        <ToggleSwitch
          :model-value="au.allowAutoFix"
          :aria-label="t('settings.automation.allowAutoFix')"
          @update:model-value="(v: boolean) => save({ agents: { automation: { allowAutoFix: v } } })"
        />
      </SettingRow>
      <SettingRow
        :title="t('settings.automation.autoApplyLabels')"
        :text="t('settings.automation.autoApplyLabelsText')"
      >
        <ToggleSwitch
          :model-value="au.autoApplyLabels"
          :aria-label="t('settings.automation.autoApplyLabels')"
          @update:model-value="(v: boolean) => save({ agents: { automation: { autoApplyLabels: v } } })"
        />
      </SettingRow>
      <Message
        v-if="au.allowAutoFix"
        severity="warn"
        size="small"
        class="warn-msg"
      >
        {{ t('settings.automation.fixWarning') }}
      </Message>
    </SettingsPanel>

    <SettingsPanel
      :title="t('settings.automation.rules')"
      :text="t('settings.automation.rulesText')"
    >
      <template #actions>
        <Button
          :label="t('settings.automation.addRule')"
          icon="pi pi-plus"
          size="small"
          severity="secondary"
          :disabled="!profiles.length"
          @click="openRule(-1)"
        />
      </template>
      <p
        v-if="!au.rules.length"
        class="muted"
      >
        {{ t('settings.automation.noRules') }}
      </p>
      <div
        v-for="(r, i) in au.rules"
        :key="r.id"
        class="rule"
        :class="{ off: !r.enabled }"
      >
        <span class="order muted mono">{{ i + 1 }}</span>
        <ToggleSwitch
          :model-value="r.enabled"
          :aria-label="t('settings.automation.ruleEnabled', { id: r.id })"
          @update:model-value="(v: boolean) => toggleRule(i, v)"
        />
        <div class="r-main">
          <div class="r-line">
            <b class="mono">{{ r.id }}</b>
            <span>{{ r.project }}</span>
            <span class="muted">·</span>
            <span>{{ t('settings.automation.event.' + r.event) }}</span>
            <i class="pi pi-arrow-right muted small" />
            <span class="flow-tag">{{ t('jobs.flow.' + r.flow) }}</span>
            <i
              v-if="r.flow === 'fix'"
              v-tooltip.top="t('settings.automation.fixWarning')"
              class="pi pi-exclamation-triangle warn"
            />
          </div>
          <div class="muted small r-meta">
            <span>{{ r.labelsAny.length ? t('settings.automation.labelsAnyShort', { labels: r.labelsAny.join(', ') }) : t('settings.automation.anyItem') }}</span>
            <span>{{ profileName(r.profileId) }}</span>
            <span v-if="r.maxPerDay">{{ t('settings.automation.ruleCapShort', { n: r.maxPerDay }) }}</span>
          </div>
        </div>
        <Button
          icon="pi pi-arrow-up"
          text
          rounded
          severity="secondary"
          :disabled="i === 0"
          :aria-label="t('settings.automation.moveUp')"
          @click="moveRule(i, -1)"
        />
        <Button
          icon="pi pi-arrow-down"
          text
          rounded
          severity="secondary"
          :disabled="i === au.rules.length - 1"
          :aria-label="t('settings.automation.moveDown')"
          @click="moveRule(i, 1)"
        />
        <Button
          icon="pi pi-pencil"
          text
          rounded
          severity="secondary"
          :aria-label="t('settings.automation.editRule')"
          @click="openRule(i)"
        />
        <Button
          icon="pi pi-trash"
          text
          rounded
          severity="danger"
          :aria-label="t('settings.agents.delete')"
          @click="removeRule(i)"
        />
      </div>
    </SettingsPanel>

    <SettingsPanel
      :title="t('settings.automation.log')"
      :text="t('settings.automation.logText')"
    >
      <template #actions>
        <Button
          icon="pi pi-refresh"
          size="small"
          severity="secondary"
          text
          :loading="logLoading"
          :aria-label="t('settings.automation.refresh')"
          @click="loadLog()"
        />
      </template>
      <p
        v-if="logLoaded && !log.length"
        class="muted"
      >
        {{ t('settings.automation.logEmpty') }}
      </p>
      <div
        v-else
        class="log-wrap"
      >
        <table class="log">
          <thead>
            <tr>
              <th>{{ t('settings.automation.col.at') }}</th>
              <th>{{ t('settings.automation.col.item') }}</th>
              <th>{{ t('settings.automation.col.rule') }}</th>
              <th>{{ t('settings.automation.col.decision') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="e in log"
              :key="e.id"
            >
              <td
                v-tooltip.top="new Date(e.at).toLocaleString()"
                class="muted nowrap"
              >
                {{ relTime(e.at) }}
              </td>
              <td class="item">
                <router-link :to="'/item/' + e.itemId">
                  <span class="mono">{{ e.repo }}#{{ e.number }}</span>
                  <span class="title">{{ e.title }}</span>
                </router-link>
                <div class="muted small">
                  {{ t('settings.automation.event.' + e.event) }}
                </div>
              </td>
              <td class="nowrap">
                <span class="mono">{{ e.ruleId }}</span>
                <span class="muted"> → {{ t('jobs.flow.' + e.flow) }}</span>
              </td>
              <td>
                <Tag
                  :severity="e.decision === 'queued' ? 'success' : 'secondary'"
                  :value="reasonText(e)"
                />
                <router-link
                  v-if="e.jobId"
                  :to="'/jobs/' + e.jobId"
                  class="job-link small"
                >
                  {{ t('settings.automation.openJob') }}
                </router-link>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <Button
        v-if="logMore"
        :label="t('settings.automation.more')"
        size="small"
        severity="secondary"
        text
        :loading="logLoading"
        @click="loadLog(true)"
      />
    </SettingsPanel>

    <Dialog
      :visible="!!editing"
      :header="editIndex >= 0 ? t('settings.automation.editRule') : t('settings.automation.addRule')"
      modal
      :style="{ width: '600px' }"
      :breakpoints="{ '660px': '94vw' }"
      @update:visible="(v: boolean) => !v && (editing = null)"
    >
      <form
        v-if="editing"
        class="form"
        @submit.prevent="saveRule"
      >
        <label class="field">
          <span>{{ t('settings.automation.id') }}</span>
          <InputText
            v-model="editing.id"
            class="mono"
            maxlength="32"
            fluid
          />
          <small class="muted">{{ t('settings.agents.idText') }}</small>
        </label>
        <div class="field">
          <span>{{ t('settings.automation.project') }}</span>
          <Select
            v-model="editing.project"
            :options="projectOptions"
            option-label="label"
            option-value="value"
            filter
            editable
            :placeholder="t('settings.agents.pickProject')"
            fluid
          />
        </div>
        <div class="field">
          <span>{{ t('settings.automation.eventTitle') }}</span>
          <SelectButton
            v-model="editing.event"
            :options="eventOptions"
            option-label="label"
            option-value="value"
            :allow-empty="false"
          />
        </div>
        <label class="field">
          <span>{{ t('settings.automation.labelsAny') }}</span>
          <InputText
            v-model="editing.labelsText"
            :placeholder="t('settings.automation.labelsAnyPlaceholder')"
            fluid
          />
          <small class="muted">{{ t('settings.automation.labelsAnyText') }}</small>
        </label>
        <div class="field">
          <span>{{ t('settings.automation.flowTitle') }}</span>
          <SelectButton
            v-model="editing.flow"
            :options="flowOptions"
            option-label="label"
            option-value="value"
            :allow-empty="false"
          />
        </div>
        <Message
          v-if="editing.flow === 'fix'"
          severity="warn"
          size="small"
        >
          {{ t('settings.automation.fixWarning') }} {{ au.allowAutoFix ? '' : t('settings.automation.fixNeedsAllow') }}
        </Message>
        <div class="nums">
          <div class="field">
            <span>{{ t('settings.automation.profile') }}</span>
            <Select
              v-model="editing.profileId"
              :options="profileOptions"
              option-label="label"
              option-value="value"
              fluid
            />
          </div>
          <label class="field">
            <span>{{ t('settings.automation.ruleCap') }}</span>
            <InputNumber
              v-model="editing.maxPerDay"
              :min="0"
              :max="500"
              show-buttons
              fluid
            />
            <small class="muted">{{ t('settings.automation.ruleCapText') }}</small>
          </label>
        </div>
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
          :loading="savingRule"
          :disabled="!editing?.id.trim() || !editing?.project.trim()"
          @click="saveRule"
        />
      </template>
    </Dialog>
  </template>
</template>

<style scoped>
.small {
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.warn-msg {
  margin-top: 8px;
}

.warn {
  color: var(--iw-warn);
}

.rule {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 0;
  border-bottom: 1px solid var(--iw-border);
}

.rule:last-child {
  border-bottom: 0;
}

.rule.off .r-main {
  opacity: 0.55;
}

.order {
  width: 18px;
  text-align: right;
  flex: none;
}

.r-main {
  flex: 1;
  min-width: 0;
}

.r-line {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.r-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 14px;
  margin-top: 2px;
}

.flow-tag {
  padding: 0 8px;
  border-radius: 999px;
  font-size: calc(11.5px * var(--iw-fs, 1));
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.log-wrap {
  overflow-x: auto;
}

.log {
  width: 100%;
  border-collapse: collapse;
  font-size: calc(13px * var(--iw-fs, 1));
}

.log th {
  text-align: left;
  font-weight: 500;
  color: var(--iw-muted);
  padding: 6px 8px;
  border-bottom: 1px solid var(--iw-border);
}

.log td {
  padding: 6px 8px;
  border-bottom: 1px solid var(--iw-border);
  vertical-align: top;
}

.log .item a {
  display: flex;
  gap: 8px;
  color: var(--iw-text);
  text-decoration: none;
  min-width: 0;
}

.log .item a:hover .title {
  text-decoration: underline;
}

.log .title {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 320px;
}

.nowrap {
  white-space: nowrap;
}

.job-link {
  margin-left: 8px;
  color: var(--iw-primary);
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

.nums {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
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
