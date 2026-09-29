<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Textarea from 'primevue/textarea'
import ToggleSwitch from 'primevue/toggleswitch'
import Message from 'primevue/message'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import PlatformIcon from '../../components/PlatformIcon.vue'
import { api } from '../../api/client'
import type { ModPlatform, PlatformStatus, SteamStatus } from '../../api/types'
import { useAppStore } from '../../stores/app'
import { useSettingsStore } from '../../stores/settings'
import { absTime, relTime } from '../../lib/format'

// Settings › Платформы: one card per mod platform. Nexus / CurseForge run
// through the owner's MCP servers (settings.providers, applied live); Steam is
// native, its secrets go to PUT /api/providers/steam and are never echoed back.
type ModId = 'nexus' | 'curseforge'
const app = useAppStore()
const settings = useSettingsStore()
const confirm = useConfirm()
const toast = useToast()
const { t } = useI18n()

const cards: { id: ModId | 'steam'; text: string }[] = [
  { id: 'nexus', text: 'platforms.nexusText' },
  { id: 'curseforge', text: 'platforms.curseforgeText' },
  { id: 'steam', text: 'platforms.steamText' },
]

const status = (id: string): PlatformStatus | undefined => app.platforms.find((p) => p.id === id)
const cfg = (id: ModId): ModPlatform | undefined => settings.doc?.settings.providers?.[id]
const tone = (s?: PlatformStatus) =>
  !s || s.state === 'disabled' ? 'off' : s.state === 'connected' ? 'ok' : s.state === 'unknown' ? 'busy' : 'error'

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
onMounted(() => {
  void loadSteam()
  void app.loadPlatforms()
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
function forgetCookies() {
  void saveSteam({ steamLoginSecure: '', sessionid: '' })
}
function clearSteam() {
  confirm.require({
    header: t('platforms.steamClearTitle'),
    message: t('platforms.steamClearText'),
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', outlined: true },
    acceptProps: { label: t('platforms.steamClear'), severity: 'danger' },
    accept: () => {
      steamDraft.steamId = ''
      void saveSteam({ steamId: '', apiKey: '', steamLoginSecure: '', sessionid: '' })
    },
  })
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
          <span class="dot" /> {{ t('platforms.state.' + (status(c.id)?.state ?? 'disabled')) }}
          <template v-if="status(c.id)?.account && status(c.id)?.state === 'connected'">
            {{ t('platforms.as', { account: status(c.id)?.account }) }}
          </template>
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
      <span><b class="mono">{{ status(c.id)?.projects ?? 0 }}</b> {{ t('platforms.projects') }}</span>
      <span
        v-if="status(c.id)?.lastSync"
        v-tooltip.top="absTime(status(c.id)?.lastSync ?? '')"
      >{{ t('platforms.lastSync', { time: relTime(status(c.id)?.lastSync ?? '') }) }}</span>
      <span v-if="status(c.id)?.running"><i class="pi pi-server" /> {{ t('platforms.serverRunning') }}</span>
    </div>

    <p
      v-if="status(c.id)?.error && status(c.id)?.state !== 'connected'"
      class="err"
    >
      <i class="pi pi-exclamation-triangle" /> {{ status(c.id)?.error }}
    </p>

    <!-- Nexus / CurseForge: author + MCP server -->
    <template v-if="c.id !== 'steam' && cfg(c.id)">
      <label class="field">
        <span class="label">{{ t(c.id === 'nexus' ? 'platforms.nexusAuthor' : 'platforms.curseforgeAuthor') }}</span>
        <InputText
          v-model="drafts[c.id as ModId].author"
          :placeholder="c.id === 'nexus' ? 'Morgott' : ''"
          fluid
        />
        <small class="muted">{{ t(c.id === 'nexus' ? 'platforms.nexusAuthorHint' : 'platforms.curseforgeAuthorHint') }}</small>
      </label>
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
          <span class="label">{{ t('platforms.command') }}</span>
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

    <!-- Steam: SteamID, API key, cookies (write-only) -->
    <template v-if="c.id === 'steam'">
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
      <Message
        v-if="steam?.session === 'expired'"
        severity="warn"
        size="small"
      >
        {{ t('platforms.sessionExpired') }}
      </Message>
      <small
        v-if="status('steam') && !status('steam')?.capabilities.reply"
        class="muted"
      ><i class="pi pi-info-circle" /> {{ t('platforms.steamReplyOff') }}</small>
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
        :label="t('platforms.check')"
        icon="pi pi-refresh"
        severity="secondary"
        outlined
        :disabled="!status(c.id)?.enabled || !!checks[c.id]"
        :loading="!!checks[c.id]"
        @click="check(c.id)"
      />
      <Button
        v-if="c.id === 'steam' && steam?.hasCookies"
        :label="t('platforms.clearCookies')"
        icon="pi pi-eraser"
        severity="secondary"
        text
        :disabled="busy.steam"
        @click="forgetCookies"
      />
      <Button
        v-if="c.id === 'steam' && steam?.configured"
        :label="t('platforms.steamClear')"
        icon="pi pi-times"
        severity="secondary"
        text
        :disabled="busy.steam"
        @click="clearSteam"
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
</style>
