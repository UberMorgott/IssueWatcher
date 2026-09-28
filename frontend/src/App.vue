<script setup lang="ts">
import { onMounted, ref } from 'vue'

// The session cookie (set by the tray's one-time launch link) authorizes /api.
const version = ref('')

onMounted(async () => {
  try {
    const res = await fetch('/api/health')
    if (res.ok) version.value = ((await res.json()) as { version: string }).version
  } catch {
    // offline: header just omits the version
  }
})
</script>

<template>
  <div class="shell">
    <header class="shell-header">
      <RouterLink
        to="/"
        class="brand"
      >
        IssueWatcher
      </RouterLink>
      <span
        v-if="version"
        class="version"
      >{{ version }}</span>
    </header>
    <main class="shell-main">
      <RouterView />
    </main>
  </div>
</template>

<style scoped>
.version {
  margin-left: 0.5rem;
  opacity: 0.6;
  font-size: 0.85em;
}
</style>
