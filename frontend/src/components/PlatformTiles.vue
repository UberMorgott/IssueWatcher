<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import PlatformIcon from './PlatformIcon.vue'
import { useAppStore } from '../stores/app'
import { platformName } from '../lib/platforms'

// Overview: one tile per mod platform with its real counts (mod pages, open
// items, unread) and account state; a switched-off platform links to its card.
const app = useAppStore()
const { t } = useI18n()

const tiles = computed(() =>
  (['nexus', 'curseforge', 'steam'] as const).map((id) => {
    const repos = app.repos.filter((r) => r.platform === id)
    const st = app.platforms.find((p) => p.id === id)
    return {
      id,
      name: platformName(id),
      projects: repos.length,
      open: repos.reduce((n, r) => n + r.open, 0),
      unread: repos.reduce((n, r) => n + r.unread, 0),
      state: st?.state ?? 'disabled',
      readOnly: st?.state === 'connected' && st.session === 'none',
      on: !!st?.enabled || repos.length > 0,
    }
  }),
)
</script>

<template>
  <div class="platforms">
    <RouterLink
      v-for="p in tiles"
      :key="p.id"
      :to="p.on ? { name: 'issues', query: { source: p.id } } : '/settings/connections'"
      class="tile panel"
      :class="{ off: !p.on }"
    >
      <PlatformIcon
        :platform="p.id"
        :size="36"
        tile
      />
      <div class="t-main">
        <div class="t-name">
          {{ p.name }}
        </div>
        <div
          v-if="p.on"
          class="t-facts"
        >
          <span><b class="mono">{{ p.open }}</b> {{ t('platforms.items', p.open) }}</span>
          <span v-if="p.unread"><span class="unread-dot" /> <b class="mono">{{ p.unread }}</b> {{ t('platforms.unread', p.unread) }}</span>
          <span class="muted"><b class="mono">{{ p.projects }}</b> {{ t('platforms.projects', p.projects) }}</span>
        </div>
        <div
          v-else
          class="muted t-facts"
        >
          {{ t('platforms.off') }}
        </div>
      </div>
      <span
        class="t-state"
        :class="{ ok: p.on && p.state === 'connected' && !p.readOnly, bad: p.on && ['relogin', 'error', 'unavailable', 'signed_out'].includes(p.state) }"
      >{{ !p.on ? t('platforms.setUp') : p.readOnly ? t('platforms.readOnlyShort') : t('platforms.state.' + p.state) }}</span>
    </RouterLink>
  </div>
</template>

<style scoped>
.platforms {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 16px;
}

.tile {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 16px 18px;
  color: var(--iw-text);
}

.tile:hover {
  border-color: var(--iw-border-strong);
  color: var(--iw-text);
}

.tile.off {
  opacity: 0.8;
}

.t-main {
  flex: 1;
  min-width: 0;
}

.t-name {
  font-weight: 600;
}

.t-facts {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px 12px;
  font-size: calc(12.5px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.t-facts b {
  color: var(--iw-text);
}

.t-state {
  align-self: flex-start;
  font-size: calc(11.5px * var(--iw-fs, 1));
  color: var(--iw-muted);
  white-space: nowrap;
}

.t-state.ok {
  color: var(--iw-success);
}

.t-state.bad {
  color: var(--iw-danger);
}

@media (width <= 899px) {
  .platforms {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
