<script setup lang="ts">
// Placeholder for the metrics view; the rich version lives in views/MetricsView.tsx
// after T05 lands the BreathingLight + LatencyHeartbeat fx components.
import { useI18n } from 'vue-i18n'
const { t } = useI18n()
</script>

<template>
  <section class="metrics-placeholder">
    <h1>{{ t('metrics.title') }}</h1>
    <p class="text-muted">{{ t('metrics.placeholder') }}</p>
  </section>
</template>

<style scoped>
.metrics-placeholder {
  padding: var(--space-6) 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
</style>