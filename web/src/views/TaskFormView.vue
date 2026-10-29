<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useTasksStore } from '../stores/tasks'
import { useToast } from '../utils/toast'
import { cron, minLength, required, url } from '../utils/validation'
import Button from '../components/ui/Button.vue'
import FormField from '../components/ui/FormField.vue'
import Switch from '../components/ui/Switch.vue'
import type { Task, TaskWritePayload } from '../api/tasks'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const tasks = useTasksStore()
const toast = useToast()

const isEdit = computed(() => !!route.params.id)
const taskId = computed(() => (route.params.id as string | undefined) ?? '')

const form = ref<TaskWritePayload>({
  name: '',
  protocol: 'openai',
  base_url: '',
  api_key: '',
  model: '',
  user_prompt: 'Say hi',
  schedule: '*/30 * * * * *',
  timeout: '30s',
  canary_group: 'default',
  insecure_skip_verify: false,
  thinking_enabled: false,
  notify: { enabled: false, channel: 'wecom' },
})
const errors = ref<Record<string, string>>({})
const submitting = ref(false)

onMounted(async () => {
  if (isEdit.value && taskId.value) {
    try {
      const t = await tasksApiGet(taskId.value)
      hydrate(t)
    } catch (e) {
      toast.error(t('toast.errLoad'), (e as Error).message)
    }
  }
})

async function tasksApiGet(id: string): Promise<Task> {
  const { api } = await import('../api/client')
  return api.get<Task>(`/admin/tasks/${id}`)
}

function hydrate(t: Task) {
  form.value = {
    name: t.name,
    protocol: t.protocol,
    base_url: t.base_url,
    api_key: '', // intentionally empty — user must re-enter to change
    model: t.model,
    system_prompt: t.system_prompt,
    user_prompt: t.user_prompt,
    prompt_vars: t.prompt_vars,
    schedule: t.schedule,
    timeout: t.timeout,
    canary_group: t.canary_group,
    insecure_skip_verify: t.insecure_skip_verify ?? false,
    thinking_enabled: t.thinking_enabled ?? false,
    notify: {
      enabled: t.notify.enabled,
      channel: t.notify.channel,
      wecom_webhook: '',
    },
    openai: t.openai ?? undefined,
    anthropic: t.anthropic ?? undefined,
    gemini: t.gemini ?? undefined,
  }
}

function validate(): boolean {
  errors.value = {}
  let ok = true
  const r1 = required(form.value.name, t('task.form.name'))
  if (!r1.ok) { errors.value.name = r1.message!; ok = false }
  const r2 = url(form.value.base_url)
  if (!r2.ok) { errors.value.base_url = r2.message!; ok = false }
  const r3 = required(form.value.model, t('task.form.model'))
  if (!r3.ok) { errors.value.model = r3.message!; ok = false }
  const r4 = required(form.value.user_prompt, t('task.form.userPrompt'))
  if (!r4.ok) { errors.value.user_prompt = r4.message!; ok = false }
  const r5 = cron(form.value.schedule)
  if (!r5.ok) { errors.value.schedule = r5.message!; ok = false }
  if (!isEdit.value) {
    const r6 = minLength(form.value.api_key ?? '', 8)
    if (!r6.ok) { errors.value.api_key = r6.message!; ok = false }
  }
  return ok
}

async function onSubmit() {
  if (!validate()) return
  submitting.value = true
  try {
    if (isEdit.value) {
      await tasks.update(taskId.value, form.value)
      toast.success(t('toast.taskUpdated'), form.value.name)
    } else {
      await tasks.create(form.value)
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
      <FormField :label="t('task.form.protocol')" id="protocol" required>
        <select id="protocol" v-model="form.protocol">
          <option value="openai">openai</option>
          <option value="anthropic">anthropic</option>
          <option value="gemini">gemini</option>
        </select>
      </FormField>
      <FormField :label="t('task.form.baseUrl')" id="base_url" required :error="errors.base_url">
        <input id="base_url" v-model="form.base_url" type="url" placeholder="https://api.openai.com" />
      </FormField>
      <FormField
        :label="isEdit ? t('task.form.apiKeyKeep') : t('task.form.apiKey')"
        id="api_key"
        :required="!isEdit"
        :error="errors.api_key"
      >
        <input id="api_key" v-model="form.api_key" type="password" autocomplete="off" />
      </FormField>
      <FormField :label="t('task.form.model')" id="model" required :error="errors.model">
        <input id="model" v-model="form.model" type="text" placeholder="gpt-4o" />
      </FormField>
      <FormField :label="t('task.form.schedule')" id="schedule" required :error="errors.schedule" :hint="t('task.form.scheduleHint')">
        <input id="schedule" v-model="form.schedule" type="text" class="text-mono" />
      </FormField>
      <FormField :label="t('task.form.timeout')" id="timeout" :hint="t('task.form.timeoutHint')">
        <input id="timeout" v-model="form.timeout" type="text" class="text-mono" />
      </FormField>
      <FormField :label="t('task.form.canary')" id="canary_group">
        <input id="canary_group" v-model="form.canary_group" type="text" class="text-mono" />
      </FormField>
      <section class="form__panel form__span">
        <h3 class="form__panel-title text-mono">{{ t('task.form.tls') }}</h3>
        <Switch
          id="insecure_skip_verify"
          v-model="form.insecure_skip_verify"
          :label="t('task.form.insecureSkipVerify')"
          :description="t('task.form.insecureSkipVerifyHint')"
        />
      </section>

      <section class="form__panel form__span">
        <h3 class="form__panel-title text-mono">{{ t('task.form.thinking') }}</h3>
        <Switch
          id="thinking_enabled"
          v-model="form.thinking_enabled"
          :label="t('task.form.thinkingLabel')"
          :description="t('task.form.thinkingHint')"
        />
      </section>

      <FormField :label="t('task.form.userPrompt')" id="user_prompt" required :error="errors.user_prompt" class="form__span">
        <textarea id="user_prompt" v-model="form.user_prompt" rows="3" />
      </FormField>
      <FormField :label="t('task.form.systemPrompt')" id="system_prompt" class="form__span">
        <textarea id="system_prompt" v-model="form.system_prompt" rows="2" />
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
        <Button type="submit" :loading="submitting">{{ isEdit ? t('task.form.submitSave') : t('task.form.submitCreate') }}</Button>
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
.form__actions { display: flex; gap: var(--space-3); }
</style>