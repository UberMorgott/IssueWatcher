<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { encode } from 'uqr'
import PlatformIcon from '../../components/PlatformIcon.vue'
import UploadCredentials from './UploadCredentials.vue'
import CredentialBlock from './CredentialBlock.vue'
import { api, type Result } from '../../api/client'
import type { LoginStatus, ModPlatform, NativePlatform, NexusKeyStatus, PlatformStatus, SteamStatus } from '../../api/types'
import { useAppStore } from '../../stores/app'
import { useSettingsStore } from '../../stores/settings'
import { absTime, relTime } from '../../lib/format'

// Settings › Платформы: one card per mod platform. «Подключить» is the whole
// setup: Steam shows a QR code from Steam's own sign-in service (scan it in
// the Steam app); Nexus / CurseForge / Factorio switch on and sign in in a
// window of the installed browser. «Отключить» switches off and forgets the
// session and the account (manual author too); «Выйти» drops only the session
// where public reads go on without it. Accounts are detected (Steam: the SteamID
// comes from the QR session); the manual overrides (author, Steam id/key/cookies)
// sit under the collapsed «Дополнительно». A tray card «войдите снова» opens /connections?login=<id>.
// Server errors arrive as codes (internal/api Code*) with their own text; the raw reason is a tooltip.
type ModId = 'nexus' | 'curseforge' | 'factorio'
type CardId = ModId | 'steam'
const app = useAppStore()
const settings = useSettingsStore()
const confirm = useConfirm()
const toast = useToast()
const route = useRoute()
const router = useRouter()
const { t, te } = useI18n()

/** The text of a server error code; unknown / missing code → a generic failure (raw reason in the tooltip). */
const codeText = (code?: string) => t(code && te('platforms.errors.' + code) ? 'platforms.errors.' + code : 'platforms.errors.failed')
/** Text of a failed call: its {code}, «not running» as is, else generic. */
function callError(r: Extract<Result<unknown>, { ok: false }>) {
  const code = (r.body as { code?: unknown } | undefined)?.code
  if (typeof code === 'string' && code) return codeText(code)
  return r.status === 0 ? r.error : codeText()
}

const cards: { id: CardId; text: string }[] = [
  { id: 'nexus', text: 'platforms.nexusText' },
  { id: 'curseforge', text: 'platforms.curseforgeText' },
  { id: 'factorio', text: 'platforms.factorioText' },
  { id: 'steam', text: 'platforms.steamText' },
]

const status = (id: string): PlatformStatus | undefined => app.platforms.find((p) => p.id === id)
const cfg = (id: ModId): ModPlatform | NativePlatform | undefined => settings.doc?.settings.providers?.[id]
// Connected without a web session = public reads only: never «Подключено как».
const readOnly = (s?: PlatformStatus) => s?.state === 'connected' && s.session === 'none'
const tone = (s?: PlatformStatus) =>
  !s || s.state === 'disabled'
    ? 'off'
    : readOnly(s)
      ? 'ro'
      : s.state === 'connected'
        ? 'ok'
        : s.state === 'unknown'
          ? 'busy'
          : 'error'
const platformName = (id: string) => status(id)?.name ?? id
function statusText(id: string) {
  const s = status(id)
  if (readOnly(s)) return t('platforms.readOnly')
  const text = t('platforms.state.' + (s?.state ?? 'disabled'))
  const who = s?.accountName || s?.account
  return s?.state === 'connected' && who ? `${text} ${t('platforms.as', { account: who })}` : text
}
// Where the session came from («сессия из браузера», «вход в окне», «вход по QR»);
// read-only Steam shows the public profile name instead.
function statusHint(id: string) {
  const s = status(id)
  // Switched off with a manual author kept: «Подключить» goes on with it.
  const author = id !== 'steam' ? cfg(id as ModId)?.author : ''
  if (s?.state === 'disabled' && author) return t('platforms.savedAuthor', { author })
  if (!s || s.state !== 'connected') return ''
  if (readOnly(s)) return s.accountName ? t('platforms.profile', { name: s.accountName }) : ''
  if (!s.session) return ''
  if (s.session === 'browser' && s.browser) return t('platforms.source.browserNamed', { browser: s.browser })
  return t('platforms.source.' + s.session)
}

