<script setup lang="ts">
defineProps<{ icon: string; title: string; text?: string; compact?: boolean }>()
</script>

<template>
  <div
    class="empty"
    :class="{ compact }"
  >
    <span class="empty-icon"><i :class="icon" /></span>
    <h3 class="empty-title">
      {{ title }}
    </h3>
    <p
      v-if="text"
      class="empty-text"
    >
      {{ text }}
    </p>
    <div
      v-if="$slots.default"
      class="empty-actions"
    >
      <slot />
    </div>
  </div>
</template>

<style scoped>
.empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  text-align: center;
  gap: 8px;
  padding: 48px 24px;
}

.empty.compact {
  padding: 24px 16px;
}

.empty-icon {
  display: grid;
  place-items: center;
  width: 48px;
  height: 48px;
  margin-bottom: 4px;
  border-radius: 14px;
  background: var(--iw-elevated);
  color: var(--iw-muted);
  font-size: 20px;
}

.compact .empty-icon {
  width: 36px;
  height: 36px;
  font-size: 15px;
}

.empty-title {
  font-size: 15px;
  font-weight: 600;
}

.empty-text {
  margin: 0;
  max-width: 420px;
  color: var(--iw-muted);
}

.empty-actions {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}
</style>
