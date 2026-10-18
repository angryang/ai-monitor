<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useToast } from '../utils/toast'
import { usersApi } from '../api/users'
import type { UserView } from '../api/auth'
import DataTable from '../components/ui/DataTable.vue'
import Badge from '../components/ui/Badge.vue'
import Button from '../components/ui/Button.vue'
import FormField from '../components/ui/FormField.vue'
import Modal from '../components/ui/Modal.vue'

const { t } = useI18n()
const toast = useToast()
const users = ref<UserView[]>([])
const loading = ref(true)
const modalOpen = ref(false)
const form = ref({ username: '', display_name: '', password: '', role: 'viewer' as 'admin' | 'viewer' })
const errors = ref<Record<string, string>>({})
const submitting = ref(false)
const deleteTarget = ref<UserView | null>(null)

const columns = computed(() => [
  { key: 'username', label: t('user.col.username'), width: '24%' },
  { key: 'display_name', label: t('user.col.display'), width: '24%' },
  { key: 'role', label: t('user.col.role'), width: '12%' },
  { key: 'status', label: t('user.col.status'), width: '14%' },
  { key: 'created', label: t('user.col.created'), width: '14%' },
  { key: 'actions', label: '', width: '12%' },
])

onMounted(load)

async function load() {
  loading.value = true
  try {
    const { users: list } = await usersApi.list()
    users.value = list
  } catch (e) {
    toast.error(t('toast.errLoad'), (e as Error).message)
  } finally {
    loading.value = false
  }
}

function openCreate() {
  form.value = { username: '', display_name: '', password: '', role: 'viewer' }
  errors.value = {}
  modalOpen.value = true
}

function validate(): boolean {
  errors.value = {}
  let ok = true
  if (!form.value.username) { errors.value.username = t('validation.required'); ok = false }
  if (!form.value.password || form.value.password.length < 12) {
    errors.value.password = t('validation.minLengthShort', { n: 12 })
    ok = false
  }
  if (!['admin', 'viewer'].includes(form.value.role)) {
    errors.value.role = t('validation.invalidRole')
    ok = false
  }
  return ok
}

async function onCreate() {
  if (!validate()) return
  submitting.value = true
  try {
    await usersApi.create({
      username: form.value.username,
      display_name: form.value.display_name,
      password: form.value.password,
      role: form.value.role,
      enabled: true,
    })
    toast.success(t('toast.userCreated'), form.value.username)
    modalOpen.value = false
    await load()
  } catch (e) {
    toast.error(t('toast.errCreate'), (e as Error).message)
  } finally {
    submitting.value = false
  }
}

async function onDelete() {
  if (!deleteTarget.value) return
  try {
    await usersApi.remove(deleteTarget.value.id)
    toast.success(t('toast.userDeleted'), deleteTarget.value.username)
    deleteTarget.value = null
    await load()
  } catch (e) {
    toast.error(t('toast.errDelete'), (e as Error).message)
  }
}
</script>

<template>
  <section class="users">
    <header class="users__head">
      <div>
        <h1>{{ t('user.list.title') }}</h1>
        <p class="text-muted">{{ t('user.list.subtitle') }}</p>
      </div>
      <Button @click="openCreate">{{ t('user.list.new') }}</Button>
    </header>

    <div v-if="loading" class="text-muted">{{ t('common.loading') }}</div>
    <DataTable v-else :rows="users" :columns="columns">
      <template #cell-username="{ row }">
        <span class="text-mono">{{ row.username }}</span>
      </template>
      <template #cell-role="{ row }">
        <Badge :tone="row.role === 'admin' ? 'accent' : 'info'">{{ row.role }}</Badge>
      </template>
      <template #cell-status="{ row }">
        <Badge :tone="row.enabled ? 'ok' : 'muted'">{{ row.enabled ? t('common.enabled') : t('common.disabled') }}</Badge>
      </template>
      <template #cell-created="{ row }">
        <span class="text-mono text-muted">{{ new Date(row.created_at).toLocaleString() }}</span>
      </template>
      <template #cell-actions="{ row }">
        <Button size="sm" variant="danger" @click="deleteTarget = row">{{ t('common.delete') }}</Button>
      </template>
      <template #drawer="{ row }">
        <dl class="users__drawer">
          <dt>{{ t('user.drawer.id') }}</dt><dd class="text-mono">{{ row.id }}</dd>
          <dt>{{ t('user.drawer.username') }}</dt><dd class="text-mono">{{ row.username }}</dd>
          <dt>{{ t('user.drawer.display') }}</dt><dd>{{ row.display_name || t('task.detail.notSet') }}</dd>
          <dt>{{ t('user.drawer.role') }}</dt><dd>{{ row.role }}</dd>
          <dt>{{ t('user.drawer.enabled') }}</dt><dd>{{ row.enabled }}</dd>
          <dt>{{ t('user.drawer.createdAt') }}</dt><dd class="text-mono">{{ row.created_at }}</dd>
          <dt>{{ t('user.drawer.updatedAt') }}</dt><dd class="text-mono">{{ row.updated_at }}</dd>
        </dl>
      </template>
    </DataTable>

    <Modal :open="modalOpen" :title="t('user.create.title')" @close="modalOpen = false">
      <form @submit.prevent="onCreate" class="users__form">
        <FormField :label="t('user.create.username')" id="cu-user" required :error="errors.username">
          <input id="cu-user" v-model="form.username" type="text" class="text-mono" />
        </FormField>
        <FormField :label="t('user.create.display')" id="cu-display">
          <input id="cu-display" v-model="form.display_name" type="text" />
        </FormField>
        <FormField :label="t('user.create.password')" id="cu-pw" required :error="errors.password" :hint="t('validation.minLengthShort', { n: 12 })">
          <input id="cu-pw" v-model="form.password" type="password" class="text-mono" />
        </FormField>
        <FormField :label="t('user.create.role')" id="cu-role" :error="errors.role">
          <select id="cu-role" v-model="form.role">
            <option value="viewer">{{ t('auth.roleViewer') }}</option>
            <option value="admin">{{ t('auth.roleAdmin') }}</option>
          </select>
        </FormField>
      </form>
      <template #footer>
        <Button variant="ghost" @click="modalOpen = false">{{ t('common.cancel') }}</Button>
        <Button :loading="submitting" @click="onCreate">{{ t('user.create.submit') }}</Button>
      </template>
    </Modal>

    <Modal :open="!!deleteTarget" :title="t('user.delete.title')" size="sm" @close="deleteTarget = null">
      <p v-if="deleteTarget">{{ t('user.delete.body', { name: deleteTarget.username }) }}</p>
      <template #footer>
        <Button variant="ghost" @click="deleteTarget = null">{{ t('common.cancel') }}</Button>
        <Button variant="danger" @click="onDelete">{{ t('common.delete') }}</Button>
      </template>
    </Modal>
  </section>
</template>

<style scoped>
.users { display: flex; flex-direction: column; gap: var(--space-5); }
.users__head { display: flex; justify-content: space-between; align-items: flex-end; gap: var(--space-4); }
.users__form { display: flex; flex-direction: column; gap: var(--space-3); }
.users__drawer { display: grid; grid-template-columns: 100px 1fr; gap: var(--space-2) var(--space-4); margin: 0; }
.users__drawer dt { color: var(--muted); font-size: var(--fs-11); font-family: var(--font-mono); text-transform: uppercase; letter-spacing: 0.06em; }
.users__drawer dd { margin: 0; font-size: var(--fs-13); }
</style>