// --- «Подключить»: start, then poll the sign-in until connected / closed.
const login = reactive<Record<string, LoginStatus | null>>({})
const starting = reactive<Record<string, boolean>>({})
const timers: Record<string, number> = {}
/** Set on unmount: late responses of connect / poll must not start timers again. */
let disposed = false
const active = (id: string) => ['qr', 'scanned', 'window'].includes(login[id]?.state ?? '')
const connectLabel = (id: string) => {
  const s = status(id)
  if (s?.state === 'relogin') return t('platforms.reconnect')
  return t(readOnly(s) ? 'platforms.signInToReply' : 'platforms.connect')
}
const showConnect = (id: CardId) => {
  const s = status(id)
  if (!s || active(id)) return false
  if (id === 'steam') return s.state !== 'connected' || !steam.value?.signedIn || steam.value.session === 'expired'
  return s.state !== 'connected' || s.session === 'none'
}
// «Выйти» while a session is stored and public reads go on without it (a
// known account: Steam's SteamID, a mod platform's author); «Отключить» while
// anything is set up, a switched-off platform's kept author included.
const canLogout = (id: CardId) =>
  id === 'steam'
    ? !!steam.value?.hasCookies && !!steam.value.steamId
    : !!cfg(id)?.enabled && !!cfg(id)?.author && (status(id)?.session ?? 'none') !== 'none'
const canDisconnect = (id: CardId) =>
  id === 'steam' ? !!steam.value?.configured || !!steam.value?.hasCookies : !!cfg(id)?.enabled || !!cfg(id)?.author

/**
 * The platform's own error line (the raw reason is its tooltip). Hidden while
 * a sign-in runs, while a local error of the card shows, and where a dedicated
 * message says it already (Steam «сессия истекла»).
 */
function cardError(id: CardId) {
  const s = status(id)
  if (!s?.error || s.state === 'connected' || active(id) || errors[id]) return ''
  if (id === 'steam' && steam.value?.session === 'expired') return ''
  return codeText(s.errorCode)
}
/** Raw reason of errors[id] (tooltip). */
const errorDetail = reactive<Record<string, string>>({})
function setError(id: string, text: string, detail = '') {
  errors[id] = text
  errorDetail[id] = detail
}

async function connect(id: CardId) {
  setError(id, '')
  starting[id] = true
  const r = await api.login(id)
  starting[id] = false
  if (disposed) {
    if (r.ok && id === 'steam') void api.cancelLogin('steam') // the QR would wait with nobody to scan it
    return
  }
  if (!r.ok) {
    setError(id, `${t('platforms.loginFailed')}: ${callError(r)}`, r.error)
    return
  }
  if (id !== 'steam') void Promise.all([settings.load(), app.loadPlatforms()]) // switched on server-side
  if (r.data.state === 'failed') {
    // The start itself failed (browser missing, Steam unreachable).
    setError(id, `${t('platforms.loginFailed')}: ${codeText(r.data.errorCode)}`, r.data.error)
    return
  }
  onLogin(id, r.data)
}

function onLogin(id: CardId, s: LoginStatus) {
  if (disposed) return // left the page: a request that returned late starts no new poll
  clearTimeout(timers[id])
  login[id] = s
  switch (s.state) {
    case 'connected':
      void connected(id, s)
      return
    case 'qr':
    case 'scanned':
    case 'window':
      timers[id] = window.setTimeout(() => void poll(id), id === 'steam' ? 2000 : 3000)
      return
    case 'failed':
      setError(id, `${t('platforms.loginEnded')}: ${codeText(s.errorCode)}`, s.error)
      login[id] = null
      return
    case 'idle':
      // Ended without a session: say why (window closed, timed out, another window open); a cancel is silent.
      if (id !== 'steam' && s.errorCode !== 'cancelled') {
        setError(id, !s.errorCode || s.errorCode === 'window_closed' ? t('platforms.windowClosed') : codeText(s.errorCode), s.error)
      }
      login[id] = null
  }
}

