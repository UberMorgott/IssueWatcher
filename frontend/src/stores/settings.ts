import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api } from '../api/client'
import type { Settings, SettingsDoc, SettingsPatch } from '../api/types'
import { lang, setLang, t } from '../i18n'
import { applyAppearance } from '../lib/appearance'

function stored(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

/**
 * Server-side settings (data\config.json): one document with a revision.
 * Every change is a JSON merge patch; a 409 (another tab saved first) reloads
 * and re-applies the same patch once. settings.changed live events keep every
 * tab in step and re-apply language and appearance.
 */
export const useSettingsStore = defineStore('settings', () => {
  const doc = ref<SettingsDoc | null>(null)
  const available = ref(true)
  const saving = ref(0)

  function apply(d: SettingsDoc) {
    if (doc.value && d.revision < doc.value.revision) return
    doc.value = d
    const s = d.settings
    if (s.general.language !== lang()) setLang(s.general.language)
    applyAppearance(s.appearance, d.info?.palettes ?? [])
  }

  async function load() {
    const r = await api.settingsDoc()
    available.value = r.ok || r.status !== 404
    if (!r.ok) return
    apply(r.data)
    await adoptBrowserPrefs(r.data)
  }

  /**
   * First run after the upgrade (revision 0 = never saved by this build): the
   * language and theme the user picked earlier lived in localStorage; carry
   * them over once so nothing visibly resets.
   */
  async function adoptBrowserPrefs(d: SettingsDoc) {
    if (d.revision !== 0) return
    const p: SettingsPatch = {}
    const l = stored('iw.lang')
    if ((l === 'ru' || l === 'en') && l !== d.settings.general.language) p.general = { language: l }
    const th = stored('iw.theme')
    if ((th === 'dark' || th === 'light') && th !== d.settings.appearance.mode) p.appearance = { mode: th }
    if (Object.keys(p).length) await patch(p)
  }

  /** Saves a change; returns '' or an error message (validation text from the server). */
  async function patch(p: SettingsPatch): Promise<string> {
    if (!doc.value) return t('settings.unavailable')
    saving.value++
    try {
      for (let attempt = 0; attempt < 2; attempt++) {
        const r = await api.patchSettings(doc.value.revision, p)
        if (r.ok) {
          apply(r.data)
          return ''
        }
        if (r.status === 409 && r.current) {
          apply(r.current) // another tab saved first: retry on top of it
          continue
        }
        return r.error
      }
      return t('settings.conflict')
    } finally {
      saving.value--
    }
  }

  async function reset(section: keyof Settings): Promise<string> {
    if (!doc.value) return t('settings.unavailable')
    const r = await api.resetSettings(doc.value.revision, section)
    if (r.ok) {
      apply(r.data)
      return ''
    }
    if (r.status === 409 && r.current) apply(r.current)
    return r.error
  }

  return { doc, available, saving, apply, load, patch, reset }
})
