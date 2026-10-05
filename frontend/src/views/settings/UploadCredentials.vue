<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { api, type Result } from '../../api/client'
import type { CurseForgeUploadStatus, SteamUploadStatus } from '../../api/types'
import ToolsStatus from './ToolsStatus.vue'
import CredentialBlock from './CredentialBlock.vue'

// Upload credentials of one platform card (Settings › Платформы), Phase 5:
// CurseForge — the upload API token (write-only, checked before it is stored);
// Steam — steamcmd (the app's own copy in data\tools, set up automatically) and
// its one-time sign-in: the login, password and Steam Guard code go straight
// to steamcmd's console; only steamcmd's own cached sign-in is kept.
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

// --- Steam (steamcmd)
const steam = ref<SteamUploadStatus | null>(null)
const form = reactive({ user: '', password: '', code: '' })
// A sign-in runs: steamcmd starts / updates itself / signs in / waits for a
// Steam Guard code or the approval in the Steam app on the phone.
const running = computed(() =>
  ['starting', 'updating', 'logging_in', 'need_code', 'confirm_mobile'].includes(steam.value?.login.state ?? ''),
)
const steamEditing = ref(false)
const stateText = computed(() => {
  const k = `platforms.steamUpload.state.${steam.value?.login.state ?? ''}`
  return te(k) ? t(k) : t('platforms.steamUpload.state.starting')
})
let timer: ReturnType<typeof setTimeout> | undefined
async function loadSteam() {
  const r = await api.steamUpload()
  if (r.ok) {
    const was = steam.value?.login.state
    steam.value = r.data
    if (was && was !== r.data.login.state) {
      if (r.data.login.state === 'ok') {
        steamEditing.value = false
        toast.add({ severity: 'success', summary: t('platforms.steamUpload.signedIn', { user: r.data.user ?? '' }), life: 3000 })
      }
      if (r.data.login.state === 'failed') {
        // The code words it (ru/en); steamcmd's own line is the tooltip.
        const k = `platforms.steamUpload.loginErrors.${r.data.login.code ?? ''}`
        error.value = te(k) ? t(k) : r.data.login.error || t('platforms.steamUpload.failed')
        errorDetail.value = r.data.login.error ?? ''
      }
    }
  }
  clearTimeout(timer)
  if (running.value) timer = setTimeout(() => void loadSteam(), 1000)
}
async function signIn() {
  if (!form.user.trim() || !form.password) return
  busy.value = 'login'
  clearError()
  const r = await api.steamUploadLogin(form.user.trim(), form.password, form.code.trim())
  form.password = ''
  form.code = ''
  busy.value = ''
  if (!r.ok) return failed(r, 'steamUpload')
  await loadSteam()
}
async function sendCode() {
  if (!form.code.trim()) return
  busy.value = 'code'
  clearError()
  const r = await api.steamUploadCode(form.code.trim())
  form.code = ''
  busy.value = ''
  if (!r.ok) return failed(r, 'steamUpload')
  await loadSteam()
}
async function cancelSignIn() {
  await api.steamUploadCancel()
  await loadSteam()
}
async function steamCall(kind: string, call: () => Promise<Result<SteamUploadStatus>>, done?: string) {
  busy.value = kind
  clearError()
  const r = await call()
  busy.value = ''
  if (!r.ok) {
    failed(r, 'steamUpload')
    await loadSteam()
    return
  }
  steam.value = r.data
  if (done) toast.add({ severity: 'success', summary: t(done, { user: r.data.user ?? '' }), life: 2500 })
}
const checkSteam = () => steamCall('check', api.steamUploadCheck, 'platforms.steamUpload.verified')
function forget() {
  confirm.require({
    header: t('platforms.steamUpload.forgetTitle'),
    message: t('platforms.steamUpload.forgetText'),
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', outlined: true },
    acceptProps: { label: t('platforms.steamUpload.forget'), severity: 'danger' },
    accept: () => void steamCall('forget', api.steamUploadForget),
  })
}

