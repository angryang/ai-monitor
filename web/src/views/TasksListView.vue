<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useTasksStore } from '../stores/tasks'
import { useRole } from '../composables/useRole'
import DataTable from '../components/ui/DataTable.vue'
import Badge from '../components/ui/Badge.vue'
import Button from '../components/ui/Button.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import type { Task } from '../api/tasks'

const { t } = useI18n()
const router = useRouter()
const tasks = useTasksStore()
const { isAdmin } = useRole()

onMounted(() => tasks.fetchAll())

const columns = computed(() => [
  { key: 'name', label: t('task.col.name'), width: '24%' },
  { key: 'protocol', label: t('task.col.protocol'), width: '12%' },
  { key: 'model', label: t('task.col.model'), width: '20%' },
  { key: 'schedule', label: t('task.col.schedule'), width: '16%' },
  { key: 'status', label: t('task.col.status'), width: '14%' },
  { key: 'updated', label: t('task.col.updated'), width: '14%' },
])

// Tasks whose stored secrets could not be decrypted with the current master
// key. They are listed but carry no usable credentials until the api key is
// re-entered, so they are not scheduled either.
const brokenTasks = computed(() => tasks.tasks.filter((task) => !!task.secret_error))

function goDetail(row: Task) { router.push(`/tasks/${row.id}`) }
function goEdit(row: Task) { router.push(`/tasks/${row.id}/edit`) }
function goCreate() { router.push('/tasks/new') }
</script>

<template>
  <section class="tasks">
    <header class="tasks__head">
      <div>
        <h1>{{ t('task.list.title') }}</h1>
        <p class="text-muted">{{ t('task.list.subtitle') }}</p>
      </div>
      <div class="tasks__actions">
        <Button v-if="isAdmin" @click="goCreate">{{ t('task.list.new') }}</Button>
      </div>
    </header>

    <div v-if="tasks.loading" class="tasks__loading text-muted">{{ t('common.loading') }}</div>
    <!--
      The banner is deliberately NOT part of the v-if / v-else-if chain that
      renders the list. It used to be a `v-else-if` sibling of the table,
      which meant that as soon as one task carried a `secret_error` the
      chain resolved at the banner and the table (and the empty state) were
      never rendered — every task, including healthy ones, disappeared
      from the UI.
    -->
    <template v-else>
      <div v-if="brokenTasks.length > 0" class="tasks__alert" role="alert">
        <strong>{{ t('task.list.secretErrorTitle') }}</strong>
        <span>{{ t('task.list.secretErrorDesc', { names: brokenTasks.map((task) => task.name).join('、') }) }}</span>
      </div>
      <EmptyState
        v-if="tasks.tasks.length === 0"
        :title="t('task.list.emptyTitle')"
        :description="t('task.list.emptyDesc')"
        glyph="◧"
      >
        <template #action>
          <Button v-if="isAdmin" @click="goCreate">{{ t('task.list.new') }}</Button>
        </template>
      </EmptyState>
      <DataTable v-else :rows="tasks.tasks" :columns="columns">
        <template #cell-name="{ row }">
          <button class="tasks__name" type="button" @click="goDetail(row)">{{ row.name }}</button>
        </template>
        <template #cell-protocol="{ row }">
          <Badge tone="accent">{{ row.protocol }}</Badge>
        </template>
        <template #cell-model="{ row }">
          <span class="text-mono">{{ row.model }}</span>
        </template>
        <template #cell-schedule="{ row }">
          <span class="text-mono text-muted">{{ row.schedule }}</span>
        </template>
        <template #cell-status="{ row }">
          <Badge :tone="row.enabled ? 'ok' : 'muted'">{{ row.enabled ? t('common.enabled') : t('common.paused') }}</Badge>
        </template>
        <template #cell-updated="{ row }">
          <span class="text-mono text-muted">{{ new Date(row.updated_at).toLocaleString() }}</span>
        </template>
        <template #drawer="{ row }">
          <div class="tasks__detail">
            <dl>
              <dt>{{ t('task.detail.id') }}</dt><dd class="text-mono">{{ row.id }}</dd>
              <dt>{{ t('task.detail.baseUrl') }}</dt><dd class="text-mono">{{ row.base_url }}</dd>
              <dt>{{ t('task.detail.userPrompt') }}</dt><dd class="text-mono">{{ row.user_prompt }}</dd>
              <dt>{{ t('task.detail.canaryGroup') }}</dt><dd class="text-mono">{{ row.canary_group }}</dd>
              <dt>{{ t('task.detail.apiKey') }}</dt>
              <dd>
                <span v-if="row.secret_error" class="tasks__secret-error">{{ t('task.list.secretErrorBadge') }}</span>
                <span v-else>{{ row.has_api_key ? t('task.detail.configured') : t('task.detail.notSet') }}</span>
              </dd>
              <dt>{{ t('task.detail.webhook') }}</dt><dd>{{ row.notify.has_webhook ? t('task.detail.configured') : t('task.detail.notSet') }}</dd>
            </dl>
            <div class="tasks__detail-actions">
              <Button size="sm" variant="ghost" @click="goDetail(row)">{{ t('task.detail.open') }}</Button>
              <Button v-if="isAdmin" size="sm" variant="ghost" @click="goEdit(row)">{{ t('task.detail.edit') }}</Button>
            </div>
          </div>
        </template>
      </DataTable>
    </template>
  </section>
</template>

<style scoped>
.tasks { display: flex; flex-direction: column; gap: var(--space-5); }
.tasks__head {
  display: flex;
  justify-content: space-between;
  align-items: flex-end;
  gap: var(--space-4);
}
.tasks__actions { display: flex; gap: var(--space-3); }
.tasks__loading { padding: var(--space-5) 0; }
.tasks__alert {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-4);
  border: 1px solid var(--danger);
  border-radius: var(--radius-lg);
  background: rgba(216, 68, 63, 0.12);
  font-size: var(--fs-13);
  line-height: var(--lh-base);
}
.tasks__alert strong { color: var(--danger); font-weight: var(--fw-semibold); }
.tasks__secret-error { color: var(--danger); font-weight: var(--fw-semibold); }
.tasks__name {
  background: transparent;
  border: 0;
  padding: 0;
  color: var(--text);
  cursor: pointer;
  font-weight: var(--fw-semibold);
  font-family: var(--font-display);
  text-align: left;
}
.tasks__name:hover { color: var(--accent); }
.tasks__detail dl { display: grid; grid-template-columns: 120px 1fr; gap: var(--space-2) var(--space-4); margin: 0 0 var(--space-4); }
.tasks__detail dt { color: var(--muted); font-size: var(--fs-11); text-transform: uppercase; letter-spacing: 0.06em; font-family: var(--font-mono); }
.tasks__detail dd { margin: 0; font-size: var(--fs-13); }
.tasks__detail-actions { display: flex; gap: var(--space-2); }
</style>