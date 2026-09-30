<script setup lang="ts">
import { computed, ref } from 'vue'
import Button from 'primevue/button'
import { useI18n } from 'vue-i18n'
import PlatformIcon from './PlatformIcon.vue'
import { useAppStore } from '../stores/app'
import { useSettingsStore } from '../stores/settings'
import { PLATFORMS } from '../lib/platforms'

// First run: nothing connected yet. GitHub is one click here; the mod platforms
// (Nexus, CurseForge, Factorio, Steam) connect on the Connections page.
const app = useAppStore()
const settings = useSettingsStore()
const secretsDir = computed(() => (settings.doc?.info.dataDir ? settings.doc.info.dataDir + '\\secrets' : 'data\\secrets'))
const { t } = useI18n()
const busy = ref(false)
const waiting = ref(false)

async function connect() {
  busy.value = true
  waiting.value = await app.connect('github')
  busy.value = false
}

const steps = computed(() => [
  { icon: 'pi pi-link', title: t('hero.steps.connectTitle'), text: t('hero.steps.connectText') },
  { icon: 'pi pi-sync', title: t('hero.steps.syncTitle'), text: t('hero.steps.syncText') },
  { icon: 'pi pi-inbox', title: t('hero.steps.reviewTitle'), text: t('hero.steps.reviewText') },
])
</script>

<template>
  <section class="hero panel">
    <div class="hero-glow" />
    <div class="hero-main">
      <div class="hero-icons">
        <PlatformIcon
          v-for="p in PLATFORMS"
          :key="p"
          :platform="p"
          :size="44"
          tile
        />
      </div>
      <h2 class="hero-title">
        {{ t('hero.title') }}
      </h2>
      <i18n-t
        keypath="hero.text"
        tag="p"
        class="hero-text"
        scope="global"
      >
        <template #path>
          <span class="mono">{{ secretsDir }}</span>
        </template>
      </i18n-t>
      <div class="hero-actions">
        <Button
          :label="t('common.connectGithub')"
          icon="pi pi-github"
          size="large"
          :loading="busy"
          :disabled="!app.authLoaded || !app.github"
          @click="connect"
        />
        <Button
          as="router-link"
          to="/connections"
          :label="t('hero.otherPlatforms')"
          icon="pi pi-link"
          size="large"
          severity="secondary"
          outlined
        />
      </div>
      <p
        v-if="waiting && !app.githubConnected"
        class="hero-wait"
      >
        <i class="pi pi-spin pi-spinner" /> {{ t('common.continueOnGithub') }}
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
  font-size: calc(26px * var(--iw-fs, 1));
  line-height: 32px;
  font-weight: 650;
  letter-spacing: -0.01em;
}

.hero-text {
  margin: 0;
  max-width: 560px;
  color: var(--iw-muted);
  font-size: calc(15px * var(--iw-fs, 1));
}

.hero-actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 8px;
}

.hero-icons {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
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
  font-size: calc(13px * var(--iw-fs, 1));
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
  font-size: calc(13px * var(--iw-fs, 1));
}

.step-text {
  font-size: calc(13px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

@media (width <= 1023px) {
  .hero {
    grid-template-columns: 1fr;
    padding: 28px 20px;
  }
}
</style>