async function poll(id: CardId) {
  if (!login[id]) return
  const r = await api.loginStatus(id)
  if (disposed || !login[id]) return // cancelled or left the page meanwhile
  if (r.ok) onLogin(id, r.data)
  else timers[id] = window.setTimeout(() => void poll(id), 5000) // server busy: keep waiting
}

async function connected(id: CardId, s: LoginStatus) {
  login[id] = null
  const platform = platformName(id)
  const account = id === 'steam' ? '' : s.account // Steam's is a raw SteamID64: the card shows the profile name
  toast.add({
    severity: 'success',
    summary: account ? t('platforms.connected', { platform, account }) : t('platforms.connectedNoAccount', { platform }),
    life: 3500,
  })
  await Promise.all([settings.load(), app.loadPlatforms(), id === 'steam' ? loadSteam() : Promise.resolve()])
  window.setTimeout(() => void app.loadPlatforms(), 4000) // the server-side check lands a moment later
}

function cancelLogin(id: CardId) {
  clearTimeout(timers[id])
  login[id] = null
  void api.cancelLogin(id)
}
onBeforeUnmount(() => {
  disposed = true
  for (const id of Object.keys(timers)) clearTimeout(timers[id])
  if (active('steam')) void api.cancelLogin('steam')
  for (const id of Object.keys(login)) login[id] = null
})

// The QR code of Steam's challenge URL, drawn as one SVG path (no v-html).
const qr = computed(() => {
  const url = login.steam?.challengeUrl
  if (!url) return null
  const { data, size } = encode(url, { border: 2 })
  let d = ''
  data.forEach((row, y) => row.forEach((on, x) => { if (on) d += `M${x} ${y}h1v1h-1z` }))
  return { d, size }
})

// --- mod platforms: author (drafts saved on demand)
const drafts = reactive<Record<ModId, { author: string }>>({
  nexus: { author: '' },
  curseforge: { author: '' },
  factorio: { author: '' },
})
const advanced = reactive<Record<string, boolean>>({})
function syncDrafts() {
  for (const id of ['nexus', 'curseforge', 'factorio'] as ModId[]) {
    const c = cfg(id)
    if (!c) continue
    drafts[id] = { author: c.author ?? '' }
  }
}
watch(() => settings.doc?.revision, syncDrafts, { immediate: true })

const busy = reactive<Record<string, boolean>>({})
const errors = reactive<Record<string, string>>({})

const dirty = (id: ModId) => {
  const c = cfg(id)
  if (!c) return false
  return drafts[id].author.trim() !== (c.author ?? '')
}

async function saveMod(id: ModId) {
  const d = drafts[id]
  busy[id] = true
  setError(id, '')
  const err = await settings.patch({ providers: { [id]: { author: d.author.trim() } } })
  busy[id] = false
  if (err) {
    setError(id, err)
    return
  }
  toast.add({ severity: 'success', summary: t('platforms.saved'), life: 2000 })
  void check(id)
}

// Checks may overlap (save + switch): the spinner stays until the last one ends.
const checks = reactive<Record<string, number>>({})
async function check(id: string) {
  checks[id] = (checks[id] ?? 0) + 1
  setError(id, '')
  const r = await api.checkPlatform(id)
  checks[id]--
  if (r.ok) app.setPlatform(r.data)
  else setError(id, callError(r), r.error)
}

