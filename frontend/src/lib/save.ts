import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { onBeforeUnmount } from 'vue'
import type { Settings, SettingsPatch } from '../api/types'
import { useSettingsStore } from '../stores/settings'

/**
 * Saves a settings patch and toasts the server's reason when it is rejected.
 * `later(key, patch)` debounces typing in number/text fields (one save per pause);
 * a save still waiting when the section closes is sent at once, never dropped.
 */
export function useSave() {
  const settings = useSettingsStore()
  const toast = useToast()
  const { t } = useI18n()
  const pending = new Map<string, { timer: number; patch: SettingsPatch }>()
  onBeforeUnmount(() => {
    for (const { timer, patch } of pending.values()) {
      window.clearTimeout(timer)
      void save(patch)
    }
    pending.clear()
  })

  async function save(p: SettingsPatch | ((s: Settings) => SettingsPatch)): Promise<boolean> {
    const err = await settings.patch(p)
    if (err) toast.add({ severity: 'error', summary: t('settings.saveFailed'), detail: err, life: 6000 })
    return !err
  }
  save.later = (key: string, patch: SettingsPatch, ms = 700) => {
    window.clearTimeout(pending.get(key)?.timer)
    const timer = window.setTimeout(() => {
      pending.delete(key)
      void save(patch)
    }, ms)
    pending.set(key, { timer, patch })
  }
  return save
}
