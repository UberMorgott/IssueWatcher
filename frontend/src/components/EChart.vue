<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { init, use, type ECharts, type EChartsCoreOption } from 'echarts/core'
import { BarChart, LineChart, PieChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { SVGRenderer } from 'echarts/renderers'
import { themeVersion } from '../lib/appearance'

use([BarChart, LineChart, PieChart, GridComponent, LegendComponent, TooltipComponent, SVGRenderer])

/** Colours resolved from the CSS tokens, so charts follow the theme. */
export interface ChartTheme {
  text: string
  muted: string
  border: string
  surface: string
  opened: string
  closed: string
  warn: string
  /** Accent for series that are not opened/closed flows (per-project bars). */
  primary: string
}

const props = defineProps<{ option: (t: ChartTheme) => EChartsCoreOption; height?: string; label: string }>()
const el = ref<HTMLDivElement>()
let chart: ECharts | undefined
let ro: ResizeObserver | undefined

function themeColors(): ChartTheme {
  const s = getComputedStyle(document.documentElement)
  const v = (n: string) => s.getPropertyValue(n).trim()
  return {
    text: v('--iw-text'),
    muted: v('--iw-muted'),
    border: v('--iw-border'),
    surface: v('--iw-surface'),
    opened: v('--iw-chart-opened'),
    closed: v('--iw-chart-closed'),
    warn: v('--iw-warn'),
    primary: v('--iw-primary'),
  }
}

function render() {
  if (!chart) return
  chart.setOption(props.option(themeColors()), { notMerge: true })
}

onMounted(() => {
  if (!el.value) return
  chart = init(el.value, undefined, { renderer: 'svg' })
  render()
  ro = new ResizeObserver(() => chart?.resize())
  ro.observe(el.value)
})

watch(() => props.option, render)
// Palette, mode or custom colours changed: re-read the tokens.
watch(themeVersion, () => requestAnimationFrame(render))

onBeforeUnmount(() => {
  ro?.disconnect()
  chart?.dispose()
})
</script>

<template>
  <div
    ref="el"
    class="echart"
    role="img"
    :aria-label="label"
    :style="{ height: height ?? '280px' }"
  />
</template>

<style scoped>
.echart {
  width: 100%;
}
</style>
