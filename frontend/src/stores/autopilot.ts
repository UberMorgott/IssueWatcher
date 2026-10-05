import { defineStore } from 'pinia'
import { ref, shallowRef } from 'vue'
import { api } from '../api/client'
import type { AutopilotEvent, AutopilotUnread } from '../api/types'

/**
 * Autopilot activity log (docs/AUTOPILOT.md → Activity log): the unread /
 * attention counters of the top bar badge and the latest live event (the log
 * page prepends it; watch with flush 'sync'). Counters come from the API on
 * start and reconnect, then from SSE autopilot.unread (sent after every new
 * event and every mark-read); autopilot.event bumps them until that arrives.
 */
export const useAutopilotStore = defineStore('autopilot', () => {
  const unread = ref(0)
  const attention = ref(0)
  const lastEvent = shallowRef<AutopilotEvent | null>(null)

  async function loadCounts() {
    const r = await api.autopilotEvents({ unreadOnly: true, limit: 1 })
    if (r.ok && r.data) setCounts(r.data)
  }
  function setCounts(c: AutopilotUnread | null | undefined) {
    if (!c) return
    unread.value = Math.max(0, Number(c.unread) || 0)
    attention.value = Math.max(0, Number(c.attention) || 0)
  }
  function onEvent(e: AutopilotEvent | null) {
    if (!e || typeof e.id !== 'number') return
    if (!e.readAt) {
      unread.value++
      if (e.severity === 'attention') attention.value++
    }
    lastEvent.value = e
  }
  /** Marks events read; the server answers (and broadcasts) the new counters. */
  async function markRead(ids: number[]): Promise<boolean> {
    if (!ids.length) return true
    const r = await api.readAutopilotEvents(ids)
    if (r.ok) setCounts(r.data)
    return r.ok
  }

  return { unread, attention, lastEvent, loadCounts, setCounts, onEvent, markRead }
})
