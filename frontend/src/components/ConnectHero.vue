<script setup lang="ts">
import { ref } from 'vue'
import Button from 'primevue/button'
import PlatformIcon from './PlatformIcon.vue'
import { useAppStore } from '../stores/app'

const app = useAppStore()
const busy = ref(false)
const waiting = ref(false)

async function connect() {
  busy.value = true
  waiting.value = await app.connect('github')
  busy.value = false
}

const steps = [
  { icon: 'pi pi-github', title: 'Connect', text: 'Create your private GitHub App and authorize it — two clicks on github.com.' },
  { icon: 'pi pi-sync', title: 'Sync', text: 'Issues and comments from every installed repo land in a local database.' },
  { icon: 'pi pi-inbox', title: 'Review', text: 'Triage, reply and get tray notifications for anything new.' },
]
</script>

<template>
  <section class="hero panel">
    <div class="hero-glow" />
    <div class="hero-main">
      <PlatformIcon
        platform="github"
        :size="56"
        tile
      />
      <h2 class="hero-title">
        Connect GitHub to start watching
      </h2>
      <p class="hero-text">
        IssueWatcher collects issues and comments from all your repositories into one place and
        tells you when something needs a reply. Nothing leaves this machine: tokens stay in
        <span class="mono">data\secrets</span>.
      </p>
      <div class="hero-actions">
        <Button
          label="Connect GitHub"
          icon="pi pi-github"
          size="large"
          :loading="busy"
          :disabled="!app.authLoaded || !!app.authError"
          @click="connect"
        />
        <RouterLink
          to="/connections"
          class="hero-link"
        >
          Other platforms <i class="pi pi-arrow-right" />
        </RouterLink>
      </div>
      <p
        v-if="waiting && !app.githubConnected"
        class="hero-wait"
      >
        <i class="pi pi-spin pi-spinner" /> Continue on github.com in the browser tab that just opened — this page updates by itself.
      </p>
      <p
        v-if="app.authError"
        class="hero-error"
      >
        <i class="pi pi-exclamation-triangle" /> {{ app.authError }}
      </p>
    </div>
    <ol class="steps">
      <li
        v-for="(s, i) in steps"
        :key="s.title"
        class="step"
      >
        <span class="step-num mono">{{ i + 1 }}</span>
        <div>
          <div class="step-title">
            <i :class="s.icon" /> {{ s.title }}
          </div>
          <div class="step-text">
            {{ s.text }}
          </div>
        </div>
      </li>
    </ol>
  </section>
</template>

<style scoped>
.hero {
  position: relative;
  overflow: hidden;
  display: grid;
  grid-template-columns: minmax(0, 1.3fr) minmax(0, 1fr);
  gap: 32px;
  padding: 40px;
  border-radius: var(--iw-radius-lg);
}

.hero-glow {
  position: absolute;
  inset: -40% auto auto -10%;
  width: 60%;
  height: 160%;
  background: radial-gradient(closest-side, var(--iw-primary-soft), transparent);
  pointer-events: none;
}

.hero-main {
  position: relative;
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 14px;
}

.hero-title {
  font-size: 26px;
  line-height: 32px;
  font-weight: 650;
  letter-spacing: -0.01em;
}

.hero-text {
  margin: 0;
  max-width: 560px;
  color: var(--iw-muted);
  font-size: 15px;
}

.hero-actions {
  display: flex;
  align-items: center;
  gap: 20px;
  margin-top: 8px;
}

.hero-link {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-weight: 500;
}

.hero-error {
  margin: 0;
  color: var(--iw-danger);
}

.hero-wait {
  margin: 0;
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--iw-primary);
}

.steps {
  position: relative;
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 12px;
}

.step {
  display: flex;
  gap: 14px;
  padding: 14px 16px;
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  border: 1px solid var(--iw-border);
}

.step-num {
  display: grid;
  place-items: center;
  width: 28px;
  height: 28px;
  flex: none;
  border-radius: 50%;
  font-weight: 700;
  font-size: 13px;
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.step-title {
  font-weight: 600;
  display: flex;
  align-items: center;
  gap: 6px;
}

.step-title i {
  color: var(--iw-muted);
  font-size: 13px;
}

.step-text {
  font-size: 13px;
  color: var(--iw-muted);
}

@media (width <= 1023px) {
  .hero {
    grid-template-columns: 1fr;
    padding: 28px 20px;
  }
}
</style>
