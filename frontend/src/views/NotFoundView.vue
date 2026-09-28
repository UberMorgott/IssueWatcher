<script setup lang="ts">
import Button from 'primevue/button'
import { useI18n } from 'vue-i18n'
import EmptyState from '../components/EmptyState.vue'

const { t } = useI18n()
</script>

<template>
  <div class="page">
    <EmptyState
      icon="pi pi-compass"
      :title="t('notFound.title')"
      :text="t('notFound.text')"
    >
      <Button
        as="router-link"
        to="/"
        :label="t('notFound.go')"
        icon="pi pi-arrow-left"
      />
    </EmptyState>
  </div>
</template>
