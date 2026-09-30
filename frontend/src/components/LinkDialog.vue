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

// «Площадки»: group mod pages under a head — the code project (GitHub repo)
// whose folder their fixes run in, or (the same mod on several mod platforms,
// no repository) another mod page. From a code project: multi-select its mod
// pages (PUT /api/projects/{head}/links {mods}: exactly these). From a mod
// page: pick its one head (added to that head's links); a mod page heading a
// group brings its pages along. Only group heads are offered (one level deep).
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

/**
 * The candidates, filtered: mod pages (code mode); group heads (mod mode) —
 * GitHub projects, then mod pages not in a group, never this page or its own pages.
 */
const options = computed(() => {
  const q = search.value.trim().toLowerCase()
  const self = props.project
  const own = new Set(self?.links ?? [])
  return app.repos
    .filter((r) =>
      modMode.value ? r.platform === 'github' || (isModPlatform(r.platform) && !r.linkedTo && r.id !== self?.id && !own.has(r.id)) : isModPlatform(r.platform),
    )
    .filter((r) => !q || r.name.toLowerCase().includes(q) || platformName(r.platform).toLowerCase().includes(q))
    .sort((a, b) => Number(isModPlatform(a.platform)) - Number(isModPlatform(b.platform)) || a.platform.localeCompare(b.platform) || a.name.localeCompare(b.name))
})
const elsewhere = (r: Repo) => (r.linkedTo && r.linkedTo !== props.project?.id ? byId.value.get(r.linkedTo)?.name : undefined)
/** Open issues and bug reports of a candidate (comment threads apart, as on the Projects page). */
const openIssues = (r: Repo) => r.open - (r.openComments ?? 0)

/** What the save moves out of another group (shown before saving, never silently). */
const moves = computed(() => {
  const p = props.project
  if (!p) return []
  if (modMode.value) {
    const from = p.linkedTo && p.linkedTo !== code.value ? byId.value.get(p.linkedTo)?.name : undefined
    return code.value && from ? [t('platforms.moveFrom', { name: p.name, from })] : []
  }
  const cur = new Set(p.links ?? [])
  return picked.value
    .filter((id) => !cur.has(id))
    .map((id) => byId.value.get(id))
    .filter((r): r is Repo => !!r && !!elsewhere(r))
    .map((r) => t('platforms.moveFrom', { name: r.name, from: elsewhere(r) }))
})
/** A mod page heading a group joins another head with its pages. */
const bringsAlong = computed(() => (modMode.value && code.value && props.project?.links?.length ? props.project.links.length : 0))

/** Mod mode without a head: the other mod pages this page can head (the same mod elsewhere). */
const memberOptions = computed(() => {
  const q = search.value.trim().toLowerCase()
  const self = props.project
  return app.repos
    .filter((r) => isModPlatform(r.platform) && r.id !== self?.id)
    .filter((r) => !q || r.name.toLowerCase().includes(q) || platformName(r.platform).toLowerCase().includes(q))
    .sort((a, b) => a.platform.localeCompare(b.platform) || a.name.localeCompare(b.name))
})

const membersChanged = computed(() => [...picked.value].sort().join(',') !== [...(props.project?.links ?? [])].sort().join(','))
const changed = computed(() => {
  const p = props.project
  if (!p) return false
  if (modMode.value) return code.value !== (p.linkedTo ?? 0) || (!code.value && membersChanged.value)
  return membersChanged.value
})

async function save() {
  const p = props.project
  if (!p || busy.value) return
  busy.value = true
  error.value = ''
  let r: { ok: boolean; error?: string; body?: unknown } = { ok: true }
  if (!modMode.value) r = await api.setLinks(p.id, picked.value)
  else if (code.value) {
    const target = byId.value.get(code.value)
    r = await api.setLinks(code.value, [...new Set([...(target?.links ?? []), p.id])])
  } else {
    if (p.linkedTo) r = await api.unlink(p.id)
    if (r.ok && membersChanged.value) r = await api.setLinks(p.id, picked.value) // this page heads the group
  }
  busy.value = false
  if (!r.ok) {
    error.value = (r.body as { code?: string } | undefined)?.code === 'bad_link' ? t('platforms.badLink') : (r.error ?? '')
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
    :header="modMode ? t('platforms.groupTitle', { name: project?.name ?? '' }) : t('platforms.linkTitle', { name: project?.name ?? '' })"
    modal
    dismissable-mask
    :style="{ width: '600px' }"
    :breakpoints="{ '680px': '94vw' }"
  >
    <div class="form">
      <p class="hint">
        {{ modMode ? t('platforms.groupText') : t('platforms.linkText') }}
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
      <span
        v-if="modMode"
        class="sub"
      >{{ t('platforms.groupHead') }}</span>
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
          <span
            v-tooltip.left="t('platforms.openCount', { n: openIssues(r) })"
            class="o-count mono muted"
          >{{ openIssues(r) }}</span>
        </label>
      </div>
      <!-- A mod page without a head can head a group itself: the same mod on the other mod platforms -->
      <template v-if="modMode && !code">
        <span class="sub">{{ t('platforms.groupMembers') }}</span>
        <div
          class="list short"
          role="listbox"
          aria-multiselectable="true"
        >
          <p
            v-if="!memberOptions.length"
            class="hint empty"
          >
            {{ t('platforms.linkEmpty') }}
          </p>
          <label
            v-for="r in memberOptions"
            :key="'m' + r.id"
            class="opt"
            :class="{ on: picked.includes(r.id) }"
          >
            <Checkbox
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
            <span
              v-tooltip.left="t('platforms.openCount', { n: openIssues(r) })"
              class="o-count mono muted"
            >{{ openIssues(r) }}</span>
          </label>
        </div>
      </template>
      <Message
        v-for="m in moves"
        :key="m"
        severity="warn"
        size="small"
        variant="simple"
      >
        {{ m }}
      </Message>
      <Message
        v-if="bringsAlong"
        severity="info"
        size="small"
        variant="simple"
      >
        {{ t('platforms.bringsAlong', bringsAlong) }}
      </Message>
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
      >{{ picked.length }} {{ t('platforms.projects', picked.length) }}</span>
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

.sub {
  font-size: calc(12px * var(--iw-fs, 1));
  font-weight: 600;
  color: var(--iw-muted);
}

.list.short {
  max-height: 200px;
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
