import { createI18n } from 'vue-i18n'
import en from './en'
import ru from './ru'

export type Lang = 'ru' | 'en'
export const LANGS: Lang[] = ['ru', 'en']
const KEY = 'iw.lang'

function saved(): Lang {
  try {
    return localStorage.getItem(KEY) === 'en' ? 'en' : 'ru'
  } catch {
    return 'ru' // storage blocked: default language
  }
}

const ruPlural = new Intl.PluralRules('ru')

/**
 * Russian plural choice for "one | few | many" (1 / 2–4 / 5+, 11–14 → many);
 * four forms put zero first: "none | one | few | many".
 */
function slavic(choice: number, choicesLength: number): number {
  const n = Math.abs(choice)
  const zero = choicesLength === 4
  if (zero && n === 0) return 0
  const cat = ruPlural.select(n)
  const i = cat === 'one' ? 0 : cat === 'few' ? 1 : 2
  return Math.min(i + (zero ? 1 : 0), choicesLength - 1)
}

export const i18n = createI18n({
  legacy: false,
  locale: saved(),
  fallbackLocale: 'en',
  messages: { ru, en },
  pluralRules: { ru: slavic },
})

/** Current UI language (reactive). */
export function lang(): Lang {
  return i18n.global.locale.value as Lang
}

export function setLang(l: Lang) {
  i18n.global.locale.value = l
  try {
    localStorage.setItem(KEY, l)
  } catch {
    /* storage blocked: choice lasts for this tab only */
  }
}

export const t = i18n.global.t
