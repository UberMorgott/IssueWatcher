<script setup lang="ts">
import { computed, ref } from 'vue'
import Button from 'primevue/button'
import Skeleton from 'primevue/skeleton'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import PlatformIcon from '../components/PlatformIcon.vue'
import { api } from '../api/client'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { absTime, duration, relTime } from '../lib/format'

/** embedded: shown inside Settings → Connections (no page frame). */
defineProps<{ embedded?: boolean }>()
const app = useAppStore()
const confirm = useConfirm()
const toast = useToast()
const { t } = useI18n()
const busy = ref(false)

const planned = [
  { id: 'curseforge', name: 'CurseForge', text: 'connections.curseforgeText', phase: 4 },
  { id: 'nexusmods', name: 'Nexus Mods', text: 'connections.nexusmodsText', phase: 4 },
  { id: 'steam', name: 'Steam Workshop', text: 'connections.steamText', phase: 4 },
]

const gh = computed(() => app.github)
const openTotal = computed(() => app.repos.reduce((n, r) => n + r.open, 0))
const ghState = computed(() => {
  const p = gh.value
  if (!app.authLoaded) return { tone: 'off', text: t('connections.checking') }
  if (!p) return { tone: 'off', text: app.authError || t('connections.unavailable') }
  if (p.connected) return { tone: 'ok', text: t('connections.connected') }
  if (p.state === 'connecting') return { tone: 'busy', text: t('connections.waiting') }
  if (p.state === 'error') return { tone: 'error', text: t('connections.error') }
  return { tone: 'off', text: p.setupNeeded ? t('connections.notSetUp') : t('connections.disconnected') }
})

async function connect() {
  busy.value = true
  await app.connect('github')
  busy.value = false
}

// Device flow: fallback when the loopback redirect cannot work (needs
// "Enable Device Flow" in the GitHub App settings).
const deviceError = ref('')
async function startDevice() {
  deviceError.value = ''
  busy.value = true
  const r = await api.authDevice()
  busy.value = false
  if (!r.ok) deviceError.value = r.error
  await app.loadAuth()
}

const copied = ref(false)
async function copyCode(code: string) {
  try {
    await navigator.clipboard.writeText(code)
    copied.value = true
    window.setTimeout(() => (copied.value = false), 1500)
  } catch {
    toast.add({ severity: 'warn', summary: t('connections.copyFailed'), detail: t('connections.copyFailedText'), life: 3000 })
  }
}

function disconnect() {
  confirm.require({
    header: t('connections.disconnectTitle'),
    message: t('connections.disconnectText'),
    icon: 'pi pi-sign-out',
    rejectProps: { label: t('common.cancel'), severity: 'secondary', outlined: true },
    acceptProps: { label: t('connections.disconnect'), severity: 'danger' },
    accept: async () => {
      busy.value = true
      await app.disconnect('github')
      busy.value = false
    },
  })
}
</script>

