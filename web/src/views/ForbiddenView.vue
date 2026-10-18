<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import Button from '../components/ui/Button.vue'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const fromPath = computed(() => {
  const raw = route.query.from
  if (typeof raw === 'string' && raw.length > 0) return raw
  return t('forbidden.fallbackPath')
})
</script>

<template>
  <section class="forbidden">
    <h1>{{ t('forbidden.title') }}</h1>
    <p class="text-muted">{{ t('forbidden.desc', { path: fromPath }) }}</p>
    <Button @click="router.replace('/')">{{ t('forbidden.action') }}</Button>
  </section>
</template>

<style scoped>
.forbidden {
  height: 100vh;
  display: grid;
  place-items: center;
  text-align: center;
  gap: var(--space-4);
  padding: var(--space-6);
}
.forbidden h1 { margin: 0; }
</style>