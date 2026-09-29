<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import IconField from 'primevue/iconfield'
import InputIcon from 'primevue/inputicon'
import Checkbox from 'primevue/checkbox'
import RadioButton from 'primevue/radiobutton'
import Message from 'primevue/message'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import PlatformIcon from './PlatformIcon.vue'
import { api } from '../api/client'
import type { Repo } from '../api/types'
import { useAppStore } from '../stores/app'
import { isModPlatform, platformName } from '../lib/platforms'

// «Площадки»: link mod pages to the code project (GitHub repo) whose folder
// their fixes run in. From a code project: multi-select its mod pages
// (PUT /api/projects/{code}/links {mods}: exactly these). From a mod page:
// pick its one code project (added to that project's links).
const props = defineProps<{ project: Repo | null }>()
const visible = defineModel<boolean>('visible', { required: true })
const { t } = useI18n()
const toast = useToast()
const app = useAppStore()

const search = ref('')
const picked = ref<number[]>([])
const code = ref<number>(0)
const busy = ref(false)
const error = ref('')
const modMode = computed(() => isModPlatform(props.project?.platform ?? ''))
const byId = computed(() => new Map(app.repos.map((r) => [r.id, r])))

watch(visible, (v) => {
  if (!v || !props.project) return
  search.value = ''
  error.value = ''
  picked.value = [...(props.project.links ?? [])]
  code.value = props.project.linkedTo ?? 0
})

/** The candidates: mod pages (code mode) or GitHub projects (mod mode), filtered. */
const options = computed(() => {
  const q = search.value.trim().toLowerCase()
  return app.repos
    .filter((r) => (modMode.value ? r.platform === 'github' : isModPlatform(r.platform)))
    .filter((r) => !q || r.name.toLowerCase().includes(q) || platformName(r.platform).toLowerCase().includes(q))
    .sort((a, b) => a.platform.localeCompare(b.platform) || a.name.localeCompare(b.name))
})
const elsewhere = (r: Repo) => (r.linkedTo && r.linkedTo !== props.project?.id ? byId.value.get(r.linkedTo)?.name : undefined)

const changed = computed(() => {
  const p = props.project
  if (!p) return false
  if (modMode.value) return code.value !== (p.linkedTo ?? 0)
  const cur = [...(p.links ?? [])].sort().join(',')
  return [...picked.value].sort().join(',') !== cur
})

async function save() {
  const p = props.project
  if (!p || busy.value) return
  busy.value = true
  error.value = ''
  let r
  if (!modMode.value) r = await api.setLinks(p.id, picked.value)
  else if (code.value) {
    const target = byId.value.get(code.value)
    r = await api.setLinks(code.value, [...new Set([...(target?.links ?? []), p.id])])
  } else r = await api.unlink(p.id)
  busy.value = false
  if (!r.ok) {
    error.value = r.error
    return
  }
  toast.add({ severity: 'success', summary: t('platforms.linksSaved'), detail: p.name, life: 2500 })
  await app.loadRepos()
  visible.value = false
}
</script>

<template>
  <Dialog
    v-model:visible="visible"
    :header="modMode ? t('platforms.linkedCode') + ' · ' + (project?.name ?? '') : t('platforms.linkTitle', { name: project?.name ?? '' })"
    modal
    dismissable-mask
    :style="{ width: '600px' }"
    :breakpoints="{ '680px': '94vw' }"
  >
    <div class="form">
      <p class="hint">
        {{ modMode ? t('platforms.fixNeedsLink') : t('platforms.linkText') }}
      </p>
      <IconField>
        <InputIcon class="pi pi-search" />
        <InputText
          v-model="search"
          :placeholder="t('platforms.linkSearch')"
          :aria-label="t('platforms.linkSearch')"
          autofocus
          fluid
        />
      </IconField>
      <div
        class="list"
        role="listbox"
        :aria-multiselectable="!modMode"
      >
        <p
          v-if="!options.length"
          class="hint empty"
        >
          {{ modMode ? t('platforms.linkEmptyCode') : t('platforms.linkEmpty') }}
        </p>
        <label
          v-for="r in options"
          :key="r.id"
          class="opt"
          :class="{ on: modMode ? code === r.id : picked.includes(r.id) }"
        >
          <RadioButton
            v-if="modMode"
            v-model="code"
            :value="r.id"
            name="code"
          />
          <Checkbox
            v-else
            v-model="picked"
            :value="r.id"
          />
          <PlatformIcon
            :platform="r.platform"
            :size="18"
          />
          <span class="o-main">
            <span class="o-name">{{ r.name }}</span>
            <span class="o-meta muted">{{ platformName(r.platform) }}<template v-if="elsewhere(r)"> · {{ t('platforms.linkedElsewhere', { name: elsewhere(r) }) }}</template></span>
          </span>
          <span class="o-count mono muted">{{ r.open }}</span>
        </label>
      </div>
      <Message
        v-if="error"
        severity="error"
        size="small"
        variant="simple"
      >
        {{ error }}
      </Message>
    </div>
    <template #footer>
      <Button
        v-if="modMode && project?.linkedTo"
        :label="t('platforms.unlink')"
        icon="pi pi-times"
        severity="danger"
        text
        class="unlink"
        :disabled="busy"
        @click="code = 0"
      />
      <span
        v-if="!modMode"
        class="muted count"
      >{{ picked.length }} {{ t('platforms.mods').toLowerCase() }}</span>
      <Button
        :label="t('common.cancel')"
        severity="secondary"
        text
        @click="visible = false"
      />
      <Button
        :label="t('platforms.save')"
        icon="pi pi-link"
        :loading="busy"
        :disabled="!changed"
        @click="save"
      />
    </template>
  </Dialog>
</template>

<style scoped>
.form {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.hint {
  margin: 0;
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.empty {
  padding: 16px;
  text-align: center;
}

.list {
  display: flex;
  flex-direction: column;
  max-height: 360px;
  overflow-y: auto;
  border: 1px solid var(--iw-border);
  border-radius: var(--iw-radius);
}

.opt {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 12px;
  border-bottom: 1px solid var(--iw-border);
  cursor: pointer;
}

.opt:last-child {
  border-bottom: 0;
}

.opt:hover {
  background: var(--iw-hover);
}

.opt.on {
  background: var(--iw-primary-soft);
}

.o-main {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-width: 0;
}

.o-name {
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.o-meta {
  font-size: calc(12px * var(--iw-fs, 1));
}

.count {
  margin-right: auto;
  font-size: calc(13px * var(--iw-fs, 1));
}

.unlink {
  margin-right: auto;
}
</style>
