<script setup lang="ts">
import { ref, watch } from 'vue'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { api } from '../api/client'
import { useAppStore } from '../stores/app'

// Whether this instance can show the native folder dialog (null = not asked yet).
const pickerAvailable = ref<boolean | null>(null)

// «Привязать папку»: map (change, unmap) a project's local git clone.
// PUT /api/projects/{id}/path refuses a folder that is not a clone of the
// project (422 {code: missing|notGit|mismatch}); the error shows inline.
const props = defineProps<{ project: { id: number; name: string; localPath?: string } | null }>()
const visible = defineModel<boolean>('visible', { required: true })
const emit = defineEmits<{ saved: [path: string] }>()
const { t, te } = useI18n()
const toast = useToast()
const app = useAppStore()

const path = ref('')
const error = ref('')
const busy = ref(false)
const picking = ref(false)
watch(visible, (v) => {
  if (!v) return
  path.value = props.project?.localPath ?? ''
  error.value = ''
  if (pickerAvailable.value === null) {
    void api.folderDialog().then((r) => (pickerAvailable.value = r.ok && r.data.available))
  }
})

// «Обзор…» opens the native Windows folder dialog (not in a headless instance);
// the chosen folder is linked right away, so the usual clone check applies.
async function browse() {
  const p = props.project
  if (!p || picking.value || busy.value) return
  picking.value = true
  error.value = ''
  const r = await api.pickFolder({ projectId: p.id, title: t('folder.browseTitle', { name: p.name }), initial: path.value.trim() || undefined })
  picking.value = false
  if (!r.ok) {
    if ((r.body as { code?: string } | undefined)?.code === 'unavailable') pickerAvailable.value = false
    else error.value = r.error
    return
  }
  if (r.data.cancelled || !r.data.path) return
  path.value = r.data.path
  await save(r.data.path)
}

async function save(next: string) {
  const p = props.project
  if (!p || busy.value) return
  busy.value = true
  error.value = ''
  const r = await api.setFolder(p.id, next.trim())
  busy.value = false
  if (!r.ok) {
    const code = (r.body as { code?: string } | undefined)?.code ?? ''
    error.value = code && te('folder.error.' + code) ? t('folder.error.' + code, { name: p.name }) : r.error
    return
  }
  toast.add({ severity: 'success', summary: next.trim() ? t('folder.saved') : t('folder.unmapped'), detail: p.name, life: 3000 })
  void app.loadRepos()
  emit('saved', r.data.localPath)
  visible.value = false
}
</script>

<template>
  <Dialog
    v-model:visible="visible"
    :header="t('folder.title', { name: project?.name ?? '' })"
    modal
    dismissable-mask
    :style="{ width: '560px' }"
    :breakpoints="{ '640px': '94vw' }"
  >
    <form
      class="form"
      @submit.prevent="save(path)"
    >
      <p class="hint">
        {{ t('folder.text') }}
      </p>
      <div class="row">
        <InputText
          v-model="path"
          class="mono grow"
          :placeholder="t('folder.placeholder')"
          :aria-label="t('folder.title', { name: project?.name ?? '' })"
          :invalid="!!error"
          autofocus
          fluid
        />
        <Button
          v-if="pickerAvailable"
          :label="t('folder.browse')"
          icon="pi pi-folder"
          severity="secondary"
          outlined
          :loading="picking"
          :disabled="busy"
          @click="browse"
        />
      </div>
      <Message
        v-if="error"
        severity="error"
        size="small"
        variant="simple"
      >
        {{ error }}
      </Message>
      <RouterLink
        to="/settings/projects"
        class="small"
        @click="visible = false"
      >
        <i class="pi pi-search" /> {{ t('folder.discover') }}
      </RouterLink>
    </form>
    <template #footer>
      <Button
        v-if="project?.localPath"
        :label="t('folder.unmap')"
        icon="pi pi-times"
        severity="danger"
        text
        :disabled="busy"
        class="unmap"
        @click="save('')"
      />
      <Button
        :label="t('common.cancel')"
        severity="secondary"
        text
        @click="visible = false"
      />
      <Button
        :label="t('folder.save')"
        icon="pi pi-folder-open"
        :loading="busy"
        :disabled="!path.trim() || path.trim() === (project?.localPath ?? '')"
        @click="save(path)"
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

.row {
  display: flex;
  gap: 8px;
  align-items: center;
}

.grow {
  flex: 1;
  min-width: 0;
}

.hint {
  margin: 0;
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.small {
  align-self: flex-start;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.unmap {
  margin-right: auto;
}
</style>
