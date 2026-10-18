<script setup lang="ts">
/**
 * Headline strip for the run-history page. Combines four primitive numbers
 * (totals / failures / success rate / p95) with the single-line latency
 * sparkline. Designed to be the first thing the eye lands on — it answers
 * "how is this task trending?" in one glance.
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Sparkline from './Sparkline.vue'
import type { ProbeRun } from '../../api/runs'

const props = defineProps<{ runs: ProbeRun[] }>()
const { t, n: tn } = useI18n()

const stats = computed(() => {
  const runs = props.runs
  const total = runs.length
  let failures = 0
  let unauth = 0
  const durations: number[] = []
  for (const r of runs) {
    if (r.status === 'failure') failures++
    if (r.status === 'unauthorized') unauth++
    durations.push(r.duration_ms)
  }
  const ok = total - failures - unauth
  const successRate = total === 0 ? 0 : (ok / total) * 100
  // p95: simple linear interpolation
  const sorted = [...durations].sort((a, b) => a - b)
  const p95 = sorted.length === 0 ? 0 : sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * 0.95))]
  return { total, ok, failures, unauth, successRate, p95 }
})

// new → old, but sparkline reads left→right as "older → newer", so reverse.
const sparkValues = computed(() => [...props.runs].reverse().map((r) => r.duration_ms))

const trendTone = computed(() => {
  if (stats.value.failures > 0 && stats.value.successRate < 90) return 'var(--danger)'
  if (stats.value.unauth > 0) return 'var(--warn)'
  return 'var(--ok)'
})
</script>

<template>
  <section class="timeline">
    <div class="timeline__meta">
      <h2 class="timeline__title">{{ t('runs.timeline.title') }}</h2>
      <p class="timeline__subtitle text-muted">{{ t('runs.timeline.subtitle', { n: stats.total }) }}</p>
    </div>

    <ul class="timeline__kpis">
      <li class="timeline__kpi">
        <span class="timeline__kpi-label">{{ t('runs.timeline.total') }}</span>
        <span class="timeline__kpi-value text-mono">{{ tn(stats.total) }}</span>
      </li>
      <li class="timeline__kpi">
        <span class="timeline__kpi-label">{{ t('runs.timeline.success') }}</span>
        <span class="timeline__kpi-value text-mono" :data-tone="stats.successRate >= 95 ? 'ok' : stats.successRate >= 80 ? 'warn' : 'danger'">
          {{ stats.successRate.toFixed(1) }}<small>%</small>
        </span>
      </li>
      <li class="timeline__kpi">
        <span class="timeline__kpi-label">{{ t('runs.timeline.failures') }}</span>
        <span class="timeline__kpi-value text-mono" :data-tone="stats.failures > 0 ? 'danger' : 'muted'">
          {{ tn(stats.failures) }}<small v-if="stats.unauth > 0"> +{{ stats.unauth }}⊘</small>
        </span>
      </li>
      <li class="timeline__kpi">
        <span class="timeline__kpi-label">{{ t('runs.timeline.p95') }}</span>
        <span class="timeline__kpi-value text-mono">{{ tn(stats.p95) }}<small>ms</small></span>
      </li>
    </ul>

    <figure class="timeline__chart" :style="{ color: trendTone }">
      <Sparkline :values="sparkValues" :line-color="trendTone" />
      <figcaption class="timeline__legend text-muted">
        <span>{{ t('runs.timeline.chartOld') }}</span>
        <span class="timeline__legend-axis" aria-hidden="true">→</span>
        <span>{{ t('runs.timeline.chartNew') }}</span>
        <span class="timeline__legend-peaks text-mono">● {{ t('runs.timeline.peak') }}</span>
      </figcaption>
    </figure>
  </section>
</template>

<style scoped>
.timeline {
  display: grid;
  grid-template-columns: 1fr;
  gap: var(--space-4);
  padding: var(--space-5);
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
}
.timeline__meta { display: flex; flex-direction: column; gap: 2px; }
.timeline__title { margin: 0; font-size: var(--fs-20); font-family: var(--font-display); letter-spacing: -0.01em; }
.timeline__subtitle { margin: 0; font-size: var(--fs-13); }

.timeline__kpis {
  list-style: none;
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: var(--space-3);
  margin: 0;
  padding: 0;
}
.timeline__kpi {
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: var(--space-3) var(--space-4);
  background: var(--surface-2);
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.timeline__kpi-label {
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--muted);
}
.timeline__kpi-value {
  font-size: var(--fs-24);
  font-weight: var(--fw-semibold);
  color: var(--text);
  line-height: 1;
}
.timeline__kpi-value[data-tone='ok']     { color: var(--ok); }
.timeline__kpi-value[data-tone='warn']   { color: var(--warn); }
.timeline__kpi-value[data-tone='danger'] { color: var(--danger); }
.timeline__kpi-value[data-tone='muted']  { color: var(--muted); }
.timeline__kpi-value small {
  font-size: 0.55em;
  color: var(--muted);
  font-weight: var(--fw-regular);
  margin-left: 2px;
}

.timeline__chart {
  margin: 0;
  padding-top: var(--space-3);
  border-top: 1px dashed var(--border);
}
.timeline__legend {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  margin-top: var(--space-2);
}
.timeline__legend-axis { color: var(--muted-2); font-size: var(--fs-13); }
.timeline__legend-peaks {
  margin-left: auto;
  color: var(--accent);
}

@media (max-width: 880px) {
  .timeline__kpis { grid-template-columns: repeat(2, 1fr); }
}
</style>