// --- Steam
const steam = ref<SteamStatus | null>(null)
// The detected account is shown read-only; steamId is only a manual override
// (empty = keep the account from the QR sign-in).
const steamDraft = reactive({ steamId: '', apiKey: '', loginSecure: '', sessionid: '' })
/** A saved secret shows as «Сохранено · Изменить»; its fields only after «Изменить». */
const steamEdit = reactive({ apiKey: false, cookies: false })
async function loadSteam() {
  const r = await api.steam()
  if (r.ok) steam.value = r.data
}
const steamIdChanged = () => {
  const v = steamDraft.steamId.trim()
  return !!v && v !== (steam.value?.steamId ?? '')
}
const steamAccount = computed(() => {
  const s = steam.value
  if (!s?.steamId) return ''
  return s.persona ? `${s.persona} (${s.steamId})` : s.steamId
})
onMounted(async () => {
  void app.loadPlatforms()
  await loadSteam()
  // Tray card «войдите снова» → /connections?login=<id>: start that sign-in.
  const want = route.query.login
  if (typeof want === 'string' && cards.some((c) => c.id === want)) {
    void router.replace({ query: { ...route.query, login: undefined } })
    void connect(want as CardId)
  }
})
const steamDirty = computed(
  () => steamIdChanged() || !!steamDraft.apiKey || !!steamDraft.loginSecure || !!steamDraft.sessionid,
)
async function saveSteam(extra: Record<string, string> = {}) {
  busy.steam = true
  setError('steam', '')
  const u: Record<string, string> = { ...extra }
  if (steamIdChanged()) u.steamId = steamDraft.steamId.trim()
  if (steamDraft.apiKey) u.apiKey = steamDraft.apiKey.trim()
  if (steamDraft.loginSecure) u.steamLoginSecure = steamDraft.loginSecure.trim()
  if (steamDraft.sessionid) u.sessionid = steamDraft.sessionid.trim()
  const r = await api.saveSteam(u)
  busy.steam = false
  if (!r.ok) {
    setError('steam', callError(r), r.error)
    return false
  }
  steam.value = r.data
  steamDraft.steamId = steamDraft.apiKey = steamDraft.loginSecure = steamDraft.sessionid = ''
  steamEdit.apiKey = steamEdit.cookies = false
  toast.add({ severity: 'success', summary: t('platforms.steamSaved'), life: 2000 })
  await app.loadPlatforms()
  if (r.data.configured) void check('steam')
  return true
}
// --- Nexus API key (publishing): write-only; the server validates it with
// v1 users/validate before storing, so only an accepted key is ever saved.
const nexusKey = ref<NexusKeyStatus | null>(null)
const nexusKeyDraft = ref('')
const nexusKeyBusy = ref<'' | 'save' | 'check' | 'remove'>('')
const nexusKeyEditing = ref(false)
const NEXUS_KEY_URL = 'https://www.nexusmods.com/users/myaccount?tab=api'
async function loadNexusKey() {
  const r = await api.nexusKey()
  if (r.ok) nexusKey.value = r.data
}
onMounted(() => void loadNexusKey())
/** Text of a failed key call: its own code first (bad_api_key differs from Steam's), else the card's. */
function nexusKeyError(r: Extract<Result<unknown>, { ok: false }>) {
  const code = (r.body as { code?: unknown } | undefined)?.code
  if (typeof code === 'string' && te('platforms.nexusKey.errors.' + code)) return t('platforms.nexusKey.errors.' + code)
  return callError(r)
}
async function saveNexusKey(remove = false) {
  const key = remove ? '' : nexusKeyDraft.value.trim()
  if (!remove && !key) return
  nexusKeyBusy.value = remove ? 'remove' : 'save'
  setError('nexusKey', '')
  const r = await api.saveNexusKey(key)
  nexusKeyBusy.value = ''
  if (!r.ok) {
    setError('nexusKey', nexusKeyError(r), r.error)
    return
  }
  nexusKey.value = r.data
  nexusKeyDraft.value = ''
  nexusKeyEditing.value = false
  toast.add({
    severity: 'success',
    summary: remove ? t('platforms.nexusKey.removed') : t('platforms.nexusKey.saved', { user: r.data.user ?? '' }),
    life: 2500,
  })
}
function removeNexusKey() {
  confirm.require({
    header: t('platforms.nexusKey.removeTitle'),
    message: t('platforms.nexusKey.removeText'),
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', outlined: true },
    acceptProps: { label: t('platforms.nexusKey.remove'), severity: 'danger' },
    accept: () => void saveNexusKey(true),
  })
}
async function checkNexusKey() {
  nexusKeyBusy.value = 'check'
  setError('nexusKey', '')
  const r = await api.checkNexusKey()
  nexusKeyBusy.value = ''
  if (!r.ok) {
    setError('nexusKey', nexusKeyError(r), r.error)
    if ((r.body as { code?: string } | undefined)?.code === 'no_api_key') nexusKey.value = { hasApiKey: false }
    return
  }
  nexusKey.value = r.data
  toast.add({ severity: 'success', summary: t('platforms.nexusKey.verified', { user: r.data.user ?? '' }), life: 2500 })
}