<template>
  <div :class="embedded ? 'embedded' : 'page'">
    <i18n-t
      keypath="connections.sub"
      tag="p"
      class="page-intro"
      scope="global"
    >
      <template #path>
        <span class="mono">data\secrets</span>
      </template>
    </i18n-t>

    <div class="cards">
      <article class="panel card primary-card">
        <header class="card-head">
          <PlatformIcon
            platform="github"
            :size="48"
            tile
          />
          <div class="card-title">
            <div class="name">
              GitHub
            </div>
            <div
              class="status"
              :class="ghState.tone"
            >
              <span class="dot" /> {{ ghState.text }}
            </div>
          </div>
        </header>

        <Skeleton
          v-if="!app.authLoaded"
          height="60px"
        />
        <template v-else-if="gh?.connected">
          <div class="account">
            <img
              v-if="gh.avatarUrl"
              :src="gh.avatarUrl"
              alt=""
              class="av"
              referrerpolicy="no-referrer"
            >
            <div>
              <div class="login">
                @{{ gh.login }}
              </div>
              <div class="muted small">
                <i18n-t
                  v-if="app.sync?.lastSync"
                  keypath="connections.lastSync"
                  scope="global"
                >
                  <template #time>
                    <span v-tooltip.top="absTime(app.sync.lastSync)">{{ relTime(app.sync.lastSync) }}</span>
                  </template>
                  <template #interval>
                    {{ duration(app.sync.interval) }}
                  </template>
                </i18n-t>
                <template v-else>
                  {{ t('connections.notSynced') }}
                </template>
              </div>
            </div>
          </div>
          <div class="facts">
            <span><b class="mono">{{ app.repos.length }}</b> {{ t('words.repositories', app.repos.length) }}</span>
            <span><b class="mono">{{ openTotal }}</b> {{ t('words.openIssues', openTotal) }}</span>
          </div>
        </template>
        <p
          v-else
          class="card-text"
        >
          {{ t('connections.cardText') }}
          <template v-if="gh?.setupNeeded">
            {{ t('connections.setupNeeded') }}
          </template>
        </p>
        <p
          v-if="gh?.error || app.authError"
          class="err"
        >
          <i class="pi pi-exclamation-triangle" /> {{ gh?.error || app.authError }}
        </p>
        <p
          v-if="gh?.state === 'connecting' && !gh.device?.pending"
          class="device"
        >
          <i class="pi pi-spin pi-spinner" /> {{ t('common.continueOnGithub') }}
        </p>
        <i18n-t
          v-if="gh?.device?.pending && gh.device.userCode"
          keypath="connections.enterCode"
          tag="p"
          class="device"
          scope="global"
        >
          <template #code>
            <b class="mono code">{{ gh.device.userCode }}</b>
            <Button
              v-tooltip.top="copied ? t('connections.copied') : t('connections.copyCode')"
              :icon="copied ? 'pi pi-check' : 'pi pi-copy'"
              size="small"
              severity="secondary"
              text
              rounded
              :aria-label="t('connections.copyCode')"
              @click="copyCode(gh.device.userCode)"
            />
          </template>
          <template #url>
            <a
              :href="gh.device.verificationUri"
              target="_blank"
              rel="noopener noreferrer"
            >{{ gh.device.verificationUri }}</a>
          </template>
        </i18n-t>
        <p
          v-if="deviceError"
          class="err"
        >
          <i class="pi pi-exclamation-triangle" /> {{ deviceError }}
        </p>

        <footer class="card-foot">
          <template v-if="gh?.connected">
            <Button
              :label="t('connections.disconnect')"
              icon="pi pi-sign-out"
              severity="secondary"
              outlined
              :loading="busy"
              @click="disconnect"
            />
            <Button
              v-if="gh.appUrl"
              as="a"
              :href="gh.appUrl"
              target="_blank"
              rel="noopener noreferrer"
              :label="t('connections.appSettings')"
              icon="pi pi-external-link"
              severity="secondary"
              text
            />
            <Button
              v-if="gh.installUrl"
              as="a"
              :href="gh.installUrl"
              target="_blank"
              rel="noopener noreferrer"
              :label="t('connections.chooseRepos')"
              icon="pi pi-plus"
              severity="secondary"
              text
            />
          </template>
          <template v-else>
            <Button
              :label="t('common.connectGithub')"
              icon="pi pi-github"
              :loading="busy"
              :disabled="!app.authLoaded || !gh"
              @click="connect"
            />
            <Button
              v-if="gh && !gh.setupNeeded"
              v-tooltip.top="t('connections.deviceTip')"
              :label="t('connections.useDevice')"
              icon="pi pi-key"
              severity="secondary"
              text
              :disabled="busy"
              @click="startDevice"
            />
          </template>
        </footer>
      </article>

      <article
        v-for="p in planned"
        :key="p.id"
        class="panel card planned"
      >
        <header class="card-head">
          <PlatformIcon
            :platform="p.id"
            :size="48"
            tile
          />
          <div class="card-title">
            <div class="name">
              {{ p.name }}
            </div>
            <div class="status off">
              <span class="dot" /> {{ t('connections.planned') }}
            </div>
          </div>
          <span class="phase-badge">{{ t('common.phase', { n: p.phase }) }}</span>
        </header>
        <p class="card-text">
          {{ t(p.text) }}
        </p>
        <footer class="card-foot">
          <span v-tooltip.top="t('common.comingSoon')">
            <Button
              :label="t('connections.connect')"
              icon="pi pi-link"
              severity="secondary"
              outlined
              disabled
            />
          </span>
        </footer>
      </article>
    </div>
  </div>
</template>

<style scoped>
.embedded {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.cards {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 20px;
}

.card {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding: 22px;
}

.primary-card {
  border-color: color-mix(in srgb, var(--iw-primary) 35%, var(--iw-border));
}

.card-head {
  display: flex;
  align-items: center;
  gap: 14px;
}

.card-head .phase-badge {
  margin-left: auto;
  align-self: flex-start;
}

.name {
  font-size: calc(18px * var(--iw-fs, 1));
  font-weight: 650;
}

.status {
  display: inline-flex;
  align-items: center;
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

.card-text {
  margin: 0;
  color: var(--iw-muted);
}

.planned {
  opacity: 0.85;
}

.account {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px;
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  border: 1px solid var(--iw-border);
}

.av {
  width: 40px;
  height: 40px;
  border-radius: 50%;
}

.login {
  font-weight: 600;
}

.small {
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.facts {
  display: flex;
  gap: 20px;
  color: var(--iw-muted);
  font-size: calc(13px * var(--iw-fs, 1));
}

.facts b {
  color: var(--iw-text);
}

.err {
  margin: 0;
  color: var(--iw-danger);
}

.device {
  margin: 0;
  padding: 10px 12px;
  border-radius: 8px;
  background: var(--iw-primary-soft);
}

.card-foot {
  margin-top: auto;
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

@media (width <= 1023px) {
  .cards {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
