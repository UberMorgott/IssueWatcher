<script setup lang="ts">
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { useJobsStore } from '../stores/jobs'
import { currentItemKind } from '../lib/currentItem'

defineProps<{ mobile?: boolean }>()
const emit = defineEmits<{ navigate: [] }>()
const app = useAppStore()
const jobs = useJobsStore()
const route = useRoute()
const { t } = useI18n()

const items: { to: string; icon: string; label: string; match: string[]; badge?: () => number }[] = [
  { to: '/', icon: 'pi pi-objects-column', label: 'nav.overview', match: ['overview'] },
  { to: '/issues', icon: 'pi pi-inbox', label: 'nav.issues', match: ['issues', 'item'], badge: () => app.unreadIssues },
  { to: '/comments', icon: 'pi pi-comments', label: 'nav.comments', match: ['comments', 'item:comment'], badge: () => app.unreadComments },
  { to: '/projects', icon: 'pi pi-folder', label: 'nav.projects', match: ['projects'] },
  { to: '/jobs', icon: 'pi pi-microchip-ai', label: 'nav.jobs', match: ['jobs', 'job'], badge: () => jobs.activeCount },
  { to: '/runs', icon: 'pi pi-send', label: 'nav.releases', match: ['runs', 'run'] },
  { to: '/connections', icon: 'pi pi-link', label: 'nav.connections', match: ['connections'] },
  { to: '/settings', icon: 'pi pi-cog', label: 'nav.settings', match: ['settings'] },
]

// The item page belongs to Comments for a mod page thread, to Issues otherwise.
const active = (match: string[]) => {
  const name = String(route.name)
  if (name === 'item' && currentItemKind.value === 'comment') return match.includes('item:comment')
  return match.includes(name)
}
</script>

<template>
  <nav
    class="sidebar"
    :class="{ collapsed: app.sidebarCollapsed && !mobile }"
    :aria-label="t('nav.main')"
  >
    <div class="brand">
      <RouterLink
        to="/"
        class="logo"
        tabindex="-1"
        aria-hidden="true"
        @click="emit('navigate')"
      >
        <i class="pi pi-eye" />
      </RouterLink>
      <div class="brand-titles">
        <RouterLink
          to="/"
          class="brand-text"
          @click="emit('navigate')"
        >
          IssueWatcher
        </RouterLink>
        <RouterLink
          v-if="app.version"
          to="/settings/updates"
          class="version mono"
          :title="`${app.version} · ${t('nav.versionTip')}`"
          @click="emit('navigate')"
        >
          <span class="version-text">{{ app.version }}</span>
          <span
            v-if="app.updateAvailable"
            class="update-dot"
            :aria-label="t('nav.updateAvailable')"
          />
        </RouterLink>
      </div>
      <button
        v-if="!mobile"
        v-tooltip.right="app.sidebarCollapsed ? t('nav.expandTip') : t('nav.collapseTip')"
        type="button"
        class="toggle"
        :aria-label="app.sidebarCollapsed ? t('nav.expand') : t('nav.collapse')"
        :aria-expanded="!app.sidebarCollapsed"
        @click="app.toggleSidebar()"
      >
        <i :class="app.sidebarCollapsed ? 'pi pi-angle-double-right' : 'pi pi-angle-double-left'" />
      </button>
    </div>

    <ul class="nav">
      <li
        v-for="it in items"
        :key="it.to"
      >
        <RouterLink
          v-tooltip.right="app.sidebarCollapsed && !mobile ? t(it.label) : undefined"
          :to="it.to"
          class="nav-item"
          :class="{ active: active(it.match) }"
          :aria-current="active(it.match) ? 'page' : undefined"
          @click="emit('navigate')"
        >
          <i :class="it.icon" />
          <span class="nav-label">{{ t(it.label) }}</span>
          <span
            v-if="it.badge && it.badge() > 0"
            class="nav-badge mono"
          >{{ it.badge() > 99 ? '99+' : it.badge() }}</span>
        </RouterLink>
      </li>
    </ul>
  </nav>
</template>

<style scoped>
/* Open/close: the width animates with an ease-out curve; labels fade instead of
   popping, and only the sidebar lays out (contain) while it moves. */
