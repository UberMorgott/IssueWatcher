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
    <div class="lp-sticky">
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
          v-tooltip.bottom="t('issues.reset')"
          :aria-label="t('issues.reset')"
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

/* Sticks flush under the top bar as an opaque page-coloured band: it spans the
   gutters and pads the toolbar by 8px above and below, so no scrolled row shows above the
   toolbar, beside it or through its rounded corners. The negative margins keep
   the unstuck layout where it was. */
.lp-sticky {
  position: sticky;
  top: var(--iw-topbar);
  z-index: 6;
  margin: -8px calc(-1 * var(--iw-gutter));
  padding: 8px var(--iw-gutter);
  background: var(--iw-bg);
  container: lp / inline-size;
}

/* One control language for every page's filters: each control is --lp-h high,
   framed like an input; selects size to their value (capped, ellipsis), the
   search takes what is left, so the whole bar fits one row at desktop widths and
   wraps only on narrow windows. Pages pass bare PrimeVue controls: no widths. */
.lp-toolbar {
  --lp-h: 38px;
  --lp-frame: 1px solid var(--p-inputtext-border-color);

  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  padding: 10px;
}

.lp-search {
  flex: 1 1 180px;
  min-width: 140px;
}

.lp-toolbar :deep(.p-inputtext),
.lp-toolbar :deep(.p-select),
.lp-toolbar :deep(.p-selectbutton),
.lp-toolbar > :deep(.p-togglebutton),
.lp-toolbar :deep(.p-button) {
  height: var(--lp-h);
  box-sizing: border-box;
}

.lp-toolbar :deep(.p-button-icon-only) {
  width: var(--lp-h);
}

.lp-toolbar :deep(.p-select) {
  flex: 0 1 auto;
  align-items: center;
  min-width: 96px;
  max-width: 200px;
}

.lp-toolbar :deep(.p-select-label) {
  overflow: hidden;
  padding-inline: 10px 2px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.lp-toolbar :deep(.p-select-dropdown) {
  width: 28px;
}

/* Segmented controls and lone toggles: an input-framed track, the options as
   pills inside it, so they line up with the selects. */
.lp-toolbar :deep(.p-selectbutton),
.lp-toolbar > :deep(.p-togglebutton) {
  display: inline-flex;
  flex: none;
  gap: 2px;
  padding: 3px;
  border: var(--lp-frame);
  border-radius: var(--p-inputtext-border-radius);
  background: var(--p-inputtext-background);
}

.lp-toolbar :deep(.p-selectbutton .p-togglebutton) {
  height: 100%;
  padding: 0;
  border: 0;
  background: transparent;
}

.lp-toolbar :deep(.p-togglebutton-content) {
  height: 100%;
  padding: 0 9px;
}

/* A narrower bar (1280–1440 px windows beside the sidebar) tightens instead of
   wrapping: smaller gaps and paddings, a lone toggle keeps only its icon (its
   aria-label and tooltip carry the name). */
@container lp (max-width: 1100px) {
  .lp-toolbar {
    gap: 6px;
  }

  .lp-search {
    flex-basis: 140px;
    min-width: 120px;
  }

  .lp-toolbar :deep(.p-select-label) {
    padding-inline: 8px 0;
  }

  .lp-toolbar :deep(.p-togglebutton-content) {
    padding: 0 7px;
  }

  .lp-toolbar > :deep(.p-togglebutton .p-togglebutton-label) {
    display: none;
  }
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
