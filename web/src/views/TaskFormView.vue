<script setup lang="ts">
/**
 * Create / edit one monitoring task.
 *
 * A provider offers a set of candidate models; this task selects exactly one
 * of them. Everything else about the upstream (protocol, url, credentials,
 * timeout) lives on the provider, so one edit there reaches every task that
 * uses it.
 */

import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useTasksStore } from '../stores/tasks'
import { useProvidersStore } from '../stores/providers'
import { useToast } from '../utils/toast'
import { cron, required } from '../utils/validation'
import Button from '../components/ui/Button.vue'
import FormField from '../components/ui/FormField.vue'
import Switch from '../components/ui/Switch.vue'
import type { Task, TaskWritePayload } from '../api/tasks'

/** The part of the payload the form owns directly; `model` is kept separate. */
type TaskForm = Omit<TaskWritePayload, 'model'>

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const tasks = useTasksStore()
const providers = useProvidersStore()
const toast = useToast()

const isEdit = computed(() => !!route.params.id)
const taskId = computed(() => (route.params.id as string | undefined) ?? '')

const form = ref<TaskForm>({
  name: '',
  provider_id: '',
  user_prompt: 'Say hi',
  schedule: '*/30 * * * * *',
  canary_group: 'default',
  notify: { enabled: false, channel: 'wecom' },
})
/** The model this task selects out of the chosen provider's candidates. */
const model = ref('')
const errors = ref<Record<string, string>>({})
const submitting = ref(false)

/** The provider the task will probe — rendered as a read-only summary. */
const chosenProvider = computed(
  () => providers.providers.find((p) => p.id === form.value.provider_id) ?? null,
)

/** Candidate models of the selected provider; the model dropdown is bound to it. */
const modelOptions = computed<string[]>(() => chosenProvider.value?.models ?? [])

// Whenever the provider (or its model list) changes, keep the selected model
// valid: drop it back to the first candidate when the old one is no longer
// offered. This also covers providers arriving after the form was mounted.
watch(modelOptions, (options) => {
  if (!options.includes(model.value)) model.value = options[0] ?? ''
})

onMounted(async () => {
  // The provider registry is the source of the upstream coordinates, so
  // the form cannot be rendered without it.
  if (providers.providers.length === 0) {
    await providers.fetchAll()
  }
  if (isEdit.value && taskId.value) {
    try {
      const { api } = await import('../api/client')
      const task = await api.get<Task>(`/admin/tasks/${taskId.value}`)
      hydrate(task)
    } catch (e) {
      toast.error(t('toast.errLoad'), (e as Error).message)
    }
  } else {
    // Preselect the first enabled provider AND its primary model so a fresh
    // form is usable without an extra click.
    const first = providers.providers.find((p) => p.enabled) ?? providers.providers[0]
    if (first) {
      form.value.provider_id = first.id
      model.value = first.models?.[0] ?? ''
    }
  }
})

function hydrate(task: Task) {
  form.value = {
    name: task.name,
    provider_id: task.provider_id,
    user_prompt: task.user_prompt,
    prompt_vars: task.prompt_vars,
    schedule: task.schedule,
    canary_group: task.canary_group,
    notify: {
      enabled: task.notify.enabled,
      channel: task.notify.channel,
      wecom_webhook: '',
    },
  }
  model.value = task.model
}

function validate(): boolean {
  errors.value = {}
  let ok = true
  const r1 = required(form.value.name, t('task.form.name'))
  if (!r1.ok) { errors.value.name = r1.message!; ok = false }
  const r2 = required(form.value.provider_id, t('task.form.provider'))
  if (!r2.ok) { errors.value.provider_id = r2.message!; ok = false }
  const r3 = required(model.value, t('task.form.model'))
  if (!r3.ok) { errors.value.model = r3.message!; ok = false }
  const r4 = required(form.value.user_prompt, t('task.form.userPrompt'))
  if (!r4.ok) { errors.value.user_prompt = r4.message!; ok = false }
  const r5 = cron(form.value.schedule)
  if (!r5.ok) { errors.value.schedule = r5.message!; ok = false }
  return ok
}

