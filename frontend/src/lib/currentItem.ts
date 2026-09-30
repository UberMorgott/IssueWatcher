import { ref } from 'vue'
import type { ItemKind } from '../api/types'

/**
 * Kind of the item the item page shows ('' while loading or elsewhere): the
 * sidebar highlights Comments for a mod page thread and Issues otherwise.
 */
export const currentItemKind = ref<ItemKind | ''>('')
