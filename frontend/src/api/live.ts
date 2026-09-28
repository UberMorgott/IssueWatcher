import { ref } from 'vue'

/** Live event names served by GET /api/events (internal/api/events.go). */
export const LIVE_EVENTS = [
  'item.new',
  'comment.new',
  'item.closed',
  'sync.status',
  'data.changed',
  'auth.changed',
  'settings.changed',
  'navigate',
  'superseded',
  'update.status',
  'job.changed',
  'job.log',
] as const
export type LiveEventName = (typeof LIVE_EVENTS)[number]

export type LiveHandler = (name: LiveEventName, data: unknown) => void

const MIN_DELAY = 1000
// Loopback only: retrying is free, and a tab must be back within the app's
// start grace (internal/api StartGrace, 3 s) so a restart opens no duplicate tab.
const MAX_DELAY = 2500

/** True while the event stream is open. */
export const liveConnected = ref(false)

/** While the app restarts for an update, reconnect quickly instead of backing off. */
const RESTART_DELAY = 500
let restartUntil = 0
export function expectRestart(ms = 90000) {
  restartUntil = Date.now() + ms
}

/**
 * Connects to the SSE stream and keeps it connected: on error the source is
 * closed and reopened with exponential backoff (1 s → 2.5 s, reset on open).
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
      if (Date.now() < restartUntil) {
        timer = window.setTimeout(open, RESTART_DELAY)
        return
      }
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
