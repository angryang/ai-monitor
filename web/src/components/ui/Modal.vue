<script setup lang="ts">
import { onMounted, onBeforeUnmount, watch } from 'vue'

interface Props {
  open: boolean
  title: string
  size?: 'sm' | 'md' | 'lg'
}
const props = withDefaults(defineProps<Props>(), { size: 'md' })
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
  <div v-if="open" class="modal" role="dialog" aria-modal="true" @click.self="$emit('close')">
    <div class="modal__panel" :class="`modal__panel--${size}`">
      <header class="modal__head">
        <h3 class="modal__title text-display">{{ title }}</h3>
        <button class="modal__close" type="button" aria-label="close" @click="$emit('close')">×</button>
      </header>
      <div class="modal__body"><slot /></div>
      <footer v-if="$slots.footer" class="modal__foot"><slot name="footer" /></footer>
    </div>
  </div>
</template>

<style scoped>
.modal {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.6);
  display: grid;
  place-items: center;
  z-index: 50;
  animation: modal-bg var(--dur-fast) var(--ease-out);
}
.modal__panel {
  background: var(--surface);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-2);
  width: min(560px, calc(100vw - var(--space-6)));
  display: flex;
  flex-direction: column;
  max-height: 80vh;
  animation: modal-rise var(--dur-base) var(--ease-spring);
}
.modal__panel--sm { width: min(360px, calc(100vw - var(--space-6))); }
.modal__panel--lg { width: min(800px, calc(100vw - var(--space-6))); }
.modal__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--space-4) var(--space-5);
  border-bottom: 1px solid var(--border);
}
.modal__title { margin: 0; font-size: var(--fs-16); }
.modal__close {
  background: transparent;
  border: 0;
  color: var(--muted);
  font-size: var(--fs-20);
  cursor: pointer;
}
.modal__body {
  padding: var(--space-5);
  overflow: auto;
}
.modal__foot {
  padding: var(--space-4) var(--space-5);
  border-top: 1px solid var(--border);
  display: flex;
  justify-content: flex-end;
  gap: var(--space-3);
}
@keyframes modal-bg { from { opacity: 0; } to { opacity: 1; } }
@keyframes modal-rise { from { opacity: 0; transform: translate3d(0, 12px, 0); } to { opacity: 1; transform: translate3d(0, 0, 0); } }
@media (prefers-reduced-motion: reduce) {
  .modal,
  .modal__panel { animation: none; }
}
</style>