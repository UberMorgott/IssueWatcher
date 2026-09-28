<script setup lang="ts">
import { useRoute } from 'vue-router'
import { useAppStore } from '../stores/app'

defineProps<{ mobile?: boolean }>()
const emit = defineEmits<{ navigate: [] }>()
const app = useAppStore()
const route = useRoute()

const items = [
  { to: '/', icon: 'pi pi-objects-column', label: 'Overview', match: ['overview'] },
  { to: '/issues', icon: 'pi pi-inbox', label: 'Issues', match: ['issues', 'item'], badge: true },
  { to: '/projects', icon: 'pi pi-folder', label: 'Projects', match: ['projects'] },
  { to: '/agents', icon: 'pi pi-microchip-ai', label: 'Agents', match: ['agents'], soon: true },
  { to: '/connections', icon: 'pi pi-link', label: 'Connections', match: ['connections'] },
  { to: '/settings', icon: 'pi pi-cog', label: 'Settings', match: ['settings'] },
]

const active = (match: string[]) => match.includes(String(route.name))
</script>

<template>
  <nav
    class="sidebar"
    :class="{ collapsed: app.sidebarCollapsed && !mobile }"
    aria-label="Main"
  >
    <RouterLink
      to="/"
      class="brand"
      @click="emit('navigate')"
    >
      <span class="logo"><i class="pi pi-eye" /></span>
      <span class="brand-text">IssueWatcher</span>
    </RouterLink>

    <ul class="nav">
      <li
        v-for="it in items"
        :key="it.to"
      >
        <RouterLink
          v-tooltip.right="app.sidebarCollapsed && !mobile ? it.label : undefined"
          :to="it.to"
          class="nav-item"
          :class="{ active: active(it.match) }"
          :aria-current="active(it.match) ? 'page' : undefined"
          @click="emit('navigate')"
        >
          <i :class="it.icon" />
          <span class="nav-label">{{ it.label }}</span>
          <span
            v-if="it.badge && app.unreadTotal > 0"
            class="nav-badge mono"
          >{{ app.unreadTotal > 99 ? '99+' : app.unreadTotal }}</span>
          <span
            v-else-if="it.soon"
            class="nav-soon"
          >soon</span>
        </RouterLink>
      </li>
    </ul>

    <div class="foot">
      <span
        v-if="app.version"
        class="version mono"
      >{{ app.version }}</span>
      <button
        v-if="!mobile"
        v-tooltip.right="app.sidebarCollapsed ? 'Expand sidebar  [' : undefined"
        type="button"
        class="collapse"
        :aria-label="app.sidebarCollapsed ? 'Expand sidebar' : 'Collapse sidebar'"
        @click="app.toggleSidebar()"
      >
        <i :class="app.sidebarCollapsed ? 'pi pi-angle-double-right' : 'pi pi-angle-double-left'" />
        <span class="nav-label">Collapse</span>
      </button>
    </div>
  </nav>
</template>

<style scoped>
.sidebar {
  display: flex;
  flex-direction: column;
  width: var(--iw-sidebar);
  height: 100%;
  padding: 16px 12px;
  gap: 8px;
  background: var(--iw-surface);
  border-right: 1px solid var(--iw-border);
  transition: width 160ms ease;
  overflow: hidden;
}

.sidebar.collapsed {
  width: var(--iw-sidebar-collapsed);
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  height: 40px;
  padding: 0 6px;
  margin-bottom: 12px;
  color: var(--iw-text);
  font-weight: 650;
  font-size: 16px;
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

.nav {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.nav-item,
.collapse {
  position: relative;
  display: flex;
  align-items: center;
  gap: 12px;
  width: 100%;
  height: 40px;
  padding: 0 12px;
  border: 0;
  border-radius: var(--iw-radius-sm);
  background: transparent;
  color: var(--iw-muted);
  font: inherit;
  font-weight: 500;
  white-space: nowrap;
  cursor: pointer;
  transition: background 140ms ease, color 140ms ease;
}

.nav-item i,
.collapse i {
  font-size: 16px;
  width: 20px;
  text-align: center;
  flex: none;
}

.nav-item:hover,
.collapse:hover {
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
  font-size: 11px;
  font-weight: 600;
  text-align: center;
  color: var(--iw-on-primary);
  background: var(--iw-primary);
}

.nav-soon {
  margin-left: auto;
  font-size: 11px;
  color: var(--iw-dimmed);
}

.collapsed .nav-label,
.collapsed .brand-text,
.collapsed .nav-soon,
.collapsed .version {
  display: none;
}

.collapsed .nav-badge {
  position: absolute;
  top: 4px;
  right: 2px;
  min-width: 0;
  padding: 0 5px;
  font-size: 10px;
}

.foot {
  margin-top: auto;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.version {
  padding: 0 12px;
  font-size: 11px;
  color: var(--iw-dimmed);
}
</style>
