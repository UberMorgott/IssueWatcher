import { customRef, type Ref } from 'vue'
import type { Comment, IssueDetail, Job } from '../api/types'

// Stale-while-revalidate for non-list pages: what a page loaded last stays in
// memory (this tab only), so a revisit renders it at once and refetches
// quietly; a skeleton shows only when nothing was loaded yet.

const values = new Map<string, unknown>()

/**
 * A ref whose value outlives the view under `key`. Shallow: assign a new value
 * (nested mutation does not notify).
 */
export function cachedRef<T>(key: string, initial: T): Ref<T> {
  return customRef<T>((track, trigger) => ({
    get() {
      track()
      return (values.has(key) ? values.get(key) : initial) as T
    },
    set(v) {
      values.set(key, v)
      trigger()
    },
  }))
}

/** The last loaded value per id (item and job pages), the newest `limit` kept. */
export function detailCache<T>(limit = 50) {
  const m = new Map<string, T>()
  return {
    get: (id: string | number) => m.get(String(id)),
    set(id: string | number, v: T) {
      const k = String(id)
      m.delete(k)
      m.set(k, v)
      if (m.size > limit) m.delete(m.keys().next().value as string)
    },
  }
}

export const itemCache = detailCache<IssueDetail>()
export const commentsCache = detailCache<Comment[]>()
export const itemJobsCache = detailCache<Job[]>()
export const jobCache = detailCache<Job>()
