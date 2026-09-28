<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import PlatformIcon from '../components/PlatformIcon.vue'

const { t } = useI18n()

const profiles = computed(() => [
  {
    role: t('agents.coder'),
    agent: 'Claude Code',
    mark: 'claude',
    cli: 'claude -p',
    text: t('agents.coderText'),
    flows: ['fix'],
    phase: t('common.phase', { n: 2 }),
  },
  {
    role: t('agents.responder'),
    agent: 'Codex',
    mark: 'codex',
    cli: 'codex exec',
    text: t('agents.responderText'),
    flows: ['reply', 'label'],
    phase: t('common.phase', { n: 3 }),
  },
])

const pipeline = computed(() =>
  (['issue', 'queue', 'run', 'verify', 'publish'] as const).map((k, i) => ({
    icon: ['pi pi-inbox', 'pi pi-list', 'pi pi-code', 'pi pi-verified', 'pi pi-send'][i],
    title: t(`agents.pipeline.${k}`),
    text: t(`agents.pipeline.${k}Text`),
  })),
)

const prompts = computed(() => [
  { layer: t('agents.global'), text: t('agents.globalText') },
  { layer: t('agents.perProject'), text: t('agents.perProjectText') },
  { layer: t('agents.perFlow'), text: t('agents.perFlowText') },
])
</script>

<template>
  <div class="page">
    <p class="page-intro">
      {{ t('agents.sub') }}
    </p>

    <div class="profiles">
      <article
        v-for="p in profiles"
        :key="p.role"
        class="panel profile"
      >
        <header class="profile-head">
          <PlatformIcon
            :platform="p.mark"
            :size="44"
            tile
          />
          <div>
            <div class="role">
              {{ p.role }}
            </div>
            <div class="agent">
              {{ p.agent }} <span class="mono cli">{{ p.cli }}</span>
            </div>
          </div>
          <span class="phase-badge">{{ p.phase }}</span>
        </header>
        <p class="profile-text">
          {{ p.text }}
        </p>
        <div class="flows">
          <span class="muted">{{ t('agents.flows') }}</span>
          <span
            v-for="f in p.flows"
            :key="f"
            class="flow mono"
          >{{ f }}</span>
        </div>
      </article>
    </div>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">{{ t('agents.howTitle') }}</span>
      </div>
      <div class="panel-body">
        <ol class="pipeline">
          <li
            v-for="(s, i) in pipeline"
            :key="s.title"
            class="stage"
          >
            <span class="stage-icon"><i :class="s.icon" /></span>
            <div class="stage-title">
              {{ s.title }}
            </div>
            <div class="stage-text">
              {{ s.text }}
            </div>
            <i
              v-if="i < pipeline.length - 1"
              class="pi pi-arrow-right arrow"
              aria-hidden="true"
            />
          </li>
        </ol>
      </div>
    </section>

    <section class="panel">
      <div class="panel-head">
        <span class="panel-title">{{ t('agents.prompts') }}</span>
        <span class="phase-badge">{{ t('common.phase', { n: 3 }) }}</span>
      </div>
      <div class="panel-body prompts">
        <div
          v-for="p in prompts"
          :key="p.layer"
          class="prompt"
        >
          <div class="prompt-layer">
            {{ p.layer }}
          </div>
          <div class="muted">
            {{ p.text }}
          </div>
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.profiles {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 20px;
}

.profile {
  padding: 22px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.profile-head {
  display: flex;
  align-items: center;
  gap: 14px;
}

.profile-head .phase-badge {
  margin-left: auto;
  align-self: flex-start;
}

.role {
  font-size: 12px;
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--iw-muted);
}

.agent {
  font-size: 18px;
  font-weight: 650;
}

.cli {
  margin-left: 6px;
  font-size: 12px;
  font-weight: 400;
  color: var(--iw-dimmed);
}

.profile-text {
  margin: 0;
  color: var(--iw-muted);
}

.flows {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 12.5px;
}

.flow {
  padding: 1px 8px;
  border-radius: 6px;
  background: var(--iw-elevated);
  border: 1px solid var(--iw-border);
}

.pipeline {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  grid-template-columns: repeat(5, minmax(0, 1fr));
  gap: 16px;
}

.stage {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 14px;
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  border: 1px dashed var(--iw-border-strong);
}

.stage-icon {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  margin-bottom: 4px;
  border-radius: 9px;
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.stage-title {
  font-weight: 600;
}

.stage-text {
  font-size: 12.5px;
  color: var(--iw-muted);
}

.arrow {
  position: absolute;
  right: -14px;
  top: 50%;
  transform: translateY(-50%);
  font-size: 11px;
  color: var(--iw-dimmed);
}

.prompts {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 16px;
}

.prompt {
  padding: 14px;
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  border: 1px solid var(--iw-border);
}

.prompt-layer {
  font-weight: 600;
  margin-bottom: 4px;
}

@media (width <= 1023px) {
  .profiles,
  .prompts {
    grid-template-columns: minmax(0, 1fr);
  }

  .pipeline {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  .arrow {
    display: none;
  }
}
</style>
