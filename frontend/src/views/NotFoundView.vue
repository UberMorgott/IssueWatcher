<script setup lang="ts">
import Button from 'primevue/button'
import EmptyState from '../components/EmptyState.vue'
</script>

<template>
  <div class="page">
    <EmptyState
      icon="pi pi-compass"
      title="Page not found"
      text="This address does not exist in IssueWatcher."
    >
      <Button
        as="router-link"
        to="/"
        label="Go to Overview"
        icon="pi pi-arrow-left"
      />
    </EmptyState>
  </div>
</template>
