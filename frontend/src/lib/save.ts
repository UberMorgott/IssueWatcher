import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { onBeforeUnmount } from 'vue'
import type { Settings, SettingsPatch } from '../api/types'
import { useSettingsStore } from '../stores/settings'

/**
 * Saves a settings patch and toasts the server's reason when it is rejected.
 * `later(key, patch)` debounces typing in number/text fields (one save per pause).
 */
export function useSave() {
  const settings = useSettingsStore()
  const toast = useToast()
  const { t } = useI18n()
  const timers = new Map<string, number>()
  onBeforeUnmount(() => timers.forEach((id) => window.clearTimeout(id)))

  async function save(p: SettingsPatch | ((s: Settings) => SettingsPatch)): Promise<boolean> {
    const err = await settings.patch(p)
    if (err) toast.add({ severity: 'error', summary: t('settings.saveFailed'), detail: err, life: 6000 })
    return !err
  }
  save.later = (key: string, p: SettingsPatch, ms = 700) => {
    window.clearTimeout(timers.get(key))
    timers.set(key, window.setTimeout(() => void save(p), ms))
  }
  return save
}
