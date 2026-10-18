<script setup lang="ts">
interface Props {
  title: string
  description?: string
  glyph?: string
}
withDefaults(defineProps<Props>(), { glyph: '◌' })
</script>

<template>
  <div class="empty">
    <div class="empty__glyph text-display">{{ glyph }}</div>
    <h3 class="empty__title">{{ title }}</h3>
    <p v-if="description" class="empty__desc text-muted">{{ description }}</p>
    <div v-if="$slots.action" class="empty__action"><slot name="action" /></div>
  </div>
</template>

<style scoped>
.empty {
  text-align: center;
  padding: var(--space-7) var(--space-5);
  border: 1px dashed var(--border);
  border-radius: var(--radius-lg);
  background: var(--surface);
}
.empty__glyph {
  font-size: var(--fs-48);
  color: var(--muted-2);
  margin-bottom: var(--space-3);
}
.empty__title { margin: 0 0 var(--space-2); font-size: var(--fs-16); }
.empty__desc { margin: 0 auto var(--space-4); max-width: 360px; }
.empty__action { margin-top: var(--space-4); }
</style>