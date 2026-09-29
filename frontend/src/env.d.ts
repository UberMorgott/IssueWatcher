/// <reference types="vite/client" />

// Lets plain tsserver/tsc (no Vue plugin) resolve SFC imports; vue-tsc uses the real component types.
declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/no-empty-object-type
  const component: DefineComponent<{}, {}, any>
  export default component
}