// «Выйти» drops the session (public reads go on); «Отключить» (forget) also
// the detected and manual account, and the platform stops syncing («Не подключено»).
function logout(id: CardId, forget: boolean) {
  const platform = platformName(id)
  const text = !forget ? 'platforms.logoutText' : id === 'steam' ? 'platforms.disconnectTextSteam' : 'platforms.disconnectText'
  confirm.require({
    header: t(forget ? 'platforms.disconnectTitle' : 'platforms.logoutTitle', { platform }),
    message: t(text),
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', outlined: true },
    acceptProps: { label: t(forget ? 'platforms.disconnect' : 'platforms.logout'), severity: 'danger' },
    accept: () => void doLogout(id, forget, platform),
  })
}
async function doLogout(id: CardId, forget: boolean, platform: string) {
  if (active(id)) cancelLogin(id)
  busy[id] = true
  setError(id, '')
  const r = await api.logoutPlatform(id, forget)
  busy[id] = false
  if (!r.ok) {
    setError(id, callError(r), r.error)
    return
  }
  app.setPlatform(r.data)
  toast.add({ severity: 'success', summary: t(forget ? 'platforms.disconnectedToast' : 'platforms.loggedOut', { platform }), life: 2500 })
  await Promise.all([settings.load(), app.loadPlatforms(), id === 'steam' ? loadSteam() : Promise.resolve()])
}
</script>

