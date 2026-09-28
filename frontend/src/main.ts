import { createApp } from 'vue'
import { createPinia } from 'pinia'
import PrimeVue from 'primevue/config'
import ToastService from 'primevue/toastservice'
import ConfirmationService from 'primevue/confirmationservice'
import Tooltip from 'primevue/tooltip'
import { registerLicense } from '@primeui/license-manager'
import '@fontsource-variable/inter'
import 'primeicons/primeicons.css'
import App from './App.vue'
import { router } from './router'
import { preset } from './theme'
import './style.css'

// PrimeUI Community license (same setup as E:\DEV\1сEPD\web). Without a key
// PrimeVue 5 draws an "invalid license" banner over the page. The check is
// offline; the key comes from frontend/.env (gitignored) at build time and is
// bound to the organisation, not to a user.
const licenseKey = import.meta.env.VITE_PRIMEUI_LICENSE
if (licenseKey) {
  registerLicense({ primeui: licenseKey })
}

createApp(App)
  .use(createPinia())
  .use(router)
  .use(PrimeVue, {
    theme: {
      preset,
      options: { darkModeSelector: '.dark', cssLayer: { name: 'primevue', order: 'primevue' } },
    },
  })
  .use(ToastService)
  .use(ConfirmationService)
  .directive('tooltip', Tooltip)
  .mount('#app')
