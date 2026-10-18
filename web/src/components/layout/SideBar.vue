<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useRole } from '../../composables/useRole'

const { t } = useI18n()
const { isAdmin } = useRole()

interface NavItem {
  to: string
  labelKey: string
  glyph: string
  adminOnly?: boolean
}

const items: NavItem[] = [
  { to: '/tasks', labelKey: 'nav.tasks', glyph: '◧' },
  { to: '/metrics', labelKey: 'nav.metrics', glyph: '∿' },
  { to: '/users', labelKey: 'nav.users', glyph: '◌', adminOnly: true },
  { to: '/profile', labelKey: 'nav.profile', glyph: '◇' },
]

const visible = computed(() => items.filter((i) => !i.adminOnly || isAdmin.value))
</script>

<template>
  <aside class="side">
    <div class="side__brand">
      <span class="side__brand-mark">AI</span>
      <span class="side__brand-text">{{ t('nav.brand') }}</span>
    </div>
    <nav class="side__nav stagger">
      <RouterLink
        v-for="item in visible"
        :key="item.to"
        :to="item.to"
        class="side__item"
        active-class="side__item--active"
      >
        <span class="side__glyph">{{ item.glyph }}</span>
        <span class="side__label">{{ t(item.labelKey) }}</span>
      </RouterLink>
    </nav>
    <footer class="side__footer">
      <span class="text-muted">v0.1 · §18 {{ t('nav.controlPlane') }}</span>
    </footer>
  </aside>
</template>

<style scoped>
.side {
  background: var(--surface);
  border-right: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  padding: var(--space-5) 0;
}

.side__brand {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: 0 var(--space-5) var(--space-5);
  border-bottom: 1px solid var(--border);
}

.side__brand-mark {
  width: 32px;
  height: 32px;
  display: grid;
  place-items: center;
  background: var(--accent-soft);
  color: var(--accent);
  border-radius: var(--radius-sm);
  font-family: var(--font-display);
  font-weight: var(--fw-bold);
  font-size: var(--fs-13);
}

.side__brand-text {
  font-family: var(--font-display);
  font-weight: var(--fw-semibold);
  font-size: var(--fs-14);
  letter-spacing: 0.04em;
}

.side__nav {
  display: flex;
  flex-direction: column;
  padding: var(--space-4) var(--space-3);
  flex: 1;
  gap: 2px;
}

.side__item {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-3) var(--space-3);
  color: var(--muted);
  border-radius: var(--radius-md);
  border: 1px solid transparent;
  text-decoration: none;
  transition: background var(--dur-fast) var(--ease-out), color var(--dur-fast) var(--ease-out),
    border-color var(--dur-fast) var(--ease-out);
}

.side__item:hover {
  color: var(--text);
  background: var(--surface-2);
}

.side__item--active {
  color: var(--text);
  background: var(--accent-soft);
  border-color: var(--border-strong);
}

.side__glyph {
  width: 20px;
  font-family: var(--font-display);
  text-align: center;
  font-size: var(--fs-16);
}

.side__label {
  font-size: var(--fs-13);
  font-weight: var(--fw-medium);
  letter-spacing: 0.02em;
}

.side__footer {
  padding: var(--space-4) var(--space-5);
  border-top: 1px solid var(--border);
  font-size: var(--fs-11);
  color: var(--muted);
  font-family: var(--font-mono);
}
</style>