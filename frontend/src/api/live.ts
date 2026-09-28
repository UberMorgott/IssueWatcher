import { ref } from 'vue'

/** Live event names served by GET /api/events (internal/api/events.go). */
export const LIVE_EVENTS = ['item.new', 'comment.new', 'item.closed', 'sync.status', 'data.changed', 'auth.changed', 'navigate'] as const
export type LiveEventName = (typeof LIVE_EVENTS)[number]

export type LiveHandler = (name: LiveEventName, data: unknown) => void

const MIN_DELAY = 1000
const MAX_DELAY = 30000

/** True while the event stream is open. */
export const liveConnected = ref(false)

/**
 * Connects to the SSE stream and keeps it connected: on error the source is
 * closed and reopened with exponential backoff (1 s → 30 s, reset on open).
 * `onReconnect` fires after a reconnect so pages can reload what they missed.
 */
export function connectLive(handler: LiveHandler, onReconnect: () => void): () => void {
  let es: EventSource | null = null
  let delay = MIN_DELAY
  let timer: number | undefined
  let stopped = false
  let everOpened = false

  const open = () => {
    if (stopped) return
    es = new EventSource('/api/events')
    es.onopen = () => {
      liveConnected.value = true
      delay = MIN_DELAY
      if (everOpened) onReconnect()
      everOpened = true
    }
    es.onerror = () => {
      liveConnected.value = false
      es?.close()
      es = null
      if (stopped) return
      timer = window.setTimeout(open, delay)
      delay = Math.min(delay * 2, MAX_DELAY)
    }
    for (const name of LIVE_EVENTS) {
      es.addEventListener(name, (ev) => {
        let data: unknown = null
        try {
          data = JSON.parse((ev as MessageEvent<string>).data)
        } catch {
          /* ignore malformed payload */
        }
        handler(name, data)
      })
    }
  }
  open()

  return () => {
    stopped = true
    window.clearTimeout(timer)
    es?.close()
    liveConnected.value = false
  }
}
