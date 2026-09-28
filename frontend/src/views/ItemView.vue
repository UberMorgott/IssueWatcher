<script setup lang="ts">
import { computed } from 'vue'
import { useItemsStore } from '../stores/items'

const props = defineProps<{ id: string }>()
const store = useItemsStore()
const item = computed(() => store.byId(Number(props.id)))
</script>

<template>
  <section>
    <RouterLink to="/">
      &larr; Dashboard
    </RouterLink>
    <h2 v-if="item">
      {{ item.project }} #{{ item.number }}: {{ item.title }}
    </h2>
    <h2 v-else>
      Item {{ props.id }}
    </h2>
    <p v-if="item">
      Status: {{ item.status }} · updated {{ item.updated }}
    </p>
    <p v-else>
      Not in the local store yet (mock data covers ids 1–5).
    </p>
  </section>
</template>
