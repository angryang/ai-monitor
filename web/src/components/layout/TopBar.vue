<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '../../stores/auth'
import { useRole } from '../../composables/useRole'
import { setLocale, type AppLocale, LOCALE_OPTIONS, SUPPORTED_LOCALES } from '../../i18n'
import ThemeSwitcher from '../ui/ThemeSwitcher.vue'

const { t, locale } = useI18n()
const auth = useAuthStore()
const { user, isAdmin } = useRole()
const route = useRoute()
const router = useRouter()

const breadcrumb = computed(() => {
  const path = route.path
  if (path.startsWith('/tasks/new')) return t('crumb.tasksNew')
  if (path.match(/^\/tasks\/[^/]+\/edit/)) return t('crumb.tasksEdit')
  if (path.match(/^\/tasks\/[^/]+/)) return t('crumb.tasksDetail')
  if (path === '/tasks' || path === '/') return t('crumb.tasks')
  if (path === '/metrics') return t('crumb.metrics')
  if (path === '/users') return t('crumb.users')
  if (path === '/profile') return t('crumb.profile')
  return t('crumb.fallback')
})

async function logout() {
  await auth.logout()
  router.push('/login')
}

function onLangChange(e: Event) {
  const next = (e.target as HTMLSelectElement).value as AppLocale
  if ((SUPPORTED_LOCALES as string[]).includes(next)) {
    setLocale(next)
  }
}
</script>

<template>
  <header class="top">
    <div class="top__crumb text-mono">
      <span class="text-muted">{{ breadcrumb }}</span>
    </div>
    <div class="top__user">
      <ThemeSwitcher />
      <label class="top__lang" :title="t('lang.switchLabel')">
        <span class="top__lang-mark" aria-hidden="true">◐</span>
        <select
          class="top__lang-select"
          :value="locale"
          :aria-label="t('lang.switchLabel')"
          @change="onLangChange"
        >
          <option v-for="opt in LOCALE_OPTIONS" :key="opt.code" :value="opt.code">
            {{ opt.label }}
          </option>
        </select>
      </label>
      <span class="top__role" :data-role="user?.role">{{ isAdmin ? t('auth.roleAdmin') : t('auth.roleViewer') }}</span>
      <span class="top__name text-display">{{ user?.username }}</span>
      <button class="top__logout" type="button" @click="logout">{{ t('auth.logout') }}</button>
    </div>
  </header>
</template>

<style scoped>
.top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 var(--space-6);
  border-bottom: 1px solid var(--border);
  background: var(--surface);
}

.top__crumb {
  font-size: var(--fs-12);
  letter-spacing: 0.04em;
  text-transform: uppercase;
}

.top__user {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

.top__lang {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  padding: var(--space-1) var(--space-2);
  border-radius: var(--radius-md);
  border: 1px solid var(--border-strong);
  background: var(--surface-2);
  color: var(--muted);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  cursor: pointer;
  transition: color var(--dur-fast) var(--ease-out), border-color var(--dur-fast) var(--ease-out);
}
.top__lang:hover { color: var(--text); border-color: var(--accent); }

.top__lang-mark { font-family: var(--font-display); color: var(--accent); }

.top__lang-select {
  background: transparent;
  border: 0;
  color: inherit;
  font: inherit;
  letter-spacing: inherit;
  text-transform: inherit;
  padding: 0;
  cursor: pointer;
  outline: none;
}
.top__lang-select:focus-visible { outline: none; }
.top__lang-select option {
  background: var(--surface);
  color: var(--text);
  text-transform: none;
  letter-spacing: 0;
}

.top__role {
  padding: 2px var(--space-2);
  border-radius: var(--radius-pill);
  background: var(--accent-soft);
  color: var(--accent);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}

.top__role[data-role='viewer'] {
  background: rgba(91, 141, 239, 0.18);
  color: var(--info);
}

.top__name {
  font-size: var(--fs-13);
}

.top__logout {
  background: transparent;
  border: 1px solid var(--border-strong);
  color: var(--muted);
  padding: var(--space-1) var(--space-3);
  border-radius: var(--radius-md);
  cursor: pointer;
  transition: color var(--dur-fast) var(--ease-out), border-color var(--dur-fast) var(--ease-out);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
}
.top__logout:hover {
  color: var(--text);
  border-color: var(--accent);
}
</style>