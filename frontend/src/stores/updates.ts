import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api } from '../api/client'
import { expectRestart } from '../api/live'
import type { UpdateStatus } from '../api/types'
import { useAppStore } from './app'

/**
 * Self-update state (GET /api/update + update.status live events). The app
 * restarts itself after an install: the event stream reconnects to the new
 * version and App.vue reloads the page on the same route.
 */
export const useUpdatesStore = defineStore('updates', () => {
  const status = ref<UpdateStatus | null>(null)
  const error = ref('')
  const busy = computed(() => !!status.value && status.value.state !== 'idle')

  function apply(s: UpdateStatus) {
    status.value = s
    useAppStore().updateAvailable = s.updateAvailable && !s.devBuild
    if (s.state === 'restarting' || s.state === 'installing') expectRestart()
  }

  async function load() {
    const r = await api.updateStatus()
    if (r.ok) apply(r.data)
  }

  async function check() {
    error.value = ''
    const r = await api.updateCheck()
    if (r.ok) apply(r.data)
    else {
      error.value = r.error
      await load()
    }
  }

  async function install() {
    error.value = ''
    const r = await api.updateInstall()
    if (!r.ok) error.value = r.error
    await load()
  }

  return { status, error, busy, apply, load, check, install }
})
