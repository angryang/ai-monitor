<script setup lang="ts">
import { onMounted, ref, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useRole } from '../composables/useRole'
import { useToast } from '../utils/toast'
import Button from '../components/ui/Button.vue'
import Badge from '../components/ui/Badge.vue'
import Modal from '../components/ui/Modal.vue'
import type { Task } from '../api/tasks'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const { isAdmin } = useRole()
const toast = useToast()

const task = ref<Task | null>(null)
const loading = ref(true)
const confirmOpen = ref(false)
const startConfirmOpen = ref(false)
const stopConfirmOpen = ref(false)
const busy = ref(false)

onMounted(async () => {
  try {
    const id = route.params.id as string
    const { api } = await import('../api/client')
    task.value = await api.get<Task>(`/admin/tasks/${id}`)
  } catch (e) {
    toast.error(t('toast.errLoad'), (e as Error).message)
  } finally {
    loading.value = false
  }
})

const isRunning = computed(() => task.value?.enabled === true)

async function onDelete() {
  confirmOpen.value = false
  if (!task.value) return
  try {
    const { tasksApi } = await import('../api/tasks')
    await tasksApi.remove(task.value.id)
    toast.success(t('toast.taskDeleted'), task.value.name)
    router.replace('/tasks')
  } catch (e) {
    toast.error(t('toast.errDelete'), (e as Error).message)
  }
}

async function onStart() {
  startConfirmOpen.value = false
  if (!task.value || busy.value) return
  busy.value = true
  try {
    const { tasksApi } = await import('../api/tasks')
    const updated = await tasksApi.start(task.value.id)
    task.value = updated
    toast.success(t('toast.taskStarted'), task.value.name)
  } catch (e) {
    toast.error(t('toast.errStart'), (e as Error).message)
  } finally {
    busy.value = false
  }
}

