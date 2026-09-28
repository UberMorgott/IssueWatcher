<script setup lang="ts">
defineProps<{ label: string; value: number | string | null; icon: string; tone?: 'primary' | 'success' | 'warn' | 'muted'; hint?: string; loading?: boolean }>()
</script>

<template>
  <div
    class="stat panel"
    :class="tone ?? 'primary'"
  >
    <div class="stat-top">
      <span class="stat-label">{{ label }}</span>
      <span class="stat-icon"><i :class="icon" /></span>
    </div>
    <div
      v-if="loading"
      class="stat-skel"
    />
    <div
      v-else
      class="stat-value mono"
    >
      {{ value ?? '—' }}
    </div>
    <div
      v-if="hint"
      class="stat-hint"
    >
      {{ hint }}
    </div>
  </div>
</template>

<style scoped>
.stat {
  padding: 18px 20px;
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}

.stat-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.stat-label {
  font-size: 12px;
  font-weight: 500;
  color: var(--iw-muted);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}

.stat-icon {
  display: grid;
  place-items: center;
  width: 30px;
  height: 30px;
  border-radius: 9px;
  font-size: 13px;
}

.primary .stat-icon {
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.success .stat-icon {
  color: var(--iw-success);
  background: var(--iw-success-soft);
}

.warn .stat-icon {
  color: var(--iw-warn);
  background: var(--iw-warn-soft);
}

.muted .stat-icon {
  color: var(--iw-muted);
  background: var(--iw-elevated);
}

.stat-value {
  font-size: 32px;
  line-height: 38px;
  font-weight: 650;
  font-family: var(--iw-font);
  letter-spacing: -0.02em;
}

.stat-skel {
  height: 38px;
  width: 60%;
  border-radius: 8px;
  background: linear-gradient(90deg, var(--iw-elevated), var(--iw-hover), var(--iw-elevated));
  background-size: 200% 100%;
  animation: shimmer 1.2s linear infinite;
}

.stat-hint {
  font-size: 12px;
  color: var(--iw-muted);
}

@keyframes shimmer {
  from {
    background-position: 200% 0;
  }

  to {
    background-position: -200% 0;
  }
}
</style>
