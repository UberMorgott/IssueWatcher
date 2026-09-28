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
    { path: '/agents', name: 'agents', component: () => import('./views/AgentsView.vue'), meta: { title: 'agents' } },
    { path: '/connections', name: 'connections', component: () => import('./views/ConnectionsView.vue'), meta: { title: 'connections' } },
    { path: '/settings', name: 'settings', component: () => import('./views/SettingsView.vue'), meta: { title: 'settings' } },
    { path: '/:pathMatch(.*)*', name: 'not-found', component: () => import('./views/NotFoundView.vue'), meta: { title: 'notFound' } },
  ],
  scrollBehavior: () => ({ top: 0 }),
})

/** Localised page title of a route ("" when it has none). */
export function routeTitle(r: RouteLocationNormalizedLoaded): string {
  const k = r.meta.title as string | undefined
  return k ? t('title.' + k) : ''
}

export function updateDocumentTitle() {
  const title = routeTitle(router.currentRoute.value)
  document.title = title ? `${title} · IssueWatcher` : 'IssueWatcher'
}

router.afterEach(() => updateDocumentTitle())
