<script setup lang="ts">
import { computed, ref } from 'vue'
import Button from 'primevue/button'
import Skeleton from 'primevue/skeleton'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import PlatformIcon from '../components/PlatformIcon.vue'
import { api } from '../api/client'
import { useAppStore } from '../stores/app'
import { absTime, relTime } from '../lib/format'

const app = useAppStore()
const confirm = useConfirm()
const toast = useToast()
const busy = ref(false)

const planned = [
  { id: 'curseforge', name: 'CurseForge', text: 'Mod comments via your browser session (no official comments API).', phase: 'Phase 4' },
  { id: 'nexusmods', name: 'Nexus Mods', text: 'Sign in with Nexus SSO; posts, bugs and comments of your mods.', phase: 'Phase 4' },
  { id: 'steam', name: 'Steam Workshop', text: 'Workshop item comment threads via your Steam session.', phase: 'Phase 4' },
]

const gh = computed(() => app.github)
const ghState = computed(() => {
  const p = gh.value
  if (!app.authLoaded) return { tone: 'off', text: 'Checking…' }
  if (!p) return { tone: 'off', text: app.authError || 'Unavailable' }
  if (p.connected) return { tone: 'ok', text: 'Connected' }
  if (p.state === 'connecting') return { tone: 'busy', text: 'Waiting for GitHub…' }
  if (p.state === 'error') return { tone: 'error', text: 'Error' }
  return { tone: 'off', text: p.setupNeeded ? 'Not set up' : 'Disconnected' }
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
    toast.add({ severity: 'warn', summary: 'Copy failed', detail: 'Select the code and copy it manually.', life: 3000 })
  }
}

function disconnect() {
  confirm.require({
    header: 'Disconnect GitHub?',
    message: 'IssueWatcher forgets the local token and stops syncing. Synced issues stay in the local database; your GitHub App registration is kept, so reconnecting is one click.',
    icon: 'pi pi-sign-out',
    rejectProps: { label: 'Cancel', severity: 'secondary', outlined: true },
    acceptProps: { label: 'Disconnect', severity: 'danger' },
    accept: async () => {
      busy.value = true
      await app.disconnect('github')
      busy.value = false
    },
  })
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">
          Connections
        </h2>
        <p class="page-sub">
          Platforms IssueWatcher reads from. Credentials stay on this machine in <span class="mono">data\secrets</span>.
        </p>
      </div>
    </div>

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
                <template v-if="app.sync?.lastSync">
                  Last sync <span v-tooltip.top="absTime(app.sync.lastSync)">{{ relTime(app.sync.lastSync) }}</span> · every {{ app.sync.interval }}
                </template>
                <template v-else>
                  Not synced yet
                </template>
              </div>
            </div>
          </div>
          <div class="facts">
            <span><b class="mono">{{ app.repos.length }}</b> repositories</span>
            <span><b class="mono">{{ app.repos.reduce((n, r) => n + r.open, 0) }}</b> open issues</span>
          </div>
        </template>
        <p
          v-else
          class="card-text"
        >
          Issues, comments and replies across every repository you install your private GitHub App on.
          <template v-if="gh?.setupNeeded">
            First connect creates the app on github.com — confirm, install, authorize.
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
          <i class="pi pi-spin pi-spinner" /> Continue on github.com in the browser tab that opened — this page updates by itself.
        </p>
        <p
          v-if="gh?.device?.pending && gh.device.userCode"
          class="device"
        >
          Enter code <b class="mono code">{{ gh.device.userCode }}</b>
          <Button
            v-tooltip.top="copied ? 'Copied' : 'Copy code'"
            :icon="copied ? 'pi pi-check' : 'pi pi-copy'"
            size="small"
            severity="secondary"
            text
            rounded
            aria-label="Copy code"
            @click="copyCode(gh.device.userCode)"
          />
          at
          <a
            :href="gh.device.verificationUri"
            target="_blank"
            rel="noopener noreferrer"
          >{{ gh.device.verificationUri }}</a>
        </p>
        <p
          v-if="deviceError"
          class="err"
        >
          <i class="pi pi-exclamation-triangle" /> {{ deviceError }}
        </p>

        <footer class="card-foot">
          <template v-if="gh?.connected">
            <Button
              label="Disconnect"
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
              label="App settings"
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
              label="Choose repositories"
              icon="pi pi-plus"
              severity="secondary"
              text
            />
          </template>
          <template v-else>
            <Button
              label="Connect GitHub"
              icon="pi pi-github"
              :loading="busy"
              :disabled="!app.authLoaded || !gh"
              @click="connect"
            />
            <Button
              v-if="gh && !gh.setupNeeded"
              v-tooltip.top="'If the browser redirect fails: sign in with a one-time code (enable Device Flow in the app settings first)'"
              label="Use a device code"
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
              <span class="dot" /> Planned
            </div>
          </div>
          <span class="phase-badge">{{ p.phase }}</span>
        </header>
        <p class="card-text">
          {{ p.text }}
        </p>
        <footer class="card-foot">
          <span v-tooltip.top="'Coming soon'">
            <Button
              label="Connect"
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
  font-size: 18px;
  font-weight: 650;
}

.status {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
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
  font-size: 12.5px;
}

.facts {
  display: flex;
  gap: 20px;
  color: var(--iw-muted);
  font-size: 13px;
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
