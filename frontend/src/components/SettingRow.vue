<script setup lang="ts">
defineProps<{ title: string; text?: string; stack?: boolean }>()
</script>

<template>
  <div
    class="setting-row"
    :class="{ stack }"
  >
    <div class="label">
      <div class="title">
        {{ title }}
      </div>
      <div
        v-if="text || $slots.text"
        class="text"
      >
        <slot name="text">
          {{ text }}
        </slot>
      </div>
    </div>
    <div class="control">
      <slot />
    </div>
  </div>
</template>

<style scoped>
.setting-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
  padding: 14px 0;
  border-bottom: 1px solid var(--iw-border);
}

.setting-row:last-child {
  border-bottom: 0;
}

.setting-row.stack {
  flex-direction: column;
  align-items: stretch;
  gap: 12px;
}

.label {
  flex: 1 1 0;
  min-width: 200px;
}

.title {
  font-weight: 500;
}

.text {
  margin-top: 2px;
  font-size: 13px;
  color: var(--iw-muted);
}

.control {
  display: flex;
  flex: 0 1 auto;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  justify-content: flex-end;
  min-width: 0;
  max-width: 65%;
}

.stack .control {
  justify-content: flex-start;
  max-width: none;
}

@media (width <= 767px) {
  .setting-row {
    flex-direction: column;
    align-items: stretch;
    gap: 10px;
  }

  .control {
    justify-content: flex-start;
    max-width: none;
  }
}
</style>
