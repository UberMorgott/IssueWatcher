<script setup lang="ts">
defineProps<{ title: string; text?: string }>()
</script>

<template>
  <section class="panel">
    <div class="panel-head">
      <span class="panel-title">{{ title }}</span>
      <slot name="actions" />
    </div>
    <div class="panel-body rows">
      <p
        v-if="text"
        class="intro"
      >
        {{ text }}
      </p>
      <slot />
    </div>
  </section>
</template>

<style scoped>
.rows {
  display: flex;
  flex-direction: column;
  padding-top: 6px;
}

.intro {
  margin: 4px 0 6px;
  font-size: 13px;
  color: var(--iw-muted);
}
</style>
