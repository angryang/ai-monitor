<script setup lang="ts">
import { onMounted, onBeforeUnmount, watch } from 'vue'

interface Props {
  open: boolean
  title?: string
  side?: 'right' | 'left'
}
const props = withDefaults(defineProps<Props>(), { side: 'right' })
const emit = defineEmits<{ close: [] }>()

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape' && props.open) emit('close')
}
onMounted(() => window.addEventListener('keydown', onKey))
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))

watch(() => props.open, (v) => {
  document.body.style.overflow = v ? 'hidden' : ''
})
</script>

<template>
  <div v-if="open" class="drawer" :class="`drawer--${side}`" role="dialog">
    <div class="drawer__panel">
      <header class="drawer__head">
        <h3 class="drawer__title text-display">{{ title }}</h3>
        <button class="drawer__close" type="button" aria-label="close" @click="$emit('close')">×</button>
      </header>
      <div class="drawer__body"><slot /></div>
    </div>
    <div class="drawer__scrim" @click="$emit('close')" />
  </div>
</template>

<style scoped>
.drawer {
  position: fixed;
  inset: 0;
  display: flex;
  z-index: 40;
}
.drawer--right { justify-content: flex-end; }
.drawer--left { justify-content: flex-start; }
.drawer__panel {
  position: relative;
  width: min(420px, calc(100vw - var(--space-6)));
  background: var(--surface);
  border-left: 1px solid var(--border-strong);
  display: flex;
  flex-direction: column;
  box-shadow: var(--shadow-2);
  animation: drawer-rise var(--dur-base) var(--ease-spring);
  z-index: 1;
}
.drawer--left .drawer__panel {
  border-left: 0;
  border-right: 1px solid var(--border-strong);
}
.drawer__scrim {
  position: absolute;
  inset: 0;
  background: rgba(0, 0, 0, 0.4);
  z-index: 0;
}
.drawer__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--space-4) var(--space-5);
  border-bottom: 1px solid var(--border);
}
.drawer__title { margin: 0; font-size: var(--fs-14); }
.drawer__close {
  background: transparent;
  border: 0;
  color: var(--muted);
  font-size: var(--fs-20);
  cursor: pointer;
}
.drawer__body {
  padding: var(--space-5);
  overflow: auto;
  flex: 1;
}
@keyframes drawer-rise {
  from { transform: translate3d(20px, 0, 0); opacity: 0; }
  to { transform: translate3d(0, 0, 0); opacity: 1; }
}
.drawer--left .drawer__panel { animation-name: drawer-rise-left; }
@keyframes drawer-rise-left {
  from { transform: translate3d(-20px, 0, 0); opacity: 0; }
  to { transform: translate3d(0, 0, 0); opacity: 1; }
}
@media (prefers-reduced-motion: reduce) {
  .drawer__panel { animation: none; }
}
</style>