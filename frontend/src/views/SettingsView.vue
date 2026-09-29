<script setup lang="ts">
import { computed, defineAsyncComponent, defineComponent, h, type AsyncComponentLoader, type Component } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import Skeleton from 'primevue/skeleton'
import { useSettingsStore } from '../stores/settings'
import { useCrumbs } from '../lib/crumbs'
import EmptyState from '../components/EmptyState.vue'

const props = defineProps<{ section?: string }>()
const { t } = useI18n()
const router = useRouter()
const settings = useSettingsStore()

/** Placeholder while the settings document or a section's chunk loads (never a blank body). */
const SectionSkeleton = defineComponent({
  render: () =>
    h('div', { style: 'display:flex;flex-direction:column;gap:16px' }, [
      h(Skeleton, { height: '36px', width: '40%' }),
      ...[1, 2, 3].map((i) => h(Skeleton, { key: i, height: '120px', borderRadius: '12px' })),
    ]),
})
const lazySection = (loader: AsyncComponentLoader) => defineAsyncComponent({ loader, loadingComponent: SectionSkeleton, delay: 0 })

// Secondary menu (docs/ARCHITECTURE.md → Settings). Deep link: /settings/<id>.
const SECTIONS: { id: string; icon: string; view: Component }[] = [
  { id: 'general', icon: 'pi pi-sliders-h', view: lazySection(() => import('./settings/GeneralSection.vue')) },
  { id: 'appearance', icon: 'pi pi-palette', view: lazySection(() => import('./settings/AppearanceSection.vue')) },
  { id: 'notifications', icon: 'pi pi-bell', view: lazySection(() => import('./settings/NotificationsSection.vue')) },
  { id: 'sync', icon: 'pi pi-sync', view: lazySection(() => import('./settings/SyncSection.vue')) },
  { id: 'connections', icon: 'pi pi-link', view: lazySection(() => import('./ConnectionsView.vue')) },
  { id: 'projects', icon: 'pi pi-folder', view: lazySection(() => import('./settings/ProjectsSection.vue')) },
  { id: 'agents', icon: 'pi pi-microchip-ai', view: lazySection(() => import('./settings/AgentsSection.vue')) },
  { id: 'updates', icon: 'pi pi-cloud-download', view: lazySection(() => import('./settings/UpdatesSection.vue')) },
  { id: 'advanced', icon: 'pi pi-wrench', view: lazySection(() => import('./settings/AdvancedSection.vue')) },
]

const current = computed(() => SECTIONS.find((s) => s.id === props.section) ?? SECTIONS[0])
if (props.section && !SECTIONS.some((s) => s.id === props.section)) void router.replace('/settings/general')

useCrumbs(() => [
  { label: t('nav.settings'), to: '/settings' },
  { label: t('settings.sections.' + current.value.id) },
])
</script>

<template>
  <div class="page settings">
    <nav
      class="side"
      :aria-label="t('nav.settings')"
    >
      <RouterLink
        v-for="s in SECTIONS"
        :key="s.id"
        :to="'/settings/' + s.id"
        class="side-link"
        :class="{ active: current.id === s.id }"
        :aria-current="current.id === s.id ? 'page' : undefined"
      >
        <i :class="s.icon" />
        <span>{{ t('settings.sections.' + s.id) }}</span>
      </RouterLink>
    </nav>
    <div class="body">
      <EmptyState
        v-if="!settings.doc && !settings.available"
        icon="pi pi-exclamation-triangle"
        :title="t('settings.unavailable')"
        :text="t('settings.unavailableText')"
      />
      <!-- First load only: the settings store keeps the document for later visits. -->
      <SectionSkeleton v-else-if="!settings.doc" />
      <component
        :is="current.view"
        v-else
        :key="current.id"
        v-bind="current.id === 'connections' ? { embedded: true } : {}"
      />
    </div>
  </div>
</template>

<style scoped>
.settings {
  display: grid;
  grid-template-columns: 220px minmax(0, 1fr);
  align-items: start;
  gap: 28px;
}

.side {
  position: sticky;
  top: calc(var(--iw-topbar) + 24px);
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.side-link {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 12px;
  border-radius: var(--iw-radius-sm);
  color: var(--iw-muted);
  font-weight: 500;
}

.side-link:hover {
  color: var(--iw-text);
  background: var(--iw-hover);
}

.side-link.active {
  color: var(--iw-text);
  background: var(--iw-primary-soft);
}

.side-link.active i {
  color: var(--iw-primary);
}

.body {
  min-width: 0;
  max-width: 920px;
  display: flex;
  flex-direction: column;
  gap: 20px;
}

@media (width <= 899px) {
  .settings {
    grid-template-columns: minmax(0, 1fr);
  }

  .side {
    position: static;
    flex-direction: row;
    overflow-x: auto;
    padding-bottom: 4px;
  }

  .side-link {
    white-space: nowrap;
  }
}
</style>