async function onSubmit() {
  if (!validate()) return
  submitting.value = true
  try {
    const payload: TaskWritePayload = { ...form.value, model: model.value }
    if (isEdit.value) {
      await tasks.update(taskId.value, payload)
      toast.success(t('toast.taskUpdated'), form.value.name)
    } else {
      await tasks.create(payload)
      toast.success(t('toast.taskCreated'), form.value.name)
    }
    router.push('/tasks')
  } catch (e) {
    toast.error(t(isEdit.value ? 'toast.errUpdate' : 'toast.errCreate'), (e as Error).message)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section class="form">
    <header class="form__head">
      <h1>{{ isEdit ? t('task.form.editTitle') : t('task.form.newTitle') }}</h1>
      <p class="text-muted">{{ t('task.form.note') }}</p>
    </header>
    <form class="form__grid" @submit.prevent="onSubmit">
      <FormField :label="t('task.form.name')" id="name" required :error="errors.name">
        <input id="name" v-model="form.name" type="text" />
      </FormField>

      <FormField
        :label="t('task.form.provider')"
        id="provider_id"
        required
        :error="errors.provider_id"
        :hint="t('task.form.providerHint')"
      >
        <select id="provider_id" v-model="form.provider_id">
          <option value="" disabled>{{ t('task.form.providerPlaceholder') }}</option>
          <option v-for="p in providers.providers" :key="p.id" :value="p.id" :disabled="!p.enabled">
            {{ p.name }} · {{ p.protocol }} · {{ t('task.form.modelCount', { n: p.models?.length ?? 0 }) }}{{ p.enabled ? '' : ' (' + t('common.disabled') + ')' }}
          </option>
        </select>
        <p v-if="providers.providers.length === 0" class="form__no-provider">
          {{ t('task.form.noProviders') }}
        </p>
      </FormField>

      <FormField
        :label="t('task.form.model')"
        id="model"
        required
        :error="errors.model"
        :hint="t('task.form.modelHint')"
      >
        <select id="model" v-model="model">
          <option value="" disabled>{{ t('task.form.modelPlaceholder') }}</option>
          <option v-for="m in modelOptions" :key="m" :value="m">{{ m }}</option>
        </select>
      </FormField>

      <section v-if="chosenProvider" class="form__panel form__span">
        <h3 class="form__panel-title text-mono">{{ t('task.form.upstream') }}</h3>
        <dl class="form__summary">
          <dt>{{ t('task.detail.protocol') }}</dt><dd class="text-mono">{{ chosenProvider.protocol }}</dd>
          <dt>{{ t('task.detail.baseUrl') }}</dt><dd class="text-mono">{{ chosenProvider.base_url }}</dd>
          <dt>{{ t('task.detail.timeout') }}</dt><dd class="text-mono">{{ chosenProvider.timeout }}</dd>
          <dt>{{ t('task.detail.apiKey') }}</dt>
          <dd>
            <span v-if="chosenProvider.secret_error" class="form__secret-error">{{ t('task.list.secretErrorBadge') }}</span>
            <span v-else>{{ chosenProvider.has_api_key ? t('task.detail.configured') : t('task.detail.notSet') }}</span>
          </dd>
        </dl>
        <p class="form__hint text-muted">{{ t('task.form.upstreamHint') }}</p>
      </section>

      <FormField :label="t('task.form.schedule')" id="schedule" required :error="errors.schedule" :hint="t('task.form.scheduleHint')">
        <input id="schedule" v-model="form.schedule" type="text" class="text-mono" />
      </FormField>
      <FormField :label="t('task.form.canary')" id="canary_group">
        <input id="canary_group" v-model="form.canary_group" type="text" class="text-mono" />
      </FormField>

      <FormField :label="t('task.form.userPrompt')" id="user_prompt" required :error="errors.user_prompt" class="form__span" :hint="t('task.form.userPromptHint')">
        <textarea id="user_prompt" v-model="form.user_prompt" rows="3" />
      </FormField>

      <section class="form__panel form__span">
        <h3 class="form__panel-title text-mono">{{ t('task.form.notification') }}</h3>
        <Switch
          id="notify_enabled"
          v-model="form.notify.enabled"
          :label="t('task.form.notifyEnabled')"
        />
        <FormField :label="t('task.form.wecom')" id="wecom" :hint="isEdit ? t('task.form.wecomHint') : ''">
          <input id="wecom" v-model="form.notify.wecom_webhook" type="text" class="text-mono" />
        </FormField>
      </section>

      <div class="form__actions form__span">
        <Button type="submit" :loading="submitting" :disabled="providers.providers.length === 0">
          {{ isEdit ? t('task.form.submitSave') : t('task.form.submitCreate') }}
        </Button>
        <Button type="button" variant="ghost" @click="router.back()">{{ t('task.form.cancel') }}</Button>
      </div>
    </form>
  </section>
</template>

<style scoped>
.form { display: flex; flex-direction: column; gap: var(--space-5); max-width: 960px; }
.form__grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: var(--space-4);
}
.form__span { grid-column: span 2; }
/* 区块面板与 FormField 的输入控件同一视觉语言：
   surface-2 底 + border-strong 边框 + radius-md 圆角 */
.form__panel {
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-md);
  background: var(--surface-2);
  padding: var(--space-3) var(--space-4);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.form__panel-title {
  margin: 0;
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--muted);
}
.form__summary {
  display: grid;
  grid-template-columns: 120px 1fr;
  gap: var(--space-2) var(--space-4);
  margin: 0;
}
.form__summary dt { color: var(--muted); font-size: var(--fs-11); font-family: var(--font-mono); text-transform: uppercase; letter-spacing: 0.06em; }
.form__summary dd { margin: 0; font-size: var(--fs-13); }
.form__hint { margin: 0; font-size: var(--fs-11); }
.form__secret-error { color: var(--danger); font-weight: var(--fw-semibold); }
.form__no-provider { color: var(--danger); font-size: var(--fs-11); margin: var(--space-2) 0 0; }
.form__actions { display: flex; gap: var(--space-3); }
</style>
