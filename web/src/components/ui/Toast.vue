<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useToast } from '../../utils/toast'

const { t } = useI18n()
const { items, dismiss } = useToast()
</script>

<template>
  <div class="toast-host" aria-live="polite" aria-atomic="false">
    <transition-group name="toast" tag="div" class="toast-stack">
      <div
        v-for="t in items"
        :key="t.id"
        class="toast"
        :data-kind="t.kind"
        role="status"
      >
        <div class="toast__bar" />
        <div class="toast__body">
          <p class="toast__title">{{ t.title }}</p>
          <p v-if="t.detail" class="toast__detail text-muted">{{ t.detail }}</p>
        </div>
        <button class="toast__close" type="button" :aria-label="$t('common.dismiss')" @click="dismiss(t.id)">×</button>
      </div>
    </transition-group>
  </div>
</template>

<style scoped>
.toast-host {
  position: fixed;
  right: var(--space-5);
  bottom: var(--space-5);
  z-index: 60;
  pointer-events: none;
}
.toast-stack {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  pointer-events: auto;
}
.toast {
  display: flex;
  align-items: stretch;
  width: min(360px, calc(100vw - var(--space-5) * 2));
  background: var(--surface);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-2);
  overflow: hidden;
}
.toast__bar {
  width: 4px;
  background: var(--accent);
}
.toast[data-kind='success'] .toast__bar { background: var(--ok); }
.toast[data-kind='warn'] .toast__bar { background: var(--warn); }
.toast[data-kind='error'] .toast__bar { background: var(--danger); }
.toast__body { padding: var(--space-3) var(--space-4); flex: 1; }
.toast__title { margin: 0; font-size: var(--fs-13); font-weight: var(--fw-semibold); }
.toast__detail { margin: var(--space-1) 0 0; font-size: var(--fs-12); }
.toast__close {
  background: transparent;
  border: 0;
  color: var(--muted);
  font-size: var(--fs-20);
  padding: 0 var(--space-3);
  cursor: pointer;
}
.toast-enter-active,
.toast-leave-active {
  transition: opacity var(--dur-base) var(--ease-out),
    transform var(--dur-base) var(--ease-out);
}
.toast-enter-from,
.toast-leave-to {
  opacity: 0;
  transform: translate3d(0, 12px, 0);
}
</style>