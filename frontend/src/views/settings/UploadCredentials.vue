<script setup lang="ts">
import { onMounted, ref } from 'vue'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { api, type Result } from '../../api/client'
import type { CurseForgeUploadStatus } from '../../api/types'
import ToolsStatus from './ToolsStatus.vue'
import CredentialBlock from './CredentialBlock.vue'

// Upload credentials of one platform card (Settings › Платформы), Phase 5:
// CurseForge — the upload API token (write-only, checked before it is stored);
// Steam — no credentials: Workshop uploads go through the owner's running,
// signed-in Steam client (steamcmd's own sign-in logged the Steam client out).
const props = defineProps<{ platform: 'curseforge' | 'steam' }>()
const confirm = useConfirm()
const toast = useToast()
const { t, te } = useI18n()

const CF_TOKENS_URL = 'https://authors.curseforge.com/account/api-tokens'
const busy = ref('')
const error = ref('')
const errorDetail = ref('')

function failed(r: Extract<Result<unknown>, { ok: false }>, scope: 'cfUpload' | 'steamUpload') {
  const code = (r.body as { code?: unknown } | undefined)?.code
  error.value =
    typeof code === 'string' && te(`platforms.${scope}.errors.${code}`) ? t(`platforms.${scope}.errors.${code}`) : r.error || t('platforms.errors.failed')
  errorDetail.value = r.error
}
function clearError() {
  error.value = errorDetail.value = ''
}

// --- CurseForge token
const cf = ref<CurseForgeUploadStatus | null>(null)
const cfDraft = ref('')
const cfEditing = ref(false)
async function saveToken(remove = false) {
  const token = remove ? '' : cfDraft.value.trim()
  if (!remove && !token) return
  busy.value = remove ? 'remove' : 'save'
  clearError()
  const r = await api.saveCfUpload(token)
  busy.value = ''
  cfDraft.value = ''
  if (!r.ok) return failed(r, 'cfUpload')
  cf.value = r.data
  cfEditing.value = false
  toast.add({ severity: 'success', summary: t(remove ? 'platforms.cfUpload.removed' : 'platforms.cfUpload.saved'), life: 2500 })
}
function removeToken() {
  confirm.require({
    header: t('platforms.cfUpload.removeTitle'),
    message: t('platforms.cfUpload.removeText'),
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', outlined: true },
    acceptProps: { label: t('platforms.cfUpload.remove'), severity: 'danger' },
    accept: () => void saveToken(true),
  })
}
async function checkToken() {
  busy.value = 'check'
  clearError()
  const r = await api.checkCfUpload()
  busy.value = ''
  if (!r.ok) {
    if ((r.body as { code?: string } | undefined)?.code === 'no_token') cf.value = { hasToken: false }
    return failed(r, 'cfUpload')
  }
  cf.value = r.data
  toast.add({ severity: 'success', summary: t('platforms.cfUpload.verified'), life: 2500 })
}

onMounted(async () => {
  if (props.platform === 'curseforge') {
    const r = await api.cfUpload()
    if (r.ok) cf.value = r.data
  }
})
</script>

<template>
  <!-- CurseForge: upload API token -->
  <CredentialBlock
    v-if="platform === 'curseforge'"
    v-model:editing="cfEditing"
    :title="t('platforms.cfUpload.title')"
    :configured="!!cf?.hasToken"
    :summary="t('platforms.cfUpload.stored')"
    :empty="t('platforms.cfUpload.none')"
    :checked-at="cf?.checkedAt"
    :busy="busy"
    :remove-label="t('platforms.cfUpload.remove')"
    @check="checkToken()"
    @remove="removeToken()"
  >
    <template #hint>
      {{ t('platforms.cfUpload.hint') }}
      <a
        :href="CF_TOKENS_URL"
        target="_blank"
        rel="noopener noreferrer"
      >{{ t('platforms.cfUpload.getToken') }} <i class="pi pi-external-link" /></a>
    </template>
    <div class="key-row">
      <InputText
        v-model="cfDraft"
        type="password"
        autocomplete="off"
        :placeholder="t('platforms.cfUpload.placeholder')"
        :aria-label="t('platforms.cfUpload.title')"
        fluid
        @keydown.enter="saveToken()"
      />
      <Button
        :label="t('platforms.cfUpload.save')"
        icon="pi pi-check"
        :disabled="!cfDraft.trim() || !!busy"
        :loading="busy === 'save'"
        @click="saveToken()"
      />
    </div>
    <template #after>
      <p
        v-if="error"
        v-tooltip.top="errorDetail || undefined"
        class="err small"
      >
        <i class="pi pi-exclamation-triangle" /> {{ error }}
      </p>
    </template>
  </CredentialBlock>

  <!-- Steam: helpers (a compact line); uploads use the running Steam client -->
  <template v-else>
    <ToolsStatus />
    <p class="muted small">
      <i class="pi pi-info-circle" /> {{ t('platforms.steamUpload.viaClient') }}
    </p>
  </template>
</template>
<style scoped>
.adv {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px;
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  border: 1px solid var(--iw-border);
}

.key-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.key-state {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.key-state.ok {
  color: var(--iw-success);
}

.key-state.warn {
  color: var(--iw-warning, var(--iw-danger));
}

.key-row {
  display: flex;
  gap: 8px;
}

.key-actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}

.login {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.err {
  margin: 0;
  color: var(--iw-danger);
  overflow-wrap: anywhere;
}

.small {
  font-size: calc(12.5px * var(--iw-fs, 1));
  margin: 0;
}

.progress {
  color: var(--iw-muted);
}

.progress.attention {
  color: var(--iw-text);
  font-weight: 600;
}
</style>
