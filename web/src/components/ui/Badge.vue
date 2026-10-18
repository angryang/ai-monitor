<script setup lang="ts">
interface Props {
  tone?: 'ok' | 'warn' | 'danger' | 'info' | 'muted' | 'accent'
  text?: string
}
withDefaults(defineProps<Props>(), { tone: 'muted' })
</script>

<template>
  <span class="badge" :data-tone="tone">
    <slot>{{ text }}</slot>
  </span>
</template>

<style scoped>
.badge {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  padding: 2px var(--space-2);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  border-radius: var(--radius-pill);
  border: 1px solid var(--border-strong);
  color: var(--muted);
  background: transparent;
}
.badge[data-tone='ok'] { color: var(--ok); border-color: rgba(20, 160, 109, 0.5); background: rgba(20, 160, 109, 0.08); }
.badge[data-tone='warn'] { color: var(--warn); border-color: rgba(214, 165, 20, 0.5); background: rgba(214, 165, 20, 0.08); }
.badge[data-tone='danger'] { color: var(--danger); border-color: rgba(216, 68, 63, 0.5); background: rgba(216, 68, 63, 0.08); }
.badge[data-tone='info'] { color: var(--info); border-color: rgba(91, 141, 239, 0.5); background: rgba(91, 141, 239, 0.08); }
.badge[data-tone='accent'] { color: var(--accent); border-color: var(--accent); background: var(--accent-soft); }
</style>