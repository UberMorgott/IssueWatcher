<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { api, type Result } from '../../api/client'
import type { CurseForgeUploadStatus, SteamUploadStatus } from '../../api/types'
import { absTime } from '../../lib/format'

// Upload credentials of one platform card (Settings › Платформы), Phase 5:
// CurseForge — the upload API token (write-only, checked before it is stored);
// Steam — steamcmd (found on the PC or downloaded from Valve on a click) and
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
const form = reactive({ user: '', password: '', code: '', path: '' })
const showPath = ref(false)
const running = computed(() => ['starting', 'need_code', 'confirm_mobile'].includes(steam.value?.login.state ?? ''))
let timer: ReturnType<typeof setTimeout> | undefined
async function loadSteam() {
  const r = await api.steamUpload()
  if (r.ok) {
    const was = steam.value?.login.state
    steam.value = r.data
    if (was && was !== r.data.login.state) {
      if (r.data.login.state === 'ok') toast.add({ severity: 'success', summary: t('platforms.steamUpload.signedIn', { user: r.data.user ?? '' }), life: 3000 })
      if (r.data.login.state === 'failed') error.value = r.data.login.error || t('platforms.steamUpload.failed')
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
const install = () => steamCall('install', api.steamUploadInstall, 'platforms.steamUpload.installed')
const checkSteam = () => steamCall('check', api.steamUploadCheck, 'platforms.steamUpload.verified')
const savePath = () => steamCall('path', () => api.steamUploadPath(form.path.trim()))
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
  <div
    v-if="platform === 'curseforge'"
    class="adv"
    role="group"
    :aria-label="t('platforms.cfUpload.title')"
  >
    <div class="key-head">
      <span class="label">{{ t('platforms.cfUpload.title') }}</span>
      <span
        v-if="cf?.hasToken"
        v-tooltip.top="cf.checkedAt ? t('platforms.checked', { time: absTime(cf.checkedAt) }) : undefined"
        class="key-state ok"
      ><i class="pi pi-check-circle" /> {{ t('platforms.cfUpload.stored') }}</span>
      <span
        v-else
        class="key-state muted"
      ><i class="pi pi-key" /> {{ t('platforms.cfUpload.none') }}</span>
    </div>
    <small class="muted">{{ t('platforms.cfUpload.hint') }}
      <a
        :href="CF_TOKENS_URL"
        target="_blank"
        rel="noopener noreferrer"
      >{{ t('platforms.cfUpload.getToken') }} <i class="pi pi-external-link" /></a>
    </small>
    <div class="key-row">
      <InputText
        v-model="cfDraft"
        type="password"
        autocomplete="off"
        :placeholder="cf?.hasToken ? t('platforms.savedSecret') : t('platforms.cfUpload.placeholder')"
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
    <div
      v-if="cf?.hasToken"
      class="key-actions"
    >
      <Button
        :label="t('platforms.check')"
        icon="pi pi-refresh"
        size="small"
        severity="secondary"
        outlined
        :disabled="!!busy"
        :loading="busy === 'check'"
        @click="checkToken()"
      />
      <Button
        :label="t('platforms.cfUpload.remove')"
        icon="pi pi-trash"
        size="small"
        severity="secondary"
        text
        :disabled="!!busy"
        :loading="busy === 'remove'"
        @click="removeToken()"
      />
    </div>
    <p
      v-if="error"
      v-tooltip.top="errorDetail || undefined"
      class="err small"
    >
      <i class="pi pi-exclamation-triangle" /> {{ error }}
    </p>
  </div>

  <!-- Steam: steamcmd + its one-time sign-in -->
  <div
    v-else
    class="adv"
    role="group"
    :aria-label="t('platforms.steamUpload.title')"
  >
    <div class="key-head">
      <span class="label">{{ t('platforms.steamUpload.title') }}</span>
      <span
        v-if="steam?.loggedIn"
        v-tooltip.top="steam.checkedAt ? t('platforms.checked', { time: absTime(steam.checkedAt) }) : undefined"
        class="key-state ok"
      ><i class="pi pi-check-circle" /> {{ t('platforms.steamUpload.signedInAs', { user: steam.user }) }}</span>
      <span
        v-else-if="steam?.expired"
        class="key-state warn"
      ><i class="pi pi-exclamation-circle" /> {{ t('platforms.steamUpload.expired', { user: steam.user }) }}</span>
      <span
        v-else
        class="key-state muted"
      ><i class="pi pi-key" /> {{ t('platforms.steamUpload.none') }}</span>
    </div>
    <small class="muted">{{ t('platforms.steamUpload.hint') }}</small>

    <!-- steamcmd -->
    <div class="tool">
      <span
        v-if="steam?.steamcmd"
        class="muted small mono path"
        :title="steam.steamcmd"
      ><i class="pi pi-wrench" /> {{ steam.steamcmd }} · {{ t('platforms.steamUpload.source.' + (steam.source ?? 'configured')) }}</span>
      <span
        v-else
        class="muted small"
      ><i class="pi pi-info-circle" /> {{ t('platforms.steamUpload.noSteamcmd') }}</span>
      <div class="key-actions">
        <Button
          v-if="!steam?.steamcmd"
          :label="t('platforms.steamUpload.install')"
          icon="pi pi-download"
          size="small"
          :disabled="!!busy || running"
          :loading="busy === 'install'"
          @click="install()"
        />
        <Button
          :label="t('platforms.steamUpload.setPath')"
          icon="pi pi-folder-open"
          size="small"
          severity="secondary"
          text
          @click="showPath = !showPath"
        />
      </div>
      <div
        v-if="showPath"
        class="key-row"
      >
        <InputText
          v-model="form.path"
          :placeholder="t('platforms.steamUpload.pathPlaceholder')"
          :aria-label="t('platforms.steamUpload.setPath')"
          fluid
          @keydown.enter="savePath()"
        />
        <Button
          :label="t('common.save')"
          icon="pi pi-check"
          :disabled="!!busy"
          :loading="busy === 'path'"
          @click="savePath()"
        />
      </div>
    </div>

    <!-- sign-in in progress -->
    <template v-if="running">
      <p class="muted small">
        <i class="pi pi-spin pi-spinner" /> {{ t('platforms.steamUpload.state.' + steam?.login.state) }}
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
          :label="t(steam?.user ? 'platforms.steamUpload.signInAgain' : 'platforms.steamUpload.signIn')"
          icon="pi pi-sign-in"
          :disabled="!form.user.trim() || !form.password || !!busy"
          :loading="busy === 'login'"
        />
        <Button
          v-if="steam?.user"
          :label="t('platforms.steamUpload.check')"
          icon="pi pi-refresh"
          size="small"
          severity="secondary"
          outlined
          :disabled="!!busy"
          :loading="busy === 'check'"
          @click="checkSteam()"
        />
        <Button
          v-if="steam?.user"
          :label="t('platforms.steamUpload.forget')"
          icon="pi pi-trash"
          size="small"
          severity="secondary"
          text
          :disabled="!!busy"
          @click="forget()"
        />
      </div>
      <small class="muted">{{ t('platforms.steamUpload.privacy') }}</small>
    </form>

    <p
      v-if="error"
      v-tooltip.top="errorDetail || undefined"
      class="err small"
    >
      <i class="pi pi-exclamation-triangle" /> {{ error }}
    </p>
  </div>
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

.tool,
.login {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.path {
  overflow-wrap: anywhere;
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
</style>
