import { createRouter, createWebHistory } from 'vue-router'

// History mode: the Go server falls back to index.html for client routes.
// /item/:id is the deep link used by tray notifications.
export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'overview', component: () => import('./views/OverviewView.vue'), meta: { title: 'Overview' } },
    { path: '/issues', name: 'issues', component: () => import('./views/IssuesView.vue'), meta: { title: 'Issues' } },
    { path: '/item/:id', name: 'item', component: () => import('./views/ItemView.vue'), props: true, meta: { title: 'Issue' } },
    { path: '/projects', name: 'projects', component: () => import('./views/ProjectsView.vue'), meta: { title: 'Projects' } },
    { path: '/agents', name: 'agents', component: () => import('./views/AgentsView.vue'), meta: { title: 'Agents' } },
    { path: '/connections', name: 'connections', component: () => import('./views/ConnectionsView.vue'), meta: { title: 'Connections' } },
    { path: '/settings', name: 'settings', component: () => import('./views/SettingsView.vue'), meta: { title: 'Settings' } },
    { path: '/:pathMatch(.*)*', name: 'not-found', component: () => import('./views/NotFoundView.vue'), meta: { title: 'Not found' } },
  ],
  scrollBehavior: () => ({ top: 0 }),
})

router.afterEach((to) => {
  const t = to.meta.title as string | undefined
  document.title = t ? `${t} · IssueWatcher` : 'IssueWatcher'
})
