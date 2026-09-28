import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  build: {
    // Desktop app: the bundle loads from the embedded asset server, not the network.
    chunkSizeWarningLimit: 1500,
  },
})
