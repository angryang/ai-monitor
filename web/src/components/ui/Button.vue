<script setup lang="ts">
interface Props {
  variant?: 'primary' | 'ghost' | 'danger' | 'success'
  size?: 'sm' | 'md'
  type?: 'button' | 'submit' | 'reset'
  disabled?: boolean
  loading?: boolean
  block?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  variant: 'primary',
  size: 'md',
  type: 'button',
  disabled: false,
  loading: false,
  block: false,
})

defineEmits<{ click: [event: MouseEvent] }>()
const _ = props
</script>

<template>
  <button
    :type="type"
    :disabled="disabled || loading"
    :class="['btn', `btn--${variant}`, `btn--${size}`, block && 'btn--block', loading && 'btn--loading']"
    @click="$emit('click', $event)"
  >
    <span v-if="loading" class="btn__spinner" aria-hidden="true" />
    <span class="btn__label"><slot /></span>
  </button>
</template>

<style scoped>
.btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: var(--space-2);
  border-radius: var(--radius-md);
  font-weight: var(--fw-semibold);
  font-family: var(--font-body);
  cursor: pointer;
  transition: background var(--dur-fast) var(--ease-out), color var(--dur-fast) var(--ease-out),
    border-color var(--dur-fast) var(--ease-out), box-shadow var(--dur-fast) var(--ease-out);
  border: 1px solid transparent;
  user-select: none;
}

.btn--sm { padding: var(--space-1) var(--space-3); font-size: var(--fs-12); }
.btn--md { padding: var(--space-2) var(--space-4); font-size: var(--fs-13); }

.btn--primary {
  background: var(--accent);
  color: var(--bg);
  box-shadow: var(--shadow-1);
}
.btn--primary:hover:not(:disabled) {
  background: var(--accent-2);
  box-shadow: var(--shadow-glow);
}

.btn--ghost {
  background: transparent;
  color: var(--text);
  border-color: var(--border-strong);
}
.btn--ghost:hover:not(:disabled) {
  background: var(--surface-2);
}

.btn--danger {
  background: var(--danger);
  color: var(--text);
}
.btn--danger:hover:not(:disabled) { filter: brightness(1.1); }

.btn--success {
  background: var(--ok);
  color: var(--bg);
}

.btn--block { width: 100%; }

.btn:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}

.btn__spinner {
  width: 12px;
  height: 12px;
  border: 2px solid currentColor;
  border-top-color: transparent;
  border-radius: 50%;
  animation: btn-spin 0.6s linear infinite;
}

@keyframes btn-spin {
  to { transform: rotate(360deg); }
}

@media (prefers-reduced-motion: reduce) {
  .btn__spinner { animation: none; }
}
</style>