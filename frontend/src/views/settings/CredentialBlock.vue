<script setup lang="ts">
import Button from 'primevue/button'
import { useI18n } from 'vue-i18n'
import { absTime } from '../../lib/format'

// One credential of an integration (an API key, an upload token, a sign-in).
// Set: a compact state line (who / what, when checked) with «Проверить»,
// «Изменить» and «Удалить» — no empty input fields. Not set, or after
// «Изменить»: the input form (default slot) with its hint; «Отмена» goes back.
const props = defineProps<{
  title: string
  /** set and usable: the compact state shows */
  configured: boolean
  /** the state line when set («Ключ проверен: user») */
  summary: string
  /** the state line when not set («Ключ не задан») */
  empty: string
  checkedAt?: string
  /** a running action: 'check' | 'remove' | … (disables the buttons) */
  busy?: string
  checkLabel?: string
  changeLabel?: string
  removeLabel: string
  /** a warning state line instead of the summary (expired) */
  warn?: string
}>()
const editing = defineModel<boolean>('editing', { default: false })
const emit = defineEmits<{ check: []; remove: [] }>()
const { t } = useI18n()
</script>

<template>
  <div
    class="adv cred"
    role="group"
    :aria-label="title"
  >
    <div class="key-head">
      <span class="label">{{ title }}</span>
      <span
        v-if="props.warn"
        class="key-state warn"
      ><i class="pi pi-exclamation-circle" /> {{ props.warn }}</span>
      <span
        v-else-if="configured"
        v-tooltip.top="checkedAt ? t('platforms.checked', { time: absTime(checkedAt) }) : undefined"
        class="key-state ok"
      ><i class="pi pi-check-circle" /> {{ summary }}</span>
      <span
        v-else
        class="key-state muted"
      ><i class="pi pi-key" /> {{ empty }}</span>
    </div>
    <slot name="status" />
    <template v-if="!configured || editing">
      <small
        v-if="$slots.hint"
        class="muted"
      ><slot name="hint" /></small>
      <slot />
      <div
        v-if="configured && editing"
        class="key-actions"
      >
        <Button
          :label="t('platforms.credential.cancel')"
          size="small"
          severity="secondary"
          text
          :disabled="!!busy"
          @click="editing = false"
        />
      </div>
    </template>
    <div
      v-else
      class="key-actions"
    >
      <Button
        :label="checkLabel || t('platforms.check')"
        icon="pi pi-refresh"
        size="small"
        severity="secondary"
        outlined
        :disabled="!!busy"
        :loading="busy === 'check'"
        @click="emit('check')"
      />
      <Button
        :label="changeLabel || t('platforms.credential.change')"
        icon="pi pi-pencil"
        size="small"
        severity="secondary"
        outlined
        :disabled="!!busy"
        @click="editing = true"
      />
      <Button
        :label="removeLabel"
        icon="pi pi-trash"
        size="small"
        severity="secondary"
        text
        :disabled="!!busy"
        :loading="busy === 'remove' || busy === 'forget'"
        @click="emit('remove')"
      />
    </div>
    <slot name="after" />
  </div>
</template>

<style scoped>
.adv {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px;
  border-radius: var(--iw-radius);
  background: var(--iw-bg);
  border: 1px solid var(--iw-border);
}

.key-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.label {
  font-size: calc(13px * var(--iw-fs, 1));
  font-weight: 500;
}

.key-state {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.key-state.ok {
  color: var(--iw-success);
}

.key-state.warn {
  color: var(--iw-warning, var(--iw-danger));
}

.key-actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
</style>
