<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '../stores/auth'
import { useToast } from '../utils/toast'
import Button from '../components/ui/Button.vue'
import FormField from '../components/ui/FormField.vue'

const { t } = useI18n()
const auth = useAuthStore()
const toast = useToast()
const oldPassword = ref('')
const newPassword = ref('')
const submitting = ref(false)
const errors = ref<Record<string, string>>({})

async function onSubmit() {
  errors.value = {}
  if (!oldPassword.value) errors.value.old = t('validation.required')
  if (!newPassword.value || newPassword.value.length < 12) errors.value.new = t('validation.minLengthShort', { n: 12 })
  if (Object.keys(errors.value).length > 0) return
  submitting.value = true
  try {
    const { authApi } = await import('../api/auth')
    await authApi.changePassword(oldPassword.value, newPassword.value)
    toast.success(t('toast.pwdChanged'), t('toast.pwdChangedHint'))
    oldPassword.value = ''
    newPassword.value = ''
  } catch (e) {
    toast.error(t('toast.errChange'), (e as Error).message)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section class="profile">
    <header class="profile__head">
      <h1>{{ t('profile.title') }}</h1>
      <p class="text-muted">{{ t('profile.subtitle') }}</p>
    </header>
    <article class="profile__card">
      <h4>{{ t('profile.identity') }}</h4>
      <dl>
        <dt>{{ t('profile.username') }}</dt><dd class="text-mono">{{ auth.user?.username }}</dd>
        <dt>{{ t('profile.displayName') }}</dt><dd>{{ auth.user?.display_name || t('task.detail.notSet') }}</dd>
        <dt>{{ t('profile.role') }}</dt><dd class="text-mono">{{ auth.user?.role }}</dd>
        <dt>{{ t('profile.sessionExpires') }}</dt>
        <dd class="text-mono">{{ auth.expiresAt ? new Date(auth.expiresAt).toLocaleString() : t('task.detail.notSet') }}</dd>
      </dl>
    </article>
    <article class="profile__card">
      <h4>{{ t('profile.changePwd') }}</h4>
      <form class="profile__form" @submit.prevent="onSubmit">
        <FormField :label="t('profile.currentPwd')" id="old" required :error="errors.old">
          <input id="old" v-model="oldPassword" type="password" autocomplete="current-password" />
        </FormField>
        <FormField :label="t('profile.newPwd')" id="new" required :error="errors.new" :hint="t('profile.newPwdHint')">
          <input id="new" v-model="newPassword" type="password" autocomplete="new-password" />
        </FormField>
        <Button type="submit" :loading="submitting">{{ t('profile.update') }}</Button>
      </form>
    </article>
  </section>
</template>

<style scoped>
.profile { display: flex; flex-direction: column; gap: var(--space-5); max-width: 720px; }
.profile__card {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: var(--space-5);
}
.profile__card h4 { margin: 0 0 var(--space-3); font-size: var(--fs-13); color: var(--muted); text-transform: uppercase; letter-spacing: 0.06em; font-family: var(--font-mono); }
.profile__card dl { display: grid; grid-template-columns: 140px 1fr; gap: var(--space-2) var(--space-4); margin: 0; }
.profile__card dt { color: var(--muted); font-size: var(--fs-11); font-family: var(--font-mono); text-transform: uppercase; letter-spacing: 0.06em; }
.profile__card dd { margin: 0; font-size: var(--fs-13); }
.profile__form { display: flex; flex-direction: column; gap: var(--space-3); max-width: 360px; }
</style>