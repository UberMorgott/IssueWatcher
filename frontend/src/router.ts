import { createRouter, createWebHistory, type RouteLocationNormalizedLoaded } from 'vue-router'
import { t } from './i18n'

// History mode: the Go server falls back to index.html for client routes.
// /item/:id is the deep link used by tray notifications.
// meta.title = key under `title.*` in src/i18n.
export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'overview', component: () => import('./views/OverviewView.vue'), meta: { title: 'overview' } },
    { path: '/issues', name: 'issues', component: () => import('./views/IssuesView.vue'), meta: { title: 'issues' } },
    { path: '/item/:id', name: 'item', component: () => import('./views/ItemView.vue'), props: true, meta: { title: 'item' } },
    { path: '/projects', name: 'projects', component: () => import('./views/ProjectsView.vue'), meta: { title: 'projects' } },
    // Agent jobs; /jobs/:id is the deep link of the tray's «agent finished» card.
    { path: '/jobs', name: 'jobs', component: () => import('./views/JobsView.vue'), meta: { title: 'jobs' } },
    { path: '/jobs/:id', name: 'job', component: () => import('./views/JobView.vue'), props: true, meta: { title: 'job' } },
    { path: '/agents', redirect: '/settings/agents' },
    { path: '/connections', name: 'connections', component: () => import('./views/ConnectionsView.vue'), meta: { title: 'connections' } },
    // Deep links: /settings/<section> (general, appearance, notifications, sync, …).
    { path: '/settings/:section?', name: 'settings', component: () => import('./views/SettingsView.vue'), props: true, meta: { title: 'settings' } },
    { path: '/:pathMatch(.*)*', name: 'not-found', component: () => import('./views/NotFoundView.vue'), meta: { title: 'notFound' } },
  ],
  scrollBehavior: () => ({ top: 0 }),
})

/** Localised page title of a route ("" when it has none). */
export function routeTitle(r: RouteLocationNormalizedLoaded): string {
  const k = r.meta.title as string | undefined
  return k ? t('title.' + k) : ''
}

/**
 * "IssueWatcher · <page>": the prefix is the locale-independent marker the tray
 * uses to find this tab's browser window (internal/notify IsDashboardTitle).
 */
export function updateDocumentTitle() {
  document.title = 'IssueWatcher · ' + (routeTitle(router.currentRoute.value) || 'IssueWatcher')
}

router.afterEach(() => updateDocumentTitle())