<template>
  <article
    v-for="c in cards"
    :key="c.id"
    class="panel card"
    :data-platform="c.id"
  >
    <header class="card-head">
      <PlatformIcon
        :platform="c.id"
        :size="48"
        tile
      />
      <div class="card-title">
        <div class="name">
          {{ status(c.id)?.name ?? c.id }}
        </div>
        <div
          class="status"
          :class="tone(status(c.id))"
        >
          <span class="dot" /><span>{{ statusText(c.id) }}</span>
        </div>
        <div
          v-if="statusHint(c.id)"
          class="status-hint"
        >
          {{ statusHint(c.id) }}
        </div>
      </div>
    </header>

    <p class="card-text">
      {{ t(c.text) }}
    </p>

    <div
      v-if="status(c.id)?.enabled"
      class="facts"
    >
      <span><b class="mono">{{ status(c.id)?.projects ?? 0 }}</b> {{ t('platforms.projects', status(c.id)?.projects ?? 0) }}</span>
      <span
        v-if="status(c.id)?.lastSync"
        v-tooltip.top="absTime(status(c.id)?.lastSync ?? '')"
      >{{ t('platforms.lastSync', { time: relTime(status(c.id)?.lastSync ?? '') }) }}</span>
    </div>

    <p
      v-if="cardError(c.id)"
      v-tooltip.top="status(c.id)?.error"
      class="err"
    >
      <i class="pi pi-exclamation-triangle" /> {{ cardError(c.id) }}
    </p>

    <!-- Sign-in in progress: Steam QR code / the platform's sign-in window -->
    <div
      v-if="c.id === 'steam' && active('steam')"
      class="qr-box"
      role="group"
      :aria-label="t('platforms.qrTitle')"
    >
      <svg
        v-if="qr"
        class="qr"
        :viewBox="`0 0 ${qr.size} ${qr.size}`"
        role="img"
        :aria-label="t('platforms.qrAlt')"
        shape-rendering="crispEdges"
      >
        <rect
          :width="qr.size"
          :height="qr.size"
          fill="#fff"
        />
        <path
          :d="qr.d"
          fill="#000"
        />
      </svg>
      <div class="qr-text">
        <b>{{ t('platforms.qrTitle') }}</b>
        <span>{{ t(login.steam?.state === 'scanned' ? 'platforms.qrScanned' : 'platforms.qrScan') }}</span>
        <span class="muted small"><i class="pi pi-spin pi-spinner" /> {{ t('platforms.connecting') }}</span>
      </div>
    </div>
    <Message
      v-if="c.id === 'steam' && login.steam?.state === 'expired'"
      severity="warn"
      size="small"
    >
      {{ t('platforms.qrExpired') }}
    </Message>
    <!-- Nexus / CurseForge: browser session first, then a sign-in page -->
    <Message
      v-if="c.id !== 'steam' && starting[c.id]"
      severity="info"
      size="small"
    >
      <i class="pi pi-spin pi-spinner" /> {{ t('platforms.checkingSession') }}
    </Message>
    <Message
      v-if="c.id !== 'steam' && login[c.id]?.state === 'window'"
      severity="info"
      size="small"
    >
      <i class="pi pi-spin pi-spinner" />
      {{
        t('platforms.windowOpenNative', { platform: platformName(c.id), browser: login[c.id]?.browser || t('platforms.defaultBrowser') })
      }}
    </Message>

    <small
      v-if="c.id === 'factorio' && status('factorio')?.enabled && !status('factorio')?.capabilities.reply"
      class="muted"
    ><i class="pi pi-info-circle" /> {{ t('platforms.factorioReplyOff') }}</small>

    <!-- Nexus: the API key for publishing (write-only; validated before it is stored) -->
    <CredentialBlock
      v-if="c.id === 'nexus'"
      v-model:editing="nexusKeyEditing"
      class="nexus-key"
      :title="t('platforms.nexusKey.title')"
      :configured="!!nexusKey?.hasApiKey"
      :summary="nexusKey?.user ? t('platforms.nexusKey.verifiedAs', { user: nexusKey.user }) : t('platforms.nexusKey.stored')"
      :empty="t('platforms.nexusKey.none')"
      :checked-at="nexusKey?.checkedAt"
      :busy="nexusKeyBusy"
      :remove-label="t('platforms.nexusKey.remove')"
      @check="checkNexusKey()"
      @remove="removeNexusKey()"
    >
      <template #hint>
        {{ t('platforms.nexusKey.hint') }}
        <a
          :href="NEXUS_KEY_URL"
          target="_blank"
          rel="noopener noreferrer"
        >{{ t('platforms.nexusKey.getKey') }} <i class="pi pi-external-link" /></a>
      </template>
      <div class="key-row">
        <InputText
          v-model="nexusKeyDraft"
          type="password"
          autocomplete="off"
          :placeholder="t('platforms.nexusKey.placeholder')"
          :aria-label="t('platforms.nexusKey.title')"
          fluid
          @keydown.enter="saveNexusKey()"
        />
        <Button
          :label="t('platforms.nexusKey.save')"
          icon="pi pi-check"
          :disabled="!nexusKeyDraft.trim() || !!nexusKeyBusy"
          :loading="nexusKeyBusy === 'save'"
          @click="saveNexusKey()"
        />
      </div>
      <template #after>
        <p
          v-if="errors.nexusKey"
          v-tooltip.top="errorDetail.nexusKey || undefined"
          class="err small"
        >
          <i class="pi pi-exclamation-triangle" /> {{ errors.nexusKey }}
        </p>
      </template>
    </CredentialBlock>

    <!-- CurseForge: upload token; Steam: steamcmd sign-in (publishing) -->
    <UploadCredentials
      v-if="c.id === 'curseforge' || c.id === 'steam'"
      :platform="c.id === 'steam' ? 'steam' : 'curseforge'"
    />

    <!-- Nexus / CurseForge / Factorio: author, optional -->
    <template v-if="c.id !== 'steam' && cfg(c.id)">
      <button
        type="button"
        class="adv-toggle"
        :aria-expanded="!!advanced[c.id]"
        @click="advanced[c.id] = !advanced[c.id]"
      >
        <i :class="advanced[c.id] ? 'pi pi-chevron-down' : 'pi pi-chevron-right'" /> {{ t('platforms.advanced') }}
      </button>
      <div
        v-if="advanced[c.id]"
        class="adv"
      >
        <label class="field">
          <span class="label">{{ t(`platforms.${c.id}Author`) }}</span>
          <InputText
            v-model="drafts[c.id as ModId].author"
            fluid
          />
          <small class="muted">{{ t(`platforms.${c.id}AuthorHint`) }}</small>
        </label>
      </div>
    </template>

    <!-- Steam: detected account (read-only); manual SteamID, API key, cookies (write-only) under «Дополнительно» -->
    <template v-if="c.id === 'steam'">
      <!-- Connected: the status line names the account already (once, not three times). -->
      <small
        v-if="steamAccount && status('steam')?.state !== 'connected'"
        class="muted"
      ><i class="pi pi-user" /> {{ t('platforms.steamAccount', { name: steamAccount }) }}</small>
      <Message
        v-if="steam?.session === 'expired'"
        severity="warn"
        size="small"
      >
        {{ t('platforms.sessionExpired') }}
      </Message>
      <small
        v-if="steam?.hasCookies && steam.session !== 'expired' && !status('steam')?.capabilities.reply"
        class="muted"
      ><i class="pi pi-info-circle" /> {{ t('platforms.steamReplyOff') }}</small>
      <button
        type="button"
        class="adv-toggle"
        :aria-expanded="!!advanced.steam"
        @click="advanced.steam = !advanced.steam"
      >
        <i :class="advanced.steam ? 'pi pi-chevron-down' : 'pi pi-chevron-right'" /> {{ t('platforms.advanced') }}
      </button>
      <div
        v-if="advanced.steam"
        class="adv"
      >
        <label class="field">
          <span class="label">{{ t('platforms.steamId') }}</span>
          <InputText
            v-model="steamDraft.steamId"
            :placeholder="steam?.steamId || '7656119…'"
            class="mono"
            fluid
          />
          <small class="muted">{{ t('platforms.steamIdHint') }}</small>
        </label>
        <div
          v-if="steam?.hasApiKey && !steamEdit.apiKey"
          class="saved-row"
        >
          <span class="label">{{ t('platforms.apiKey') }}</span>
          <span class="key-state ok"><i class="pi pi-check-circle" /> {{ t('platforms.secretSaved') }}</span>
          <Button
            :label="t('platforms.credential.change')"
            icon="pi pi-pencil"
            size="small"
            severity="secondary"
            text
            @click="steamEdit.apiKey = true"
          />
        </div>
        <label
          v-else
          class="field"
        >
          <span class="label">{{ t('platforms.apiKey') }}</span>
          <InputText
            v-model="steamDraft.apiKey"
            type="password"
            autocomplete="off"
            fluid
          />
          <small class="muted">{{ t('platforms.apiKeyHint') }}</small>
        </label>
        <div
          v-if="steam?.hasCookies && !steamEdit.cookies"
          class="saved-row"
        >
          <span class="label">{{ t('platforms.cookies') }}</span>
          <span class="key-state ok"><i class="pi pi-check-circle" /> {{ t('platforms.secretSaved') }}</span>
          <Button
            :label="t('platforms.credential.change')"
            icon="pi pi-pencil"
            size="small"
            severity="secondary"
            text
            @click="steamEdit.cookies = true"
          />
        </div>
        <div
          v-else
          class="field"
        >
          <span class="label">{{ t('platforms.cookies') }}</span>
          <div class="pair">
            <InputText
              v-model="steamDraft.loginSecure"
              type="password"
              autocomplete="off"
              placeholder="steamLoginSecure"
              aria-label="steamLoginSecure"
              fluid
            />
            <InputText
              v-model="steamDraft.sessionid"
              type="password"
              autocomplete="off"
              placeholder="sessionid"
              aria-label="sessionid"
              fluid
            />
          </div>
          <small class="muted">{{ t('platforms.cookiesHint') }}</small>
        </div>
      </div>
    </template>

    <p
      v-if="errors[c.id]"
      v-tooltip.top="errorDetail[c.id] || undefined"
      class="err"
    >
      <i class="pi pi-exclamation-triangle" /> {{ errors[c.id] }}
    </p>
    <p
      v-if="checks[c.id]"
      class="muted small"
    >
      <i class="pi pi-spin pi-spinner" /> {{ t('platforms.checking') }}
    </p>

    <footer class="card-foot">
      <Button
        v-if="showConnect(c.id)"
        :label="c.id === 'steam' && login.steam?.state === 'expired' ? t('platforms.qrRetry') : connectLabel(c.id)"
        :icon="c.id === 'steam' ? 'pi pi-qrcode' : 'pi pi-sign-in'"
        :loading="starting[c.id]"
        @click="connect(c.id)"
      />
      <Button
        v-if="active(c.id)"
        :label="t('common.cancel')"
        icon="pi pi-times"
        severity="secondary"
        outlined
        @click="cancelLogin(c.id)"
      />
      <Button
        v-if="c.id !== 'steam' && dirty(c.id as ModId)"
        :label="t('platforms.save')"
        icon="pi pi-check"
        :loading="busy[c.id]"
        @click="saveMod(c.id as ModId)"
      />
      <Button
        v-if="c.id === 'steam' && steamDirty"
        :label="t('platforms.save')"
        icon="pi pi-check"
        :loading="busy.steam"
        @click="saveSteam()"
      />
      <Button
        v-if="status(c.id)?.enabled"
        :label="t('platforms.check')"
        icon="pi pi-refresh"
        severity="secondary"
        outlined
        :disabled="!!checks[c.id]"
        :loading="!!checks[c.id]"
        @click="check(c.id)"
      />
      <Button
        v-if="canLogout(c.id)"
        :label="t(c.id === 'steam' ? 'platforms.logoutQr' : 'platforms.logout')"
        icon="pi pi-sign-out"
        severity="secondary"
        text
        :disabled="busy[c.id]"
        @click="logout(c.id, false)"
      />
      <Button
        v-if="canDisconnect(c.id)"
        :label="t('platforms.disconnect')"
        icon="pi pi-power-off"
        severity="secondary"
        outlined
        :disabled="busy[c.id]"
        @click="logout(c.id, true)"
      />
      <span
        v-if="status(c.id)?.checkedAt"
        v-tooltip.top="absTime(status(c.id)?.checkedAt ?? '')"
        class="muted small checked"
      >{{ t('platforms.checked', { time: relTime(status(c.id)?.checkedAt ?? '') }) }}</span>
    </footer>
  </article>