onMounted(async () => {
  if (props.platform === 'curseforge') {
    const r = await api.cfUpload()
    if (r.ok) cf.value = r.data
  } else {
    await loadSteam()
  }
})
onBeforeUnmount(() => clearTimeout(timer))
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

  <!-- Steam: helpers (a compact line) + steamcmd's one-time sign-in -->
  <template v-else>
    <ToolsStatus @ready="loadSteam()" />
    <CredentialBlock
      v-model:editing="steamEditing"
      :title="t('platforms.steamUpload.title')"
      :configured="!!steam?.loggedIn && !running"
      :summary="t('platforms.steamUpload.summary', { user: steam?.user ?? '' })"
      :empty="t('platforms.steamUpload.none')"
      :warn="steam?.expired && !running ? t('platforms.steamUpload.expired', { user: steam.user }) : undefined"
      :checked-at="steam?.checkedAt"
      :busy="busy"
      :check-label="t('platforms.steamUpload.check')"
      :change-label="t('platforms.steamUpload.changeAccount')"
      :remove-label="t('platforms.steamUpload.forget')"
      @check="checkSteam()"
      @remove="forget()"
    >
      <template #hint>
        {{ t('platforms.steamUpload.hint') }}
      </template>
      <p
        v-if="steam && !steam.steamcmd"
        class="muted small"
      >
        <i class="pi pi-info-circle" /> {{ t('platforms.steamUpload.noSteamcmd') }}
      </p>

      <!-- sign-in in progress -->
      <template v-if="running">
        <p
          class="small progress"
          :class="{ attention: steam?.login.state === 'confirm_mobile' || steam?.login.state === 'need_code' }"
          role="status"
        >
          <i
            class="pi"
            :class="steam?.login.state === 'confirm_mobile' ? 'pi-mobile' : 'pi-spin pi-spinner'"
          /> {{ stateText }}
        </p>
        <div
          v-if="steam?.login.state === 'need_code'"
          class="key-row"
        >
          <InputText
            v-model="form.code"
            autocomplete="one-time-code"
            :placeholder="t('platforms.steamUpload.codePlaceholder')"
            :aria-label="t('platforms.steamUpload.code')"
            fluid
            @keydown.enter="sendCode()"
          />
          <Button
            :label="t('platforms.steamUpload.sendCode')"
            icon="pi pi-send"
            :disabled="!form.code.trim() || !!busy"
            :loading="busy === 'code'"
            @click="sendCode()"
          />
        </div>
        <div class="key-actions">
          <Button
            :label="t('common.cancel')"
            size="small"
            severity="secondary"
            text
            @click="cancelSignIn()"
          />
        </div>
      </template>

      <!-- the one-time sign-in form -->
      <form
        v-else-if="steam?.steamcmd"
        class="login"
        autocomplete="off"
        @submit.prevent="signIn()"
      >
        <InputText
          v-model="form.user"
          autocomplete="off"
          :placeholder="t('platforms.steamUpload.user')"
          :aria-label="t('platforms.steamUpload.user')"
          fluid
        />
        <InputText
          v-model="form.password"
          type="password"
          autocomplete="new-password"
          :placeholder="t('platforms.steamUpload.password')"
          :aria-label="t('platforms.steamUpload.password')"
          fluid
        />
        <InputText
          v-model="form.code"
          autocomplete="one-time-code"
          :placeholder="t('platforms.steamUpload.codeOptional')"
          :aria-label="t('platforms.steamUpload.code')"
          fluid
        />
        <div class="key-actions">
          <Button
            type="submit"
            :label="t('platforms.steamUpload.signIn')"
            icon="pi pi-sign-in"
            :disabled="!form.user.trim() || !form.password || !!busy"
            :loading="busy === 'login'"
          />
          <Button
            v-if="steam?.user && !steam.loggedIn"
            :label="t('platforms.steamUpload.forget')"
            icon="pi pi-sign-out"
            size="small"
            severity="secondary"
            text
            :disabled="!!busy"
            @click="forget()"
          />
        </div>
        <small class="muted">{{ t('platforms.steamUpload.privacy') }}</small>
      </form>
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
