import { createRouter, createWebHistory } from 'vue-router'
import DashboardView from './views/DashboardView.vue'
import ItemView from './views/ItemView.vue'

// History mode: the Go server falls back to index.html for client routes.
export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'dashboard', component: DashboardView },
    { path: '/item/:id', name: 'item', component: ItemView, props: true },
  ],
})
