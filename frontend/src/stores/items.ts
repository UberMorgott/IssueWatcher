import { defineStore } from 'pinia'
import { ref } from 'vue'

export interface Item {
  id: number
  project: string
  number: number
  title: string
  status: 'open' | 'closed'
  updated: string
}

// Mock rows until GitHub sync (Phase 1) fills the store from Go.
export const useItemsStore = defineStore('items', () => {
  const items = ref<Item[]>([
    { id: 1, project: 'UberMorgott/morgue', number: 12, title: 'Crash on IL2CPP metadata v31', status: 'open', updated: '2026-09-27' },
    { id: 2, project: 'UberMorgott/morgue', number: 15, title: 'Tray icon blurry at 150% DPI', status: 'open', updated: '2026-09-26' },
    { id: 3, project: 'UberMorgott/pult', number: 4, title: 'Portable mode writes to AppData', status: 'closed', updated: '2026-09-20' },
    { id: 4, project: 'UberMorgott/quality-gate', number: 88, title: 'Gate misses vue-tsc errors', status: 'open', updated: '2026-09-25' },
    { id: 5, project: 'UberMorgott/aegis-engine', number: 31, title: 'Stale symbol after rename', status: 'open', updated: '2026-09-24' },
  ])

  const byId = (id: number) => items.value.find((i) => i.id === id)

  return { items, byId }
})
