<script setup lang="ts">
/**
 * Compact single-line sparkline for time-series values. Inline SVG, zero
 * dependencies — drops into any container, inherits currentColor for the
 * line and a translucent stroke for the area fill.
 *
 * Each value becomes one column; the canvas width auto-fits the value
 * count via the SVG viewBox. A peak dot is drawn above the max so the eye
 * snaps to it without scanning.
 */
import { computed } from 'vue'

const props = defineProps({
  values: { type: Array as () => number[], required: true },
  /** Optional baseline (e.g. 0). If null, the SVG floor is the min of values. */
  baseline: { type: Number, default: null },
  /** Stroke color for the line. Defaults to currentColor (lets parent set tone). */
  lineColor: { type: String, default: 'currentColor' },
  /** Peak dot color. Defaults to the accent token. */
  peakColor: { type: String, default: 'var(--accent)' },
  height: { type: Number, default: 56 },
})

const VIEW_W = 600
const VIEW_H = 56

function buildPath(values: number[], baseline: number | null): { line: string; area: string } {
  if (values.length === 0) return { line: '', area: '' }
  const max = Math.max(...values)
  const min = baseline != null ? baseline : Math.min(...values)
  const range = Math.max(1, max - min)
  const stepX = VIEW_W / Math.max(1, values.length - 1)
  const pts = values.map((v, i) => {
    const x = i * stepX
    const y = VIEW_H - ((v - min) / range) * (VIEW_H - 4) - 2
    return [x, y] as const
  })
  const line = pts.map(([x, y], i) => `${i === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`).join(' ')
  const area = `${line} L${VIEW_W},${VIEW_H} L0,${VIEW_H} Z`
  return { line, area }
}

const path = computed(() => buildPath(props.values, props.baseline ?? null))

const peak = computed(() => {
  if (props.values.length === 0) return null
  const max = Math.max(...props.values)
  const idx = props.values.indexOf(max)
  const min = props.baseline ?? Math.min(...props.values)
  const range = Math.max(1, max - min)
  const stepX = VIEW_W / Math.max(1, props.values.length - 1)
  const x = idx * stepX
  const y = VIEW_H - ((max - min) / range) * (VIEW_H - 4) - 2
  return { x, y, value: max }
})
</script>

<template>
  <svg
    class="sparkline"
    :viewBox="`0 0 ${VIEW_W} ${VIEW_H}`"
    preserveAspectRatio="none"
    role="img"
    aria-label="recent run latency trend"
  >
    <path
      class="sparkline__area"
      :d="path.area"
      :style="{ fill: lineColor, opacity: 0.12 }"
    />
    <path
      class="sparkline__line"
      :d="path.line"
      :style="{ stroke: lineColor }"
      fill="none"
      stroke-width="1.5"
      vector-effect="non-scaling-stroke"
    />
    <circle
      v-if="peak"
      class="sparkline__peak"
      :cx="peak.x"
      :cy="peak.y"
      r="3"
      :fill="peakColor"
    />
  </svg>
</template>

<style scoped>
.sparkline {
  display: block;
  width: 100%;
  height: v-bind('props.height + "px"');
  font-variant-numeric: tabular-nums;
}
.sparkline__peak {
  filter: drop-shadow(0 0 6px var(--accent));
}
@media (prefers-reduced-motion: reduce) {
  .sparkline__area,
  .sparkline__line { transition: none; }
}
</style>