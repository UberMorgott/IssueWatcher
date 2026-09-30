import { ref, shallowRef } from 'vue'
import type { Result } from '../api/client'
import { listCache } from './cache'

/** What a list showed last under one cache key (lists keep at most LIST_CACHE_ROWS rows). */
interface Snapshot<T> {
  items: T[]
  total: number | null
  counts: ChunkCounts | null
  head: string
}
const LIST_CACHE_ROWS = 200

/** One keyset chunk from the API (items, cursor of the last row, more to come). */
export interface Chunk<T> {
  items: T[]
  nextCursor: string
  more: boolean
  total?: number
  headCursor?: string
  /** Header counters (issue list, first chunk). */
  counts?: ChunkCounts
}

export interface ChunkCounts {
  open: number
  closed: number
  unread: number
  /** Comment threads marked «Решено» (issue list). */
  resolved?: number
}

/**
 * Keyset "load more" list: chunks append in order, never all at once, one
 * request in flight, stale responses (after reset) dropped. Rows are deduped by
 * id so a row that moved while scrolling cannot appear twice.
 */
export function useChunks<T extends { id: number | string }>(
  fetch: (cursor: string) => Promise<Result<Chunk<T>>>,
  opts: {
    /**
     * Stale-while-revalidate: reset() shows what this key (the filter) loaded
     * last at once, and the next load replaces it with a fresh first chunk.
     */
    cacheKey?: () => string
  } = {},
) {
  const items = shallowRef<T[]>([])
  const total = ref<number | null>(null)
  const counts = ref<ChunkCounts | null>(null)
  const loading = ref(false)
  const done = ref(false)
  const error = ref('')
  const status = ref(0)
  /** headCursor of the first chunk (newest row) for head refreshes. */
  const head = ref('')
  /** Cached rows are shown and the first chunk is being refetched (no skeleton). */
  const refreshing = ref(false)
  let cursor = ''
  let stale = false
  let gen = 0
  let inflight: Promise<void> | null = null

  async function run(g: number) {
    const r = await fetch(cursor)
    if (g !== gen) return
    loading.value = false
    refreshing.value = false
    inflight = null
    if (!r.ok) {
      error.value = r.error
      status.value = r.status
      return
    }
    error.value = ''
    if (stale) {
      stale = false
      items.value = []
      head.value = ''
    }
    const known = new Set(items.value.map((it) => it.id))
    const fresh = r.data.items.filter((it) => !known.has(it.id))
    if (!items.value.length && r.data.headCursor) head.value = r.data.headCursor
    items.value = items.value.concat(fresh)
    if (r.data.total !== undefined) total.value = r.data.total
    if (r.data.counts) counts.value = r.data.counts
    if (r.data.nextCursor) cursor = r.data.nextCursor
    done.value = !r.data.more
    save()
  }

  function save() {
    const k = opts.cacheKey?.()
    if (k !== undefined) listCache.set(k, { items: items.value.slice(0, LIST_CACHE_ROWS), total: total.value, counts: counts.value, head: head.value } satisfies Snapshot<T>)
  }

  /** Loads the next chunk (no-op while loading or when everything is loaded). */
  function loadMore(): Promise<void> {
    if (inflight) return inflight
    if (done.value) return Promise.resolve()
    if (stale) refreshing.value = true
    else loading.value = true
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
    counts.value = null
    head.value = ''
    cursor = ''
    done.value = false
    loading.value = false
    refreshing.value = false
    error.value = ''
    status.value = 0
    stale = false
    const k = opts.cacheKey?.()
    const snap = k === undefined ? undefined : (listCache.get(k) as Snapshot<T> | undefined)
    if (snap?.items.length) {
      items.value = snap.items
      total.value = snap.total
      counts.value = snap.counts
      head.value = snap.head
      stale = true // the first loadMore refetches from the top and replaces these
    }
  }

  /** Replaces the whole list with a fresh re-read of the loaded range (live refresh): rows, cursor and end stay consistent. */
  function replace(c: Chunk<T>) {
    gen++
    inflight = null
    loading.value = false
    refreshing.value = false
    stale = false
    items.value = c.items
    if (c.total !== undefined) total.value = c.total
    if (c.counts) counts.value = c.counts
    cursor = c.nextCursor
    done.value = !c.more
    save()
  }

  return { items, total, counts, loading, refreshing, done, error, status, head, loadMore, loadTail, reset, replace, save }
}
