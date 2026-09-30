/**
 * Where a list was left (scroll offset, keyboard cursor) per filter key: going
 * back from an item returns to the same rows instead of the top.
 */
export interface ListPosition {
  top: number
  cursor: number
}

const positions = new Map<string, ListPosition>()

export const rememberPosition = (key: string, p: ListPosition) => positions.set(key, p)

/** The saved position of key, removed once taken (a later visit starts at the top). */
export function takePosition(key: string): ListPosition | undefined {
  const p = positions.get(key)
  positions.delete(key)
  return p
}
