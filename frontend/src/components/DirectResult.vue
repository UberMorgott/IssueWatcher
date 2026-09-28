<script setup lang="ts">
import { computed } from 'vue'
import Message from 'primevue/message'
import { useI18n } from 'vue-i18n'
import type { Job } from '../api/types'
import { absTime, relTime } from '../lib/format'
import { jobOutcome } from '../lib/jobs'

// Result of a direct fix job: outcome, the agent's commits in the mapped folder
// and warnings about the folder state (uncommitted changes, missing "Fixes #N").
const props = defineProps<{ job: Job }>()
const { t } = useI18n()

const local = computed(() => props.job.result.local)
const outcome = computed(() => jobOutcome(props.job))
const commits = computed(() => local.value?.commits ?? [])
const tone = computed(() => {
  const o = outcome.value
  return o === 'closed' || o === 'pushed' ? 'ok' : o === 'failed' ? 'bad' : 'warn'
})
</script>

<template>
  <div class="direct">
    <div
      v-if="outcome"
      class="outcome"
      :class="tone"
    >
      {{ t('jobs.outcome.' + outcome) }}
    </div>
    <div
      v-if="local?.pushedAt"
      class="muted small"
      :title="absTime(local.pushedAt)"
    >
      {{ t('job.direct.pushedAt', { time: relTime(local.pushedAt) }) }}
    </div>
    <ul
      v-if="commits.length"
      class="commits"
      :aria-label="t('job.direct.commits')"
    >
      <li
        v-for="c in commits"
        :key="c.sha"
      >
        <span class="mono sha">{{ c.sha.slice(0, 7) }}</span>
        <span
          class="subject"
          :title="c.subject"
        >{{ c.subject }}</span>
        <i
          v-if="c.fixes"
          v-tooltip.top="t('job.direct.fixesTip', { n: job.number })"
          class="pi pi-link fixes"
        />
      </li>
    </ul>
    <Message
      v-if="local?.dirtyBefore?.length"
      severity="warn"
      size="small"
    >
      <details>
        <summary>{{ t('job.direct.dirtyBefore') }}</summary>
        <ul class="files mono">
          <li
            v-for="f in local.dirtyBefore"
            :key="f"
          >
            {{ f }}
          </li>
        </ul>
      </details>
    </Message>
    <Message
      v-if="commits.length && local && !local.fixesRef"
      severity="warn"
      size="small"
    >
      {{ t('job.direct.noFixesRef', { n: job.number }) }}
    </Message>
    <Message
      v-if="local?.dirtyAfter?.length"
      severity="warn"
      size="small"
    >
      <details>
        <summary>{{ t('job.direct.dirtyAfter') }}</summary>
        <ul class="files mono">
          <li
            v-for="f in local.dirtyAfter"
            :key="f"
          >
            {{ f }}
          </li>
        </ul>
      </details>
    </Message>
  </div>
</template>

<style scoped>
.direct {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
}

.outcome {
  align-self: flex-start;
  padding: 1px 10px;
  border-radius: 999px;
  font-size: calc(12px * var(--iw-fs, 1));
  font-weight: 500;
}

.outcome.ok {
  color: var(--iw-success);
  background: var(--iw-success-soft);
}

.outcome.warn {
  color: var(--iw-warn);
  background: var(--iw-warn-soft);
}

.outcome.bad {
  color: var(--iw-danger);
  background: var(--iw-danger-soft);
}

.small {
  font-size: calc(12px * var(--iw-fs, 1));
}

.commits {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin: 0;
  padding: 0;
  list-style: none;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.commits li {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.sha {
  flex: none;
  color: var(--iw-primary);
}

.subject {
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.fixes {
  flex: none;
  font-size: calc(11px * var(--iw-fs, 1));
  color: var(--iw-success);
}

details summary {
  cursor: pointer;
}

.files {
  margin: 6px 0 0;
  padding-left: 18px;
  font-size: calc(12px * var(--iw-fs, 1));
  overflow-wrap: anywhere;
}
</style>