async function onStop() {
  stopConfirmOpen.value = false
  if (!task.value || busy.value) return
  busy.value = true
  try {
    const { tasksApi } = await import('../api/tasks')
    const updated = await tasksApi.stop(task.value.id)
    task.value = updated
    toast.success(t('toast.taskStopped'), task.value.name)
  } catch (e) {
    toast.error(t('toast.errStop'), (e as Error).message)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <section class="detail">
    <header class="detail__head">
      <div>
        <h1>{{ task?.name ?? t('task.detail.notSet') }}</h1>
        <p v-if="task" class="text-muted text-mono">{{ task.id }}</p>
      </div>
      <div v-if="isAdmin" class="detail__actions">
        <Button v-if="task && !isRunning" variant="primary" :disabled="busy" @click="startConfirmOpen = true">{{ t('task.detail.start') }}</Button>
        <Button v-if="task && isRunning" variant="ghost" :disabled="busy" @click="stopConfirmOpen = true">{{ t('task.detail.stop') }}</Button>
        <Button variant="ghost" @click="router.push(`/tasks/${task?.id}/runs`)">{{ t('task.detail.runs') }}</Button>
        <Button variant="ghost" @click="router.push(`/tasks/${task?.id}/edit`)">{{ t('task.detail.edit') }}</Button>
        <Button variant="danger" @click="confirmOpen = true">{{ t('task.detail.delete') }}</Button>
      </div>
      <div v-else class="detail__actions">
        <Button variant="ghost" @click="router.push(`/tasks/${task?.id}/runs`)">{{ t('task.detail.runs') }}</Button>
      </div>
    </header>

    <div v-if="loading" class="text-muted">{{ t('common.loading') }}</div>
    <article v-else-if="task" class="detail__grid">
      <div class="detail__card">
        <h4>{{ t('task.detail.identity') }}</h4>
        <dl>
          <dt>{{ t('task.detail.protocol') }}</dt><dd><Badge tone="accent">{{ task.protocol }}</Badge></dd>
          <dt>{{ t('task.detail.model') }}</dt><dd class="text-mono">{{ task.model }}</dd>
          <dt>{{ t('task.detail.baseUrl') }}</dt><dd class="text-mono">{{ task.base_url }}</dd>
          <dt>{{ t('task.detail.canary') }}</dt><dd class="text-mono">{{ task.canary_group }}</dd>
          <dt>{{ t('task.detail.enabled') }}</dt>
          <dd>
            <Badge :tone="task.enabled ? 'ok' : 'muted'">
              {{ task.enabled ? t('task.detail.statusRunning') : t('task.detail.statusStopped') }}
            </Badge>
          </dd>
        </dl>
      </div>
      <div class="detail__card">
        <h4>{{ t('task.detail.schedule') }}</h4>
        <dl>
          <dt>{{ t('task.detail.cron') }}</dt><dd class="text-mono">{{ task.schedule }}</dd>
          <dt>{{ t('task.detail.timeout') }}</dt><dd class="text-mono">{{ task.timeout }}</dd>
          <dt>{{ t('task.detail.apiKey') }}</dt><dd>{{ task.has_api_key ? t('task.detail.configured') : t('task.detail.notSet') }}</dd>
          <dt>{{ t('task.detail.webhook') }}</dt><dd>{{ task.notify.has_webhook ? t('task.detail.configured') : t('task.detail.notSet') }}</dd>
        </dl>
      </div>
      <div class="detail__card detail__card--wide">
        <h4>{{ t('task.detail.prompts') }}</h4>
        <p class="detail__prompt"><span class="text-muted">{{ t('task.detail.systemLabel') }}</span> {{ task.system_prompt || t('task.detail.notSet') }}</p>
        <p class="detail__prompt"><span class="text-muted">{{ t('task.detail.userLabel') }}</span> {{ task.user_prompt }}</p>
        <p class="text-mono text-muted">{{ t('task.detail.vars', { json: JSON.stringify(task.prompt_vars) }) }}</p>
      </div>
    </article>

    <Modal :open="confirmOpen" :title="t('task.detail.deleteConfirmTitle')" size="sm" @close="confirmOpen = false">
      <p>{{ t('task.detail.deleteConfirmBody', { name: task?.name ?? '' }) }}</p>
      <template #footer>
        <Button variant="ghost" @click="confirmOpen = false">{{ t('common.cancel') }}</Button>
        <Button variant="danger" @click="onDelete">{{ t('common.delete') }}</Button>
      </template>
    </Modal>

    <Modal :open="startConfirmOpen" :title="t('task.detail.startConfirmTitle')" size="sm" @close="startConfirmOpen = false">
      <p>{{ t('task.detail.startConfirmBody', { name: task?.name ?? '' }) }}</p>
      <template #footer>
        <Button variant="ghost" :disabled="busy" @click="startConfirmOpen = false">{{ t('common.cancel') }}</Button>
        <Button variant="primary" :disabled="busy" @click="onStart">{{ t('task.detail.start') }}</Button>
      </template>
    </Modal>

    <Modal :open="stopConfirmOpen" :title="t('task.detail.stopConfirmTitle')" size="sm" @close="stopConfirmOpen = false">
      <p>{{ t('task.detail.stopConfirmBody', { name: task?.name ?? '' }) }}</p>
      <template #footer>
        <Button variant="ghost" :disabled="busy" @click="stopConfirmOpen = false">{{ t('common.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="onStop">{{ t('task.detail.stop') }}</Button>
      </template>
    </Modal>
  </section>
</template>

<style scoped>
.detail { display: flex; flex-direction: column; gap: var(--space-5); }
.detail__head {
  display: flex;
  justify-content: space-between;
  align-items: flex-end;
  gap: var(--space-4);
}
.detail__actions { display: flex; gap: var(--space-3); }
.detail__grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: var(--space-4);
}
.detail__card {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: var(--space-5);
}
.detail__card--wide { grid-column: span 2; }
.detail__card h4 { margin: 0 0 var(--space-3); font-size: var(--fs-13); color: var(--muted); text-transform: uppercase; letter-spacing: 0.06em; font-family: var(--font-mono); }
.detail__card dl { display: grid; grid-template-columns: 100px 1fr; gap: var(--space-2) var(--space-4); margin: 0; }
.detail__card dt { color: var(--muted); font-size: var(--fs-11); font-family: var(--font-mono); text-transform: uppercase; letter-spacing: 0.06em; }
.detail__card dd { margin: 0; font-size: var(--fs-13); }
.detail__prompt { font-size: var(--fs-13); margin: 0 0 var(--space-2); }
</style>