</template>

<style scoped>
.card {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 22px;
}

.card-head {
  display: flex;
  align-items: center;
  gap: 14px;
}

.card-title {
  flex: 1;
  min-width: 0;
}

.name {
  font-size: calc(18px * var(--iw-fs, 1));
  font-weight: 650;
}

.status {
  display: inline-flex;
  align-items: flex-start;
  gap: 6px;
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.dot {
  flex: none;
  margin-top: 0.45em;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--iw-dimmed);
}

.status.ok {
  color: var(--iw-success);
}

.status.ok .dot {
  background: var(--iw-success);
  box-shadow: 0 0 0 3px var(--iw-success-soft);
}

.status.ro {
  color: var(--iw-warn);
}

.status.ro .dot {
  background: var(--iw-warn);
  box-shadow: 0 0 0 3px var(--iw-warn-soft);
}

.status-hint {
  margin-top: 2px;
  font-size: calc(12.5px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.status.busy .dot {
  background: var(--iw-primary);
}

.status.error {
  color: var(--iw-danger);
}

.status.error .dot {
  background: var(--iw-danger);
}

.card-text {
  margin: 0;
  color: var(--iw-muted);
}

.facts {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  color: var(--iw-muted);
  font-size: calc(13px * var(--iw-fs, 1));
}

.facts b {
  color: var(--iw-text);
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.label {
  font-size: calc(13px * var(--iw-fs, 1));
  font-weight: 500;
}

.saved-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px 10px;
}

.pair {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px;
}

.adv-toggle {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  align-self: flex-start;
  padding: 0;
  border: 0;
  background: none;
  font: inherit;
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
  cursor: pointer;
}

.adv-toggle:hover {
  color: var(--iw-text);
}

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

.key-row {
  display: flex;
  gap: 8px;
}

.key-actions {
  display: flex;
  flex-wrap: wrap;
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

.card-foot {
  margin-top: auto;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}

.checked {
  margin-left: auto;
}

@media (width <= 767px) {
  .pair {
    grid-template-columns: minmax(0, 1fr);
  }
}
.qr-box {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 14px;
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  border: 1px solid var(--iw-border);
}

.qr {
  flex: none;
  width: 168px;
  height: 168px;
  border-radius: 6px;
}

.qr-text {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
}

@media (width <= 767px) {
  .qr-box {
    flex-direction: column;
    align-items: flex-start;
  }
}
</style>