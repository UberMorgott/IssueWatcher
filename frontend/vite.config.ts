import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  define: {
    // vue-i18n feature flags: Composition API only, no devtools in the bundle.
    __VUE_I18N_FULL_INSTALL__: true,
    __VUE_I18N_LEGACY_API__: false,
    __INTLIFY_PROD_DEVTOOLS__: false,
  },
  build: {
    // Desktop app: the bundle loads from the embedded asset server, not the network.
    chunkSizeWarningLimit: 1500,
  },
})
