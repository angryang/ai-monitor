<script setup lang="ts">
/**
 * Theme switcher — single button that toggles between dark and light.
 * Sits next to the language switcher in TopBar. Reuses the same `.top__lang`
 * visual treatment (bordered pill, accent on hover) so the two controls read
 * as siblings rather than unrelated UI.
 *
 * The icon flips with the theme:
 *   - light mode active → show ☾ (click to go dark)
 *   - dark  mode active → show ☀ (click to go light)
 * The aria-label / title are i18n-driven so screen readers announce the
 * destination action, not the current state.
 *
 * `current` reads from `themeRef` (the reactive source of truth in theme.ts)
 * — not from `documentElement.dataset.theme` directly. The DOM is a pure
 * projection of the ref; Vue does not track DOMStringMap mutations, so
 * reading the attribute from a computed would freeze on its initial value
 * and `next` would always point the same way after the first click.
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { themeRef, setTheme, type AppTheme } from '../../theme'

const { t } = useI18n()

const current = computed<AppTheme>(() => themeRef.value)
const next = computed<AppTheme>(() => (current.value === 'light' ? 'dark' : 'light'))
const glyph = computed(() => (current.value === 'light' ? '☾' : '☀'))
const tip = computed(() => (current.value === 'light' ? t('theme.toDark') : t('theme.toLight')))

function onClick() {
  setTheme(next.value)
}
</script>

<template>
  <button
    type="button"
    class="theme-toggle"
    :title="tip"
    :aria-label="tip"
    @click="onClick"
  >
    <span class="theme-toggle__glyph" aria-hidden="true">{{ glyph }}</span>
  </button>
</template>

<style scoped>
.theme-toggle {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  padding: 0;
  border-radius: var(--radius-md);
  border: 1px solid var(--border-strong);
  background: var(--surface-2);
  color: var(--muted);
  font-family: var(--font-mono);
  font-size: var(--fs-13);
  cursor: pointer;
  transition: color var(--dur-fast) var(--ease-out),
    border-color var(--dur-fast) var(--ease-out),
    background var(--dur-fast) var(--ease-out);
}

.theme-toggle:hover {
  color: var(--text);
  border-color: var(--accent);
  background: var(--surface-3);
}

.theme-toggle:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 2px;
}

.theme-toggle__glyph {
  line-height: 1;
  display: inline-block;
  transform: translateY(-1px);
}
</style>