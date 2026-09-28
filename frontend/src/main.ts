import { createApp, watch } from 'vue'
import { createPinia } from 'pinia'
import PrimeVue from 'primevue/config'
import type { PrimeVueLocaleOptions } from 'primevue/config'
import ToastService from 'primevue/toastservice'
import ConfirmationService from 'primevue/confirmationservice'
import Tooltip from 'primevue/tooltip'
import { ru as primeRu } from 'primelocale/js/ru.js'
import '@fontsource-variable/inter'
import 'primeicons/primeicons.css'
import App from './App.vue'
import { router, updateDocumentTitle } from './router'
import { i18n, lang } from './i18n'
import { preset } from './theme'
import './style.css'

// No PrimeUI license key: scripts/primevue-local.mjs (npm postinstall) strips
// PrimeVue's license check, so no key ships inside the public exe.

const app = createApp(App)
  .use(createPinia())
  .use(router)
  .use(i18n)
  .use(PrimeVue, {
    theme: {
      preset,
      options: { darkModeSelector: '.dark', cssLayer: { name: 'primevue', order: 'primevue' } },
    },
  })
  .use(ToastService)
  .use(ConfirmationService)
  .directive('tooltip', Tooltip)

// PrimeVue component texts (paginator, calendar, filters, aria) follow the UI
// language: English = PrimeVue's built-in defaults, Russian = primelocale.
const primeConfig = app.config.globalProperties.$primevue.config
const primeEn = { ...primeConfig.locale } as PrimeVueLocaleOptions
watch(
  i18n.global.locale,
  () => {
    const l = lang()
    primeConfig.locale = l === 'ru' ? { ...primeEn, ...(primeRu as PrimeVueLocaleOptions), aria: { ...primeEn.aria, ...primeRu.aria } } : primeEn
    document.documentElement.lang = l
    updateDocumentTitle()
  },
  { immediate: true },
)

app.mount('#app')
