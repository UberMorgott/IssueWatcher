/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** PrimeUI Community license key (frontend/.env, not committed). */
  readonly VITE_PRIMEUI_LICENSE?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
