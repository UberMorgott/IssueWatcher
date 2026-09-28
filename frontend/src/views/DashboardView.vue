<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import DataTable from 'primevue/datatable'
import Column from 'primevue/column'
import { useItemsStore, type Item } from '../stores/items'

const store = useItemsStore()
const router = useRouter()
const selected = ref<Item[]>([])

function open(item: Item) {
  void router.push({ name: 'item', params: { id: String(item.id) } })
}
</script>

<template>
  <section>
    <p class="summary">
      {{ selected.length }} selected of {{ store.items.length }}
    </p>
    <DataTable
      v-model:selection="selected"
      :value="store.items"
      data-key="id"
      size="small"
      striped-rows
      @row-click="(e) => open(e.data as Item)"
    >
      <Column
        selection-mode="multiple"
        header-style="width: 3rem"
      />
      <Column
        field="project"
        header="Project"
        sortable
      />
      <Column
        field="number"
        header="#"
        sortable
      />
      <Column
        field="title"
        header="Title"
      />
      <Column
        field="status"
        header="Status"
        sortable
      />
      <Column
        field="updated"
        header="Updated"
        sortable
      />
    </DataTable>
  </section>
</template>

<style scoped>
.summary {
  margin: 0 0 0.5rem;
  opacity: 0.7;
}
</style>
