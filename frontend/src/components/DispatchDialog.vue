<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import Select from 'primevue/select'
import SelectButton from 'primevue/selectbutton'
import { useI18n } from 'vue-i18n'
import type { JobFlow } from '../api/types'
import { useJobsStore } from '../stores/jobs'
import { FLOW_ICON, useDispatchToast } from '../lib/jobs'
import { useAppStore } from '../stores/app'
import FolderDialog from './FolderDialog.vue'

// «Отправить агенту»: flow + profile for the selected issues, then POST /api/jobs.
const props = defineProps<{ items: { id: number; repo: string; number: number; title: string }[] }>()
const visible = defineModel<boolean>('visible', { required: true })
const emit = defineEmits<{ done: [] }>()
const { t } = useI18n()
const jobs = useJobsStore()
const report = useDispatchToast()

const flow = ref<JobFlow>('fix')
const profile = ref('')
const sending = ref(false)

const flowOptions = computed(() => [
  { label: t('jobs.flow.fix'), value: 'fix', icon: FLOW_ICON.fix },
  { label: t('jobs.flow.reply'), value: 'reply', icon: FLOW_ICON.reply },
  { label: t('jobs.flow.label'), value: 'label', icon: FLOW_ICON.label },
])
// A fix runs in the project's mapped folder: items without one are skipped
// (the API refuses them); with none left the fix is disabled.
const app = useAppStore()
const unmapped = computed(() => {
  const byName = new Map(app.repos.map((r) => [r.name, r]))
  return props.items.filter((i) => byName.has(i.repo) && !byName.get(i.repo)?.localPath)
})
const unmappedProjects = computed(() => [...new Set(unmapped.value.map((i) => i.repo))].map((n) => app.repos.find((r) => r.name === n)).filter((r) => !!r))
const fixBlocked = computed(() => flow.value === 'fix' && props.items.length > 0 && unmapped.value.length === props.items.length)
const folderOpen = ref(false)

const profileOptions = computed(() => jobs.profiles.map((p) => ({ label: `${p.name} · ${p.cli}${p.model ? ' · ' + p.model : ''}`, value: p.id })))

// Default profile = the role of the flow (coder / responder).
watch([visible, flow], () => {
  if (visible.value) profile.value = jobs.roleProfile(flow.value)
}, { immediate: true })

async function submit() {
  if (sending.value || !props.items.length) return
  sending.value = true
  const r = await jobs.dispatch(props.items.map((i) => i.id), flow.value, profile.value)
  sending.value = false
  report(r)
  if (r.ok) {
    visible.value = false
    emit('done')
  }
}
</script>

<template>
  <Dialog
    v-model:visible="visible"
    :header="t('jobs.dispatchTitle')"
    modal
    dismissable-mask
    :style="{ width: '560px' }"
    :breakpoints="{ '640px': '94vw' }"
  >
    <div class="form">
      <div class="field">
        <span class="lbl">{{ t('jobs.flowLabel') }}</span>
        <SelectButton
          v-model="flow"
          :options="flowOptions"
          option-label="label"
          option-value="value"
          :allow-empty="false"
          :aria-label="t('jobs.flowLabel')"
        >
          <template #option="{ option }">
            <i :class="option.icon" /> {{ option.label }}
          </template>
        </SelectButton>
        <span class="hint">{{ t('jobs.' + flow + 'Hint') }}</span>
        <span
          v-if="flow === 'fix' && unmapped.length"
          class="hint warn"
        >
          <i class="pi pi-folder" /> {{ fixBlocked ? t('folder.needed') : t('folder.skipped', { n: unmapped.length }) }}
          <Button
            v-if="unmappedProjects.length === 1"
            :label="t('folder.link')"
            size="small"
            link
            class="folder-link"
            @click="folderOpen = true"
          />
        </span>
      </div>
      <div class="field">
        <span class="lbl">{{ t('jobs.profile') }}</span>
        <Select
          v-model="profile"
          :options="profileOptions"
          option-label="label"
          option-value="value"
          :placeholder="t('jobs.noProfile')"
          :aria-label="t('jobs.profile')"
          fluid
        />
        <span
          v-if="!jobs.profiles.length"
          class="hint warn"
        >{{ t('jobs.noProfilesHint') }} <RouterLink to="/settings/agents">{{ t('jobs.agentSettings') }}</RouterLink></span>
      </div>
      <div class="field">
        <span class="lbl">{{ t('jobs.itemsLabel', items.length) }}</span>
        <ul class="items">
          <li
            v-for="it in items"
            :key="it.id"
          >
            <span class="mono ref">{{ it.repo }}#{{ it.number }}</span>
            <span class="ttl">{{ it.title }}</span>
          </li>
        </ul>
      </div>
    </div>
    <template #footer>
      <Button
        :label="t('common.cancel')"
        severity="secondary"
        text
        @click="visible = false"
      />
      <Button
        :label="t('jobs.dispatch', items.length)"
        icon="pi pi-sparkles"
        :loading="sending"
        :disabled="!items.length || fixBlocked"
        :title="fixBlocked ? t('folder.needed') : undefined"
        @click="submit"
      />
    </template>
  </Dialog>
  <FolderDialog
    v-model:visible="folderOpen"
    :project="unmappedProjects[0] ?? null"
  />
</template>

<style scoped>
.form {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.lbl {
  font-weight: 500;
}

.hint {
  font-size: calc(12.5px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.hint.warn {
  color: var(--iw-warn);
}

.folder-link {
  padding: 0 2px;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.items {
  margin: 0;
  padding: 0;
  list-style: none;
  max-height: 240px;
  overflow-y: auto;
  border: 1px solid var(--iw-border);
  border-radius: var(--iw-radius-sm);
}

.items li {
  display: flex;
  gap: 10px;
  padding: 6px 10px;
  border-bottom: 1px solid var(--iw-border);
  font-size: calc(13px * var(--iw-fs, 1));
}

.items li:last-child {
  border-bottom: 0;
}

.ref {
  flex: none;
  color: var(--iw-muted);
}

.ttl {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
