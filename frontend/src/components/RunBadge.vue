<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { RunState } from '../api/types'
import { RUN_ICON, RUN_TONE } from '../lib/release'

// Release run state pill (same colours as the job badge).
defineProps<{ state: RunState }>()
const { t } = useI18n()
</script>

<template>
  <span
    class="run-badge"
    :class="RUN_TONE[state]"
  >
    <i :class="RUN_ICON[state] ?? 'pi pi-circle'" />
    <span>{{ t('release.state.' + state) }}</span>
  </span>
</template>

<style scoped>
.run-badge {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  flex: none;
  padding: 1px 8px;
  border-radius: 999px;
  font-size: calc(11.5px * var(--iw-fs, 1));
  font-weight: 500;
  line-height: 1.6;
  white-space: nowrap;
  color: var(--iw-muted);
  background: var(--iw-elevated);
}

.run-badge i {
  font-size: calc(10.5px * var(--iw-fs, 1));
}

.run-badge.running {
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.run-badge.needs-review {
  color: var(--iw-warn);
  background: var(--iw-warn-soft);
}

.run-badge.done {
  color: var(--iw-success);
  background: var(--iw-success-soft);
}

.run-badge.failed {
  color: var(--iw-danger);
  background: var(--iw-danger-soft);
}

.run-badge.cancelled {
  color: var(--iw-dimmed);
}
</style>
