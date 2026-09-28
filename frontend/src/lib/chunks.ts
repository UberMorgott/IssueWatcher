import { ref, shallowRef } from 'vue'
import type { Result } from '../api/client'

/** One keyset chunk from the API (items, cursor of the last row, more to come). */
export interface Chunk<T> {
  items: T[]
  nextCursor: string
  more: boolean
  total?: number
  headCursor?: string
}

/**
 * Keyset "load more" list: chunks append in order, never all at once, one
 * request in flight, stale responses (after reset) dropped. Rows are deduped by
 * id so a row that moved while scrolling cannot appear twice.
 */
export function useChunks<T extends { id: number | string }>(fetch: (cursor: string) => Promise<Result<Chunk<T>>>) {
  const items = shallowRef<T[]>([])
  const total = ref<number | null>(null)
  const loading = ref(false)
  const done = ref(false)
  const error = ref('')
  const status = ref(0)
  /** headCursor of the first chunk (newest row) for head refreshes. */
  const head = ref('')
  let cursor = ''
  let gen = 0
  let inflight: Promise<void> | null = null

  async function run(g: number) {
    const r = await fetch(cursor)
    if (g !== gen) return
    loading.value = false
    inflight = null
    if (!r.ok) {
      error.value = r.error
      status.value = r.status
      return
    }
    error.value = ''
    const known = new Set(items.value.map((it) => it.id))
    const fresh = r.data.items.filter((it) => !known.has(it.id))
    if (!items.value.length && r.data.headCursor) head.value = r.data.headCursor
    items.value = items.value.concat(fresh)
    if (r.data.total !== undefined) total.value = r.data.total
    if (r.data.nextCursor) cursor = r.data.nextCursor
    done.value = !r.data.more
  }

  /** Loads the next chunk (no-op while loading or when everything is loaded). */
  function loadMore(): Promise<void> {
    if (inflight) return inflight
    if (done.value) return Promise.resolve()
    loading.value = true
    inflight = run(gen)
    return inflight
  }

  /** Looks for rows added after the last one (lists that grow at the tail). */
  function loadTail(): Promise<void> {
    if (inflight) return inflight
    done.value = false
    return loadMore()
  }

  function reset() {
    gen++
    inflight = null
    items.value = []
    total.value = null
    head.value = ''
    cursor = ''
    done.value = false
    loading.value = false
    error.value = ''
    status.value = 0
  }

  return { items, total, loading, done, error, status, head, loadMore, loadTail, reset }
}
