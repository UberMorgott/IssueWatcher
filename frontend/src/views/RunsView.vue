<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter, type LocationQueryRaw } from 'vue-router'
import Select from 'primevue/select'
import Button from 'primevue/button'
import DataTable from 'primevue/datatable'
import Column from 'primevue/column'
import { useI18n } from 'vue-i18n'
import EmptyState from '../components/EmptyState.vue'
import ListPage from '../components/ListPage.vue'
import PlatformIcon from '../components/PlatformIcon.vue'
import ReleaseDialog from '../components/ReleaseDialog.vue'
import RunBadge from '../components/RunBadge.vue'
import { api } from '../api/client'
import type { ReleaseRun, RunView } from '../api/types'
import { absTime, relTime } from '../lib/format'
import { RUN_STATES, heldText } from '../lib/release'
import { useAppStore } from '../stores/app'

// Release runs (newest first) with live state (SSE autopilot.run); a row opens
// the run's step timeline. «Выпустить релиз» opens the dry-run dialog for the
// filtered project, or the one picked here.
const app = useAppStore()
const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const str = (v: unknown) => (typeof v === 'string' ? v : '')
const filters = computed(() => ({
  state: (RUN_STATES as string[]).includes(str(route.query.state)) ? str(route.query.state) : '',
  project: Number(str(route.query.project)) || 0,
}))
function setQuery(patch: Partial<Record<'state' | 'project', string | number | null>>) {
  const q: LocationQueryRaw = { ...route.query }
  for (const [k, v] of Object.entries(patch)) {
    if (v === null || v === '' || v === 0) delete q[k]
    else q[k] = String(v)
  }
  void router.replace({ query: q })
}

const runs = ref<ReleaseRun[]>([])
const loaded = ref(false)
const error = ref('')
const status = ref(0)
async function load() {
  const r = await api.runs({ project: filters.value.project || undefined, state: filters.value.state || undefined, limit: 200 })
  loaded.value = true
  status.value = r.status
  if (!r.ok) {
    error.value = r.error
    return
  }
  error.value = ''
  runs.value = r.data ?? []
}
watch(filters, () => {
  loaded.value = false
  void load()
}, { deep: true })
onMounted(load)
watch(
  () => app.dataVersion,
  () => {
    if (app.lastChanges === null) void load()
  },
)

// Live: autopilot.run patches the row, adds a new matching run on top, drops one that left the filter.
function matches(r: ReleaseRun) {
  return (!filters.value.state || r.state === filters.value.state) && (!filters.value.project || r.projectId === filters.value.project)
}
watch(
  () => app.lastRun,
  (v: RunView | null) => {
    if (!v) return
    const r = v.run
    const i = runs.value.findIndex((x) => x.id === r.id)
    const next = runs.value.slice()
    if (i >= 0) {
      if (matches(r)) next[i] = r
      else next.splice(i, 1)
    } else if (matches(r)) {
      next.unshift(r)
      next.sort((a, b) => b.id - a.id)
    } else return
    runs.value = next
  },
  { flush: 'sync' },
)

/** GitHub code projects (releases belong to them). */
const codeProjects = computed(() => app.repos.filter((r) => r.platform === 'github' && !r.linkedTo))
const projectOptions = computed(() => [
  { label: t('issues.allProjects'), value: 0, platform: '' },
  ...codeProjects.value.map((r) => ({ label: r.name, value: r.id, platform: r.platform })),
])
const selectedProject = computed(() => projectOptions.value.find((o) => o.value === filters.value.project))
const stateOptions = computed(() => [{ label: t('release.allStates'), value: '' }, ...RUN_STATES.map((s) => ({ label: t('release.state.' + s), value: s }))])
const anyFilter = computed(() => !!(filters.value.state || filters.value.project))
const projectName = (r: ReleaseRun) => r.manifest?.repo || app.repos.find((x) => x.id === r.projectId)?.name || '#' + r.projectId
const versionText = (r: ReleaseRun) => {
  const from = r.manifest?.fromVersion
  const to = r.version || r.manifest?.version || ''
  return from && to ? `${from} → ${to}` : to || '—'
}

// «Выпустить релиз»
const releaseOpen = ref(false)
const releaseProject = ref<{ id: number; name: string } | null>(null)
const pickProject = ref(0)
function openRelease() {
  const id = filters.value.project || pickProject.value
  const p = codeProjects.value.find((r) => r.id === id)
  if (!p) return
  releaseProject.value = { id: p.id, name: p.name }
  releaseOpen.value = true
}

function open(e: { data: ReleaseRun }) {
  void router.push(`/runs/${e.data.id}`)
}
</script>

