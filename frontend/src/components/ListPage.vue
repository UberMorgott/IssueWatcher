<script setup lang="ts">
import { nextTick, ref } from 'vue'
import InputText from 'primevue/inputtext'
import IconField from 'primevue/iconfield'
import InputIcon from 'primevue/inputicon'
import Button from 'primevue/button'
import Skeleton from 'primevue/skeleton'
import { useI18n } from 'vue-i18n'

// One layout for every list page (Issues, Comments, Projects, Jobs): a sticky
// toolbar — search left, filter controls of one height, «Сбросить», the count
// right — then the list, or skeleton rows while the first chunk loads.

/** Search text; the box shows only when the page binds it (v-model:search). */
const search = defineModel<string>('search')
withDefaults(
  defineProps<{
    searchPlaceholder?: string
    searchAria?: string
    /** Some filter is set: «Сбросить» shows. */
    resettable?: boolean
    /** Rows matching the filters (null = unknown or not worth repeating). */
    total?: number | null
    totalLabel?: string
    /** First fetch and nothing cached: skeleton rows instead of the list. */
    skeleton?: boolean
    skeletonRows?: number
  }>(),
  { searchPlaceholder: '', searchAria: '', total: null, totalLabel: '', skeletonRows: 8 },
)
const emit = defineEmits<{ reset: [] }>()
const { t } = useI18n()

const box = ref<{ $el: HTMLElement } | null>(null)
function focusSearch() {
  void nextTick(() => {
    const el = box.value?.$el
    ;((el?.querySelector?.('input') as HTMLInputElement | null) ?? (el as HTMLInputElement | undefined))?.focus()
  })
}
defineExpose({ focusSearch })
</script>

<template>
  <div class="list-page">
    <div
      class="lp-toolbar panel"
      role="toolbar"
    >
      <IconField
        v-if="search !== undefined"
        class="lp-search"
      >
        <InputIcon class="pi pi-search" />
        <InputText
          ref="box"
          v-model="search"
          :placeholder="searchPlaceholder"
          :aria-label="searchAria || searchPlaceholder"
          fluid
        />
      </IconField>
      <slot name="filters" />
      <Button
        v-if="resettable"
        :label="t('issues.reset')"
        icon="pi pi-filter-slash"
        severity="secondary"
        text
        @click="emit('reset')"
      />
      <span class="lp-end">
        <slot name="actions" />
        <span
          v-if="total !== null && totalLabel"
          class="lp-total"
        ><b class="mono">{{ total }}</b> {{ totalLabel }}</span>
      </span>
    </div>
    <div
      v-if="skeleton"
      class="panel lp-skeleton"
      aria-busy="true"
    >
      <Skeleton
        v-for="i in skeletonRows"
        :key="i"
        height="40px"
      />
    </div>
    <slot v-else />
  </div>
</template>

<style scoped>
.list-page {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

/* Sticky under the top bar; every control shares one height. */
.lp-toolbar {
  --lp-h: 38px;

  position: sticky;
  top: calc(var(--iw-topbar) + 8px);
  z-index: 6;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  padding: 12px;
}

.lp-search {
  flex: 1 1 220px;
  min-width: 200px;
}

.lp-toolbar :deep(.p-inputtext),
.lp-toolbar :deep(.p-select),
.lp-toolbar :deep(.p-togglebutton),
.lp-toolbar :deep(.p-button) {
  height: var(--lp-h);
  box-sizing: border-box;
}

.lp-toolbar :deep(.p-select) {
  align-items: center;
}

.lp-toolbar :deep(.p-selectbutton) {
  display: inline-flex;
}

.lp-end {
  display: inline-flex;
  align-items: center;
  gap: 10px;
  margin-left: auto;
}

.lp-total {
  color: var(--iw-muted);
  font-size: calc(13px * var(--iw-fs, 1));
  white-space: nowrap;
}

.lp-total b {
  color: var(--iw-text);
}

.lp-skeleton {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 16px;
}
</style>