.sidebar {
  --ease-out: cubic-bezier(0.2, 0.8, 0.2, 1);

  display: flex;
  flex-direction: column;
  width: var(--iw-sidebar);
  height: 100%;
  padding: 16px 12px;
  gap: 8px;
  background: var(--iw-surface);
  border-right: 1px solid var(--iw-border);
  transition: width 180ms var(--ease-out);
  overflow: hidden;
  contain: layout paint;
}

.sidebar.collapsed {
  width: var(--iw-sidebar-collapsed);
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  min-height: 40px;
  padding: 0 0 0 6px;
  margin-bottom: 12px;
}

/* Name and version stacked next to the logo, so the full version fits on its own line. */
.brand-titles {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  flex: 1 1 auto;
  min-width: 0;
}

.brand-text {
  color: var(--iw-text);
  font-weight: 650;
  font-size: calc(16px * var(--iw-fs, 1));
  line-height: 1.2;
  letter-spacing: -0.01em;
  white-space: nowrap;
}

.logo {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  flex: none;
  border-radius: 10px;
  color: var(--iw-on-primary);
  background: linear-gradient(135deg, var(--iw-primary), #b58cfa);
  box-shadow: 0 4px 14px rgb(134 165 255 / 30%);
}

.version {
  display: flex;
  align-items: center;
  gap: 5px;
  max-width: 100%; /* only an extreme version shrinks (ellipsis, full text in the tooltip) */
  min-width: 0;
  font-size: calc(11px * var(--iw-fs, 1));
  line-height: 1.3;
  color: var(--iw-dimmed);
  white-space: nowrap;
}

.version-text {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
}

.update-dot {
  flex: none;
}

.version:hover {
  color: var(--iw-text);
}

.update-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--iw-primary);
}

.toggle {
  display: grid;
  place-items: center;
  width: 28px;
  height: 28px;
  flex: none;
  margin-left: auto;
  padding: 0;
  border: 0;
  border-radius: var(--iw-radius-sm);
  background: transparent;
  color: var(--iw-dimmed);
  cursor: pointer;
  transition: background 140ms ease, color 140ms ease;
}

.toggle i {
  font-size: calc(13px * var(--iw-fs, 1));
}

.toggle:hover {
  background: var(--iw-hover);
  color: var(--iw-text);
}

/* Collapsed: the toggle covers the logo on hover/focus, so the header keeps its
   height and the nav never jumps. */
.brand {
  position: relative;
}

.collapsed .toggle {
  position: absolute;
  left: 6px;
  top: 4px;
  width: 32px;
  height: 32px;
  border-radius: 10px;
  background: var(--iw-elevated);
  color: var(--iw-text);
  opacity: 0;
}

.collapsed .brand:hover .toggle,
.collapsed .toggle:focus-visible {
  opacity: 1;
}

.nav {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.nav-item {
  position: relative;
  display: flex;
  align-items: center;
  gap: 12px;
  width: 100%;
  height: 40px;
  padding: 0 12px;
  border-radius: var(--iw-radius-sm);
  color: var(--iw-muted);
  font-weight: 500;
  white-space: nowrap;
  transition: background 140ms ease, color 140ms ease;
}

.nav-item i {
  font-size: calc(16px * var(--iw-fs, 1));
  width: 20px;
  text-align: center;
  flex: none;
}

.nav-item:hover {
  background: var(--iw-hover);
  color: var(--iw-text);
}

.nav-item.active {
  background: var(--iw-primary-soft);
  color: var(--iw-text);
}

.nav-item.active::before {
  content: '';
  position: absolute;
  left: -12px;
  top: 8px;
  bottom: 8px;
  width: 3px;
  border-radius: 0 3px 3px 0;
  background: var(--iw-primary);
}

.nav-item.active i {
  color: var(--iw-primary);
}

.nav-badge {
  margin-left: auto;
  min-width: 22px;
  padding: 1px 7px;
  border-radius: 999px;
  font-size: calc(11px * var(--iw-fs, 1));
  font-weight: 600;
  text-align: center;
  color: var(--iw-on-primary);
  background: var(--iw-primary);
}

.nav-label,
.brand-text,
.version {
  transition: opacity 140ms var(--ease-out);
}

.collapsed .nav-label,
.collapsed .brand-text {
  opacity: 0;
}

.collapsed .version {
  display: none;
}

.collapsed .nav-badge {
  position: absolute;
  top: 4px;
  left: 26px;
  min-width: 0;
  padding: 0 5px;
  font-size: calc(10px * var(--iw-fs, 1));
}
</style>