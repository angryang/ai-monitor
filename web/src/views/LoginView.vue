<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '../stores/auth'
import { useToast } from '../utils/toast'
import Button from '../components/ui/Button.vue'
import FormField from '../components/ui/FormField.vue'

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const auth = useAuthStore()
const toast = useToast()

const username = ref('')
const password = ref('')
const submitting = ref(false)
const shake = ref(false)

onMounted(() => {
  if (auth.isAuthenticated) {
    const next = (route.query.next as string | undefined) ?? '/'
    router.replace(next)
  }
})

async function onSubmit() {
  if (!username.value || !password.value) {
    shake.value = true
    setTimeout(() => (shake.value = false), 320)
    return
  }
  submitting.value = true
  try {
    await auth.login(username.value, password.value)
    const next = (route.query.next as string | undefined) ?? '/'
    router.replace(next)
    toast.success(t('toast.successLogin'), t('toast.welcomeBack', { name: auth.user?.username ?? '' }))
  } catch (e) {
    shake.value = true
    setTimeout(() => (shake.value = false), 320)
    const msg = (e as Error).message ?? t('common.unknownError')
    toast.error(t('toast.errLogin'), msg)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="login">
    <canvas class="login__bg" aria-hidden="true" />
    <div class="login__panel" :class="{ shake }">
      <header class="login__head">
        <span class="login__mark text-display">{{ t('nav.brand') }}</span>
        <h1 class="login__title text-display">{{ t('auth.title') }}</h1>
        <p class="text-muted login__sub">{{ t('auth.subtitle') }}</p>
      </header>
      <form class="login__form" @submit.prevent="onSubmit">
        <FormField :label="t('auth.username')" id="username" required>
          <input id="username" v-model="username" type="text" autocomplete="username" autofocus />
        </FormField>
        <FormField :label="t('auth.password')" id="password" required>
          <input id="password" v-model="password" type="password" autocomplete="current-password" />
        </FormField>
        <Button type="submit" :loading="submitting" block>{{ t('auth.signIn') }}</Button>
      </form>
      <p class="login__hint text-muted text-mono">
        {{ t('auth.hint') }}
      </p>
    </div>
  </div>
</template>

<style scoped>
.login {
  position: relative;
  height: 100vh;
  display: grid;
  place-items: center;
  background: var(--bg);
  overflow: hidden;
}
.login__bg {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  opacity: 0.6;
}
.login__panel {
  position: relative;
  width: min(420px, calc(100vw - var(--space-6)));
  background: var(--surface);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-lg);
  padding: var(--space-6);
  box-shadow: var(--shadow-2);
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
  z-index: 1;
}
.login__head { display: flex; flex-direction: column; gap: var(--space-1); }
.login__mark { font-size: var(--fs-13); color: var(--accent); letter-spacing: 0.08em; text-transform: uppercase; }
.login__title { margin: 0; font-size: var(--fs-32); }
.login__sub { margin: 0; font-size: var(--fs-13); }
.login__form { display: flex; flex-direction: column; gap: var(--space-3); }
.login__hint {
  text-align: center;
  font-size: var(--fs-11);
  letter-spacing: 0.04em;
  margin: 0;
}
</style>