<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Textarea from 'primevue/textarea'
import ToggleSwitch from 'primevue/toggleswitch'
import Message from 'primevue/message'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { encode } from 'uqr'
import PlatformIcon from '../../components/PlatformIcon.vue'
import { api } from '../../api/client'
import type { LoginStatus, ModPlatform, PlatformStatus, SteamStatus } from '../../api/types'
import { useAppStore } from '../../stores/app'
import { useSettingsStore } from '../../stores/settings'
import { absTime, relTime } from '../../lib/format'

// Settings › Платформы: one card per mod platform. «Подключить» is the whole
// setup: Steam shows a QR code from Steam's own sign-in service (scan it in
// the Steam app), Nexus / CurseForge switch on and their MCP server imports a
// browser session or opens its sign-in window. Accounts are detected; the
// manual fields (author, MCP command, Steam id/key/cookies) sit under
// «Дополнительно». A tray card «войдите снова» opens /connections?login=<id>.
type ModId = 'nexus' | 'curseforge'
type CardId = ModId | 'steam'
const app = useAppStore()
const settings = useSettingsStore()
const confirm = useConfirm()
const toast = useToast()
const route = useRoute()
const router = useRouter()
const { t } = useI18n()

const cards: { id: CardId; text: string }[] = [
  { id: 'nexus', text: 'platforms.nexusText' },
  { id: 'curseforge', text: 'platforms.curseforgeText' },
  { id: 'steam', text: 'platforms.steamText' },
]

const status = (id: string): PlatformStatus | undefined => app.platforms.find((p) => p.id === id)
const cfg = (id: ModId): ModPlatform | undefined => settings.doc?.settings.providers?.[id]
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
const active = (id: string) => ['qr', 'scanned', 'window'].includes(login[id]?.state ?? '')
const connectLabel = (id: string) => t(status(id)?.state === 'relogin' ? 'platforms.reconnect' : 'platforms.connect')
const showConnect = (id: CardId) => {
  const s = status(id)
  if (!s || active(id)) return false
  if (id === 'steam') return s.state !== 'connected' || !steam.value?.signedIn || steam.value.session === 'expired'
  return s.state !== 'connected' || s.session === 'none'
}
// «Выйти» while a session is stored; «Отключить» while an account is set up.
const canLogout = (id: CardId) =>
  id === 'steam' ? !!steam.value?.hasCookies : !!cfg(id)?.enabled && (status(id)?.session ?? 'none') !== 'none'
const canDisconnect = (id: CardId) => (id === 'steam' ? !!steam.value?.configured : !!cfg(id)?.enabled)

async function connect(id: CardId) {
  errors[id] = ''
  starting[id] = true
  const r = await api.login(id)
  starting[id] = false
  if (!r.ok) {
    errors[id] = `${t('platforms.loginFailed')}: ${r.error}`
    return
  }
  if (id !== 'steam') void Promise.all([settings.load(), app.loadPlatforms()]) // switched on server-side
  onLogin(id, r.data)
}

function onLogin(id: CardId, s: LoginStatus) {
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
      errors[id] = `${t('platforms.loginFailed')}${s.error ? ': ' + s.error : ''}`
      login[id] = null
      return
    case 'idle':
      if (id !== 'steam') errors[id] = t('platforms.windowClosed')
      login[id] = null
  }
}

async function poll(id: CardId) {
  if (!login[id]) return
  const r = await api.loginStatus(id)
  if (!login[id]) return // cancelled meanwhile
  if (r.ok) onLogin(id, r.data)
  else timers[id] = window.setTimeout(() => void poll(id), 5000) // server busy: keep waiting
}

async function connected(id: CardId, s: LoginStatus) {
  login[id] = null
  toast.add({ severity: 'success', summary: t('platforms.connected', { platform: platformName(id), account: s.account ?? '' }), life: 3500 })
  await Promise.all([settings.load(), app.loadPlatforms(), id === 'steam' ? loadSteam() : Promise.resolve()])
  window.setTimeout(() => void app.loadPlatforms(), 4000) // the server-side check lands a moment later
}

function cancelLogin(id: CardId) {
  clearTimeout(timers[id])
  login[id] = null
  void api.cancelLogin(id)
}
onBeforeUnmount(() => {
  for (const id of Object.keys(timers)) clearTimeout(timers[id])
  if (active('steam')) void api.cancelLogin('steam')
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

// --- MCP platforms: switch, author, server command (drafts saved on demand)
const drafts = reactive<Record<ModId, { author: string; command: string; args: string }>>({
  nexus: { author: '', command: '', args: '' },
  curseforge: { author: '', command: '', args: '' },
})
const advanced = reactive<Record<string, boolean>>({})
function syncDrafts() {
  for (const id of ['nexus', 'curseforge'] as ModId[]) {
    const c = cfg(id)
    if (!c) continue
    drafts[id] = { author: c.author ?? '', command: c.mcp.command, args: c.mcp.args.join('\n') }
  }
}
watch(() => settings.doc?.revision, syncDrafts, { immediate: true })

const busy = reactive<Record<string, boolean>>({})
const errors = reactive<Record<string, string>>({})

async function toggle(id: ModId, on: boolean) {
  busy[id] = true
  errors[id] = ''
  const err = await settings.patch({ providers: { [id]: { enabled: on } } })
  busy[id] = false
  if (err) {
    errors[id] = err
    return
  }
  toast.add({ severity: 'success', summary: t(on ? 'platforms.enabled' : 'platforms.disabled'), life: 2500 })
  await app.loadPlatforms()
  if (on) void check(id)
  else cancelLogin(id)
}

const dirty = (id: ModId) => {
  const c = cfg(id)
  if (!c) return false
  const d = drafts[id]
  return d.author.trim() !== (c.author ?? '') || d.command.trim() !== c.mcp.command || argsOf(d.args).join('\n') !== c.mcp.args.join('\n')
}
const argsOf = (s: string) => s.split('\n').map((a) => a.trim()).filter(Boolean)

async function saveMod(id: ModId) {
  const d = drafts[id]
  busy[id] = true
  errors[id] = ''
  // args is an array: rebuilt on retry so a 409 never re-sends a stale one.
  const err = await settings.patch(() => ({ providers: { [id]: { author: d.author.trim(), mcp: { command: d.command.trim(), args: argsOf(d.args) } } } }))
  busy[id] = false
  if (err) {
    errors[id] = err
    return
  }
  toast.add({ severity: 'success', summary: t('platforms.saved'), life: 2000 })
  void check(id)
}

// Checks may overlap (save + switch): the spinner stays until the last one ends.
const checks = reactive<Record<string, number>>({})
async function check(id: string) {
  checks[id] = (checks[id] ?? 0) + 1
  errors[id] = ''
  const r = await api.checkPlatform(id)
  checks[id]--
  if (r.ok) app.setPlatform(r.data)
  else errors[id] = r.error
}

// --- Steam
const steam = ref<SteamStatus | null>(null)
const steamDraft = reactive({ steamId: '', apiKey: '', loginSecure: '', sessionid: '' })
async function loadSteam() {
  const r = await api.steam()
  if (r.ok) {
    steam.value = r.data
    steamDraft.steamId = r.data.steamId
  }
}
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
  () => steamDraft.steamId.trim() !== (steam.value?.steamId ?? '') || !!steamDraft.apiKey || !!steamDraft.loginSecure || !!steamDraft.sessionid,
)
async function saveSteam(extra: Record<string, string> = {}) {
  busy.steam = true
  errors.steam = ''
  const u: Record<string, string> = { ...extra }
  if (steamDraft.steamId.trim() !== (steam.value?.steamId ?? '')) u.steamId = steamDraft.steamId.trim()
  if (steamDraft.apiKey) u.apiKey = steamDraft.apiKey.trim()
  if (steamDraft.loginSecure) u.steamLoginSecure = steamDraft.loginSecure.trim()
  if (steamDraft.sessionid) u.sessionid = steamDraft.sessionid.trim()
  const r = await api.saveSteam(u)
  busy.steam = false
  if (!r.ok) {
    errors.steam = r.error
    return false
  }
  steam.value = r.data
  steamDraft.steamId = r.data.steamId
  steamDraft.apiKey = steamDraft.loginSecure = steamDraft.sessionid = ''
  toast.add({ severity: 'success', summary: t('platforms.steamSaved'), life: 2000 })
  await app.loadPlatforms()
  if (r.data.configured) void check('steam')
  return true
}
// «Выйти» drops the session (public reads go on); «Отключить» (forget) also
// the detected account, and the platform stops syncing («Не подключено»).
function logout(id: CardId, forget: boolean) {
  const platform = platformName(id)
  confirm.require({
    header: t(forget ? 'platforms.disconnectTitle' : 'platforms.logoutTitle', { platform }),
    message: t(forget ? 'platforms.disconnectText' : 'platforms.logoutText'),
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', outlined: true },
    acceptProps: { label: t(forget ? 'platforms.disconnect' : 'platforms.logout'), severity: 'danger' },
    accept: () => void doLogout(id, forget, platform),
  })
}
async function doLogout(id: CardId, forget: boolean, platform: string) {
  if (active(id)) cancelLogin(id)
  busy[id] = true
  errors[id] = ''
  const r = await api.logoutPlatform(id, forget)
  busy[id] = false
  if (!r.ok) {
    errors[id] = r.error
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
          <span class="dot" /> {{ statusText(c.id) }}
        </div>
        <div
          v-if="statusHint(c.id)"
          class="status-hint"
        >
          {{ statusHint(c.id) }}
        </div>
      </div>
      <ToggleSwitch
        v-if="c.id !== 'steam'"
        v-tooltip.top="t('platforms.enable')"
        class="switch"
        :model-value="!!cfg(c.id)?.enabled"
        :disabled="!settings.doc || busy[c.id]"
        :aria-label="t('platforms.enable')"
        @update:model-value="(v: boolean) => toggle(c.id as ModId, v)"
      />
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
      <span v-if="status(c.id)?.running"><i class="pi pi-server" /> {{ t('platforms.serverRunning') }}</span>
    </div>

    <p
      v-if="status(c.id)?.error && status(c.id)?.state !== 'connected' && !active(c.id)"
      class="err"
    >
      <i class="pi pi-exclamation-triangle" /> {{ status(c.id)?.error }}
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
      <i class="pi pi-spin pi-spinner" /> {{ t('platforms.extracting') }}
    </Message>
    <Message
      v-if="c.id !== 'steam' && login[c.id]?.state === 'window'"
      severity="info"
      size="small"
    >
      <i class="pi pi-spin pi-spinner" />
      {{
        login[c.id]?.via === 'default-browser'
          ? t('platforms.browserTabOpen', { platform: platformName(c.id), browser: login[c.id]?.browser || t('platforms.defaultBrowser') })
          : t('platforms.windowOpen', { platform: platformName(c.id) })
      }}
    </Message>

    <!-- Nexus / CurseForge: author + MCP server (all optional) -->
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
          <span class="label">{{ t(c.id === 'nexus' ? 'platforms.nexusAuthor' : 'platforms.curseforgeAuthor') }}</span>
          <InputText
            v-model="drafts[c.id as ModId].author"
            fluid
          />
          <small class="muted">{{ t(c.id === 'nexus' ? 'platforms.nexusAuthorHint' : 'platforms.curseforgeAuthorHint') }}</small>
        </label>
        <label class="field">
          <span class="label">{{ t('platforms.mcpServer') }} · {{ t('platforms.command') }}</span>
          <InputText
            v-model="drafts[c.id as ModId].command"
            class="mono"
            fluid
          />
        </label>
        <label class="field">
          <span class="label">{{ t('platforms.args') }}</span>
          <Textarea
            v-model="drafts[c.id as ModId].args"
            class="mono"
            rows="2"
            auto-resize
            fluid
          />
        </label>
        <small class="muted">{{ t('platforms.applyLive') }}</small>
      </div>
    </template>

    <!-- Steam: manual SteamID, API key, cookies (write-only), all optional -->
    <template v-if="c.id === 'steam'">
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
        <i :class="advanced.steam ? 'pi pi-chevron-down' : 'pi pi-chevron-right'" /> {{ t('platforms.manual') }}
      </button>
      <div
        v-if="advanced.steam"
        class="adv"
      >
        <label class="field">
          <span class="label">{{ t('platforms.steamId') }}</span>
          <InputText
            v-model="steamDraft.steamId"
            placeholder="7656119…"
            class="mono"
            fluid
          />
        </label>
        <label class="field">
          <span class="label">{{ t('platforms.apiKey') }}</span>
          <InputText
            v-model="steamDraft.apiKey"
            type="password"
            autocomplete="off"
            :placeholder="steam?.hasApiKey ? t('platforms.savedSecret') : ''"
            fluid
          />
          <small class="muted">{{ t('platforms.apiKeyHint') }}</small>
        </label>
        <div class="field">
          <span class="label">{{ t('platforms.cookies') }}</span>
          <div class="pair">
            <InputText
              v-model="steamDraft.loginSecure"
              type="password"
              autocomplete="off"
              :placeholder="steam?.hasCookies ? 'steamLoginSecure · ' + t('platforms.savedSecret') : 'steamLoginSecure'"
              aria-label="steamLoginSecure"
              fluid
            />
            <InputText
              v-model="steamDraft.sessionid"
              type="password"
              autocomplete="off"
              :placeholder="steam?.hasCookies ? 'sessionid · ' + t('platforms.savedSecret') : 'sessionid'"
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
        :label="t('platforms.logout')"
        icon="pi pi-sign-out"
        severity="secondary"
        text
        :disabled="busy[c.id]"
        @click="logout(c.id, false)"
      />
      <Button
        v-if="canDisconnect(c.id)"
        :label="t('platforms.disconnect')"
        icon="pi pi-times"
        severity="secondary"
        text
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
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.dot {
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

.switch {
  align-self: flex-start;
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