<template>
  <div class="page">
    <ListPage
      :resettable="anyFilter"
      :total="loaded && !error ? runs.length : null"
      :total-label="t('release.words', runs.length)"
      :skeleton="!loaded"
      @reset="router.replace({ query: {} })"
    >
      <template #filters>
        <Select
          :model-value="filters.state"
          :options="stateOptions"
          option-label="label"
          option-value="value"
          :aria-label="t('release.stateLabel')"
          @update:model-value="(v: string) => setQuery({ state: v })"
        />
        <Select
          :model-value="filters.project"
          :options="projectOptions"
          option-label="label"
          option-value="value"
          filter
          :aria-label="t('issues.project')"
          @update:model-value="(v: number) => setQuery({ project: v })"
        >
          <template #value="{ placeholder }">
            <span
              v-if="selectedProject"
              class="opt"
            ><PlatformIcon
              v-if="selectedProject.platform"
              :platform="selectedProject.platform"
              :size="14"
            />{{ selectedProject.label }}</span>
            <span v-else>{{ placeholder }}</span>
          </template>
          <template #option="{ option }">
            <span class="opt"><PlatformIcon
              v-if="option.platform"
              :platform="option.platform"
              :size="14"
            />{{ option.label }}</span>
          </template>
        </Select>
      </template>
      <template #actions>
        <Select
          v-if="!filters.project"
          v-model="pickProject"
          :options="codeProjects"
          option-label="name"
          option-value="id"
          filter
          :placeholder="t('release.pickProject')"
          :aria-label="t('release.pickProject')"
          class="pick"
        />
        <Button
          :label="t('release.action')"
          icon="pi pi-send"
          :disabled="!(filters.project || pickProject)"
          @click="openRelease"
        />
      </template>

      <ReleaseDialog
        v-model:visible="releaseOpen"
        :project="releaseProject"
      />

      <div class="panel table-panel">
        <DataTable
          :value="runs"
          data-key="id"
          row-hover
          class="runs"
          @row-click="open"
        >
          <template #empty>
            <EmptyState
              v-if="error && status === 404"
              icon="pi pi-server"
              :title="t('release.unavailable')"
              :text="t('release.unavailableText')"
            />
            <EmptyState
              v-else-if="error"
              icon="pi pi-exclamation-triangle"
              :title="t('release.loadError')"
              :text="error"
            >
              <Button
                :label="t('common.retry')"
                icon="pi pi-refresh"
                size="small"
                @click="load()"
              />
            </EmptyState>
            <EmptyState
              v-else-if="anyFilter"
              icon="pi pi-filter"
              :title="t('release.noMatch')"
              :text="t('issues.noMatchText')"
            />
            <EmptyState
              v-else
              icon="pi pi-send"
              :title="t('release.empty')"
              :text="t('release.emptyText')"
            />
          </template>
          <Column :header="t('release.colState')">
            <template #body="{ data }: { data: ReleaseRun }">
              <RunBadge :state="data.state" />
            </template>
          </Column>
          <Column :header="t('release.colProject')">
            <template #body="{ data }: { data: ReleaseRun }">
              <RouterLink
                :to="`/runs/${data.id}`"
                class="proj"
                @click.stop
              >
                <span class="mono muted">#{{ data.id }}</span> {{ projectName(data) }}
              </RouterLink>
              <div
                v-if="data.state === 'held' && data.heldReason"
                class="held"
              >
                <i class="pi pi-pause-circle" /> {{ heldText(data.heldReason, data.manifest?.targets ?? null) }}
              </div>
            </template>
          </Column>
          <Column :header="t('release.colVersion')">
            <template #body="{ data }: { data: ReleaseRun }">
              <span class="mono">{{ versionText(data) }}</span>
            </template>
          </Column>
          <Column
            :header="t('release.colOrigin')"
            class="c-origin"
          >
            <template #body="{ data }: { data: ReleaseRun }">
              <span class="muted">{{ t('release.origin.' + data.origin) }}</span>
            </template>
          </Column>
          <Column :header="t('release.colTime')">
            <template #body="{ data }: { data: ReleaseRun }">
              <span
                v-tooltip.top="absTime(data.updatedAt || data.createdAt)"
                class="muted"
              >{{ relTime(data.updatedAt || data.createdAt) }}</span>
            </template>
          </Column>
        </DataTable>
      </div>
    </ListPage>
  </div>
</template>

<style scoped>
.opt {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.pick {
  width: 220px;
}

.table-panel {
  overflow: hidden;
}

.runs :deep(tr) {
  cursor: pointer;
}

.proj {
  color: var(--iw-text);
  font-weight: 500;
}

.proj:hover {
  color: var(--iw-primary);
}

.held {
  margin-top: 2px;
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-warn);
}

@media (width <= 767px) {
  .runs :deep(.c-origin) {
    display: none;
  }

  .pick {
    width: 160px;
  }
}
</style>
