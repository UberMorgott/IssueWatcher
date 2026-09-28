<script setup lang="ts">
import { computed } from 'vue'
import { labelHue } from '../lib/format'

const props = defineProps<{ name: string }>()
const hue = computed(() => labelHue(props.name))
</script>

<template>
  <span
    class="label"
    :style="{ '--h': hue }"
  >{{ name }}</span>
</template>

<style scoped>
.label {
  display: inline-flex;
  align-items: center;
  max-width: 160px;
  padding: 1px 8px;
  border-radius: 999px;
  font-size: 11.5px;
  font-weight: 500;
  line-height: 18px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  color: hsl(var(--h) 70% 72%);
  background: hsl(var(--h) 60% 50% / 14%);
  border: 1px solid hsl(var(--h) 60% 60% / 28%);
}

:global(:root:not(.dark)) .label {
  color: hsl(var(--h) 60% 32%);
  background: hsl(var(--h) 70% 50% / 10%);
  border-color: hsl(var(--h) 60% 45% / 30%);
}
</style>
