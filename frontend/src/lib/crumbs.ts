import { onBeforeUnmount, ref, watchEffect } from 'vue'
import type { RouteLocationRaw } from 'vue-router'

/** One step of the top-bar breadcrumb; the last one is the current page. */
export interface Crumb {
  label: string
  to?: RouteLocationRaw
  mono?: boolean
}

/**
 * Breadcrumb shown in the top bar instead of the plain route title (detail
 * pages: Issues › owner/repo#12, Settings › Appearance). The top bar is the
 * only page heading, so views never repeat their title in the body.
 */
export const crumbs = ref<Crumb[]>([])

let owner = 0

/** Keeps the top-bar breadcrumb in sync with `build` while the calling view is mounted. */
export function useCrumbs(build: () => Crumb[]) {
  const me = ++owner
  let mine = true
  watchEffect(() => {
    if (!mine) return
    owner = me
    crumbs.value = build()
  })
  onBeforeUnmount(() => {
    mine = false
    if (owner === me) crumbs.value = []
  })
}
