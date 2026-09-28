<script setup lang="ts">
import { computed } from 'vue'
import { siClaude, siCurseforge, siGithub, siSteam } from 'simple-icons'

// Platform marks from simple-icons (CC0). Nexus Mods and Codex have no mark
// there, so they get a neutral monogram tile instead of a copied logo.
const props = withDefaults(defineProps<{ platform: string; size?: number; tile?: boolean }>(), { size: 20, tile: false })

const marks: Record<string, { path?: string; hex: string; mono?: string; title: string }> = {
  github: { path: siGithub.path, hex: '#' + siGithub.hex, title: 'GitHub' },
  curseforge: { path: siCurseforge.path, hex: '#' + siCurseforge.hex, title: 'CurseForge' },
  steam: { path: siSteam.path, hex: '#' + siSteam.hex, title: 'Steam Workshop' },
  claude: { path: siClaude.path, hex: '#' + siClaude.hex, title: 'Claude Code' },
  nexusmods: { hex: '#DA8E35', mono: 'N', title: 'Nexus Mods' },
  codex: { hex: '#10A37F', mono: '>_', title: 'Codex' },
}

const mark = computed(() => marks[props.platform] ?? { hex: '#627386', mono: props.platform.slice(0, 1).toUpperCase(), title: props.platform })
// GitHub's mark is black: draw it in the text colour so it shows on dark.
const fill = computed(() => (props.platform === 'github' || props.platform === 'steam' ? 'currentColor' : mark.value.hex))
</script>

<template>
  <span
    class="pi-mark"
    :class="{ tile }"
    :style="{ '--size': size + 'px', '--brand': mark.hex }"
    :title="mark.title"
    role="img"
    :aria-label="mark.title"
  >
    <svg
      v-if="mark.path"
      viewBox="0 0 24 24"
      :width="tile ? size * 0.55 : size"
      :height="tile ? size * 0.55 : size"
      aria-hidden="true"
    ><path
      :d="mark.path"
      :fill="fill"
    /></svg>
    <span
      v-else
      class="mono-mark"
    >{{ mark.mono }}</span>
  </span>
</template>

<style scoped>
.pi-mark {
  display: inline-grid;
  place-items: center;
  width: var(--size);
  height: var(--size);
  flex: none;
  color: var(--iw-text);
}

.pi-mark.tile {
  border-radius: 12px;
  background: color-mix(in srgb, var(--brand) 16%, var(--iw-elevated));
  border: 1px solid color-mix(in srgb, var(--brand) 30%, var(--iw-border));
}

.mono-mark {
  font-family: var(--iw-mono);
  font-weight: 700;
  font-size: calc(var(--size) * 0.42);
  color: var(--brand);
  line-height: 1;
}

.pi-mark:not(.tile) .mono-mark {
  display: grid;
  place-items: center;
  width: 100%;
  height: 100%;
  border-radius: 6px;
  background: color-mix(in srgb, var(--brand) 18%, transparent);
  font-size: calc(var(--size) * 0.5);
}
</style>
