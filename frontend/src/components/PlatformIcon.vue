<script setup lang="ts">
import { computed } from 'vue'
import { siClaude, siCurseforge, siGithub, siSteam } from 'simple-icons'

// Platform marks. GitHub, CurseForge, Steam and Claude: simple-icons (CC0 path
// data). Nexus Mods has no simple-icons entry: its official mark (the orange
// pinwheel) comes from Nexus Mods' own Vortex repo, icons/custom/nexus.svg,
// without the dark outline stroke. Marks are shown only to name the platform.
// Codex has no published mark, so it keeps a neutral monogram tile.
const props = withDefaults(defineProps<{ platform: string; size?: number; tile?: boolean }>(), { size: 20, tile: false })

interface Mark {
  title: string
  hex: string
  viewBox?: string
  /** fill '' = the brand colour ('currentColor' for the black GitHub/Steam marks). */
  paths?: { d: string; fill?: string }[]
  mono?: string
}

const si = (icon: { path: string; hex: string }, title: string, fill = ''): Mark => ({ title, hex: '#' + icon.hex, viewBox: '0 0 24 24', paths: [{ d: icon.path, fill }] })

const NEXUS_DARK = '#b4762c'
const nexus: Mark = {
  title: 'Nexus Mods',
  hex: '#DA8E35',
  viewBox: '13 13 137 137',
  paths: [
    { fill: NEXUS_DARK, d: 'M 56.3,88.4 57,116.7 50,111 c -7.8,12.7 -10.3,25 -6.6,34.1 l 1.3,3.2 -3.2,-1.4 c -7.3,-3.2 -13.9,-7.7 -19.4,-13.5 l -0.3,-0.3 -0.1,-0.5 c -0.4,-3.5 -0.2,-7.3 0.7,-11.2 l 0,-0.1 c 1.3,-4.9 3.2,-9.8 5.6,-14.7 1.5,-3.1 3.3,-6.2 5.3,-9.2 l -6.1,-5 29.1,-4 z' },
    { fill: NEXUS_DARK, d: 'm 105.9,74.1 -0.7,-28.3 7,5.7 c 7.8,-12.7 10.3,-25 6.6,-34.1 l -1.3,-3.2 3.2,1.4 c 7.3,3.2 13.9,7.7 19.4,13.5 l 0.3,0.3 0.1,0.5 c 0.4,3.5 0.2,7.3 -0.7,11.2 l 0,0.1 c -1.3,4.9 -3.2,9.8 -5.6,14.7 -1.5,3.1 -3.3,6.2 -5.3,9.2 l 6.1,5 -29.1,4 z' },
    { fill: NEXUS_DARK, d: 'm 88.5,105.4 28.3,-0.7 -5.7,7 c 12.7,7.8 25,10.3 34.1,6.6 l 3.2,-1.3 -1.4,3.2 c -3.2,7.3 -7.7,13.9 -13.5,19.4 l -0.3,0.3 -0.5,0.1 c -3.5,0.4 -7.3,0.2 -11.2,-0.7 l -0.1,0 c -4.9,-1.3 -9.8,-3.2 -14.7,-5.6 -3.1,-1.5 -6.2,-3.3 -9.2,-5.3 l -5,6.1 -4,-29.1 z' },
    { fill: NEXUS_DARK, d: 'm 74.1,57.6 -28.3,0.7 5.7,-7 C 38.8,43.5 26.5,41 17.4,44.7 L 14.3,46 15.7,42.8 C 18.9,35.5 23.4,28.9 29.2,23.4 L 29.5,23.1 30,23 c 3.5,-0.4 7.3,-0.2 11.2,0.7 l 0.1,0 c 4.9,1.3 9.8,3.2 14.7,5.6 3.1,1.5 6.2,3.3 9.2,5.3 l 5,-6.1 3.9,29.1 z' },
    { fill: '#DA8E35', d: 'M 20.9,80.8 a 60.5,60.5 0 1 0 121,0 a 60.5,60.5 0 1 0 -121,0 z' },
    { fill: '#fff', d: 'M 59.3,59.5 C 55.8,57.9 53.2,56.3 50.6,54.4 46.6,51.6 42.9,48.5 39.8,45.2 32.2,37.5 28.2,29.6 29.3,23.1 L 27,25.6 c -5.5,5.8 -12.8,16 -12.9,20.4 0.1,0.5 0.1,0.5 0.1,0.5 1,3.4 2.6,6.8 4.9,10.1 l 0,0.1 c 3,4.8 8.9,12.7 29.9,21.9 l -3.7,7 28.3,-7.6 -10.1,-26.5 -4.2,8 z' },
    { fill: '#fff', d: 'm 103.3,103.5 c 3.5,1.6 6.1,3.2 8.7,5.1 4,2.8 7.7,5.9 10.8,9.2 7.6,7.7 11.6,15.6 10.5,22.1 l 2.3,-2.4 c 5.5,-5.8 12.8,-16 12.9,-20.4 -0.1,-0.5 -0.1,-0.5 -0.1,-0.5 -1,-3.4 -2.6,-6.8 -4.9,-10.1 l 0,-0.1 c -3,-4.8 -8.9,-12.7 -29.9,-21.9 l 3.7,-7 -28.3,7.6 10.2,26.2 4.1,-7.8 z' },
    { fill: '#fff', d: 'm 104,59.3 c 1.6,-3.5 3.2,-6.1 5.1,-8.7 2.8,-4 5.9,-7.7 9.2,-10.8 7.7,-7.6 15.6,-11.6 22.1,-10.5 L 138,27 c -5.8,-5.5 -16,-12.8 -20.4,-12.9 -0.5,0.1 -0.5,0.1 -0.5,0.1 -3.4,1 -6.8,2.6 -10.1,4.9 l -0.1,0 C 102.1,22.1 94.2,28 85,49 l -7,-3.7 7.6,28.3 26.4,-10 -8,-4.3 z' },
    { fill: '#fff', d: 'm 58.2,103.2 c -1.6,3.5 -3.2,6.1 -5.1,8.7 -2.8,4 -5.9,7.7 -9.2,10.8 -7.7,7.6 -15.6,11.6 -22.1,10.5 l 2.4,2.3 c 5.8,5.5 16,12.8 20.4,12.9 0.5,-0.1 0.5,-0.1 0.5,-0.1 3.4,-1 6.8,-2.6 10.1,-4.9 l 0.1,0 c 4.8,-3 12.7,-8.9 21.9,-29.9 l 7,3.7 -7.6,-28.3 -26.3,10 7.9,4.3 z' },
  ],
}

const marks: Record<string, Mark> = {
  github: si(siGithub, 'GitHub', 'currentColor'),
  curseforge: si(siCurseforge, 'CurseForge'),
  steam: si(siSteam, 'Steam Workshop', 'currentColor'),
  claude: si(siClaude, 'Claude Code'),
  nexus,
  nexusmods: nexus,
  codex: { hex: '#10A37F', mono: '>_', title: 'Codex' },
}

const mark = computed<Mark>(() => marks[props.platform] ?? { hex: '#627386', mono: props.platform.slice(0, 1).toUpperCase(), title: props.platform })
const svgSize = computed(() => (props.tile ? props.size * 0.55 : props.size))
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
      v-if="mark.paths"
      :viewBox="mark.viewBox"
      :width="svgSize"
      :height="svgSize"
      aria-hidden="true"
    ><path
      v-for="(p, i) in mark.paths"
      :key="i"
      :d="p.d"
      :fill="p.fill || mark.hex"
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
