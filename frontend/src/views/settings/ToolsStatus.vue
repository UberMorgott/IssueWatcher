<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import Button from 'primevue/button'
import { useToast } from 'primevue/usetoast'
import { useI18n } from 'vue-i18n'
import { api } from '../../api/client'
import type { ToolStatus } from '../../api/types'

// Helpers next to the portable binary (data\tools): steamcmd and
// steam_api64.dll. The app sets them up by itself at startup; this shows
// their state and retries a failed or missing one on a click.
const emit = defineEmits<{ ready: [name: string] }>()
const toast = useToast()
const { t, te } = useI18n()

const list = ref<ToolStatus[]>([])
const open = ref(false)
const allReady = computed(() => list.value.length > 0 && list.value.every((x) => x.state === 'ready'))
const busy = ref('')
const error = ref('')
let timer: ReturnType<typeof setTimeout> | undefined

function label(name: string) {
  return te(`platforms.tools.name.${name}`) ? t(`platforms.tools.name.${name}`) : name
}

async function load() {
  const r = await api.tools()
  if (r.ok) {
    const before = new Map(list.value.map((x) => [x.name, x.state]))
    list.value = r.data.tools
    for (const x of r.data.tools) if (before.get(x.name) && before.get(x.name) !== 'ready' && x.state === 'ready') emit('ready', x.name)
  }
  clearTimeout(timer)
  if (list.value.some((x) => x.state === 'working')) timer = setTimeout(() => void load(), 1500)
}

async function provision(name: string) {
  busy.value = name
  error.value = ''
  const r = await api.provisionTool(name)
  busy.value = ''
  if (!r.ok) {
    const code = (r.body as { code?: unknown } | undefined)?.code
    error.value = typeof code === 'string' && te(`platforms.tools.errors.${code}`) ? t(`platforms.tools.errors.${code}`) : r.error
  } else {
    toast.add({ severity: 'success', summary: t('platforms.tools.ready', { name: label(name) }), life: 2500 })
    emit('ready', name)
  }
  await load()
}

onMounted(load)
onBeforeUnmount(() => clearTimeout(timer))
</script>

<template>
  <div
    v-if="list.length"
    class="tools"
    role="group"
    :aria-label="t('platforms.tools.title')"
  >
    <!-- all ready: one compact line; details (paths) on demand -->
    <div
      v-if="allReady && !open"
      class="row"
    >
      <span
        class="state ready"
        :title="list.map((x) => label(x.name)).join('\n')"
      ><i class="pi pi-check-circle" /> {{ t('platforms.tools.allReady') }}</span>
      <button
        type="button"
        class="link"
        :aria-expanded="false"
        @click="open = true"
      >
        {{ t('platforms.tools.details') }}
      </button>
    </div>
    <template v-else>
      <div class="row">
        <span class="label">{{ t('platforms.tools.title') }}</span>
        <button
          v-if="allReady"
          type="button"
          class="link"
          :aria-expanded="true"
          @click="open = false"
        >
          {{ t('platforms.tools.hide') }}
        </button>
      </div>
      <small class="muted">{{ t('platforms.tools.hint') }}</small>
      <div
        v-for="x in list"
        :key="x.name"
        class="row"
      >
        <span
          class="state"
          :class="x.state"
          :title="x.path || x.error || undefined"
        >
          <i
            class="pi"
            :class="{
              'pi-check-circle': x.state === 'ready',
              'pi-spin pi-spinner': x.state === 'working',
              'pi-exclamation-triangle': x.state === 'error',
              'pi-circle': x.state === 'missing',
            }"
          />
          {{ label(x.name) }} · {{ t('platforms.tools.state.' + x.state) }}
        </span>
        <Button
          v-if="x.state === 'missing' || x.state === 'error'"
          :label="t(x.state === 'error' ? 'platforms.tools.retry' : 'platforms.tools.provision')"
          icon="pi pi-download"
          size="small"
          severity="secondary"
          outlined
          :disabled="!!busy"
          :loading="busy === x.name"
          @click="provision(x.name)"
        />
        <small
          v-if="x.state === 'error' && x.error"
          class="err"
        >{{ x.error }}</small>
        <small
          v-else-if="x.state === 'ready' && x.path"
          class="muted mono path"
          :title="x.source ? t('platforms.tools.source', { source: x.source }) : undefined"
        >{{ x.path }}</small>
      </div>
    </template>
    <p
      v-if="error"
      class="err small"
    >
      <i class="pi pi-exclamation-triangle" /> {{ error }}
    </p>
  </div>
</template>

<style scoped>
.tools {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px 10px;
}

.state {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.state.ready {
  color: var(--iw-success);
}

.state.error {
  color: var(--iw-danger);
}

.label {
  font-size: calc(13px * var(--iw-fs, 1));
  font-weight: 500;
}

.link {
  padding: 0;
  border: 0;
  background: none;
  font: inherit;
  font-size: calc(12.5px * var(--iw-fs, 1));
  color: var(--iw-muted);
  text-decoration: underline;
  cursor: pointer;
}

.link:hover {
  color: var(--iw-text);
}

.path {
  flex-basis: 100%;
  overflow-wrap: anywhere;
}

.err {
  margin: 0;
  color: var(--iw-danger);
  overflow-wrap: anywhere;
}

.small {
  font-size: calc(12.5px * var(--iw-fs, 1));
}
</style>
