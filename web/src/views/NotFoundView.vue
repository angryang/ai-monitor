<script setup lang="ts">
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import Button from '../components/ui/Button.vue'

const { t } = useI18n()
const router = useRouter()
</script>

<template>
  <section class="notfound">
    <h1>{{ t('notFound.title') }}</h1>
    <p class="text-muted">{{ t('notFound.desc') }}</p>
    <Button @click="router.replace('/')">{{ t('notFound.action') }}</Button>
  </section>
</template>

<style scoped>
.notfound {
  height: 100vh;
  display: grid;
  place-items: center;
  text-align: center;
  gap: var(--space-3);
  padding: var(--space-6);
}
.notfound h1 { margin: 0; font-size: var(--fs-72); color: var(--accent); }
</style>