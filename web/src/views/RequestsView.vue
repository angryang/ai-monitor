<script setup lang="ts">
/**
 * AI request manager.
 *
 * One flat list of predefined AI requests. Each request names the AI
 * service provider it is sent to, so different rows may target different
 * upstreams and one batch can span several of them. The operator selects
 * one request (or many) and fires them together — execution is
 * asynchronous because an AI request can outlive the HTTP write timeout.
 *
 * Manual executions deliberately do not feed the monitoring signal: they
 * are recorded in the run history but never count towards the success
 * rate and never raise an alert.
 */

import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRequestsStore } from '../stores/requests'
import { useProvidersStore } from '../stores/providers'
import { useRole } from '../composables/useRole'
import { useToast } from '../utils/toast'
import Badge from '../components/ui/Badge.vue'
import Button from '../components/ui/Button.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import RequestFormModal from '../components/requests/RequestFormModal.vue'
import BatchResultModal from '../components/requests/BatchResultModal.vue'
import RunContentModal from '../components/requests/RunContentModal.vue'
import type { AIRequest, AIRequestWritePayload, RequestContentOverrides } from '../api/requests'

const { t } = useI18n()
const store = useRequestsStore()
const providers = useProvidersStore()
const toast = useToast()
const { isAdmin } = useRole()

onMounted(() => {
  void store.fetchAll()
  void providers.fetchAll()
})

const allSelected = computed(
  () => store.requests.length > 0 && store.requests.every((r) => store.selected.includes(r.id)),
)

// ---- request form ----
const formOpen = ref(false)
const beingEdited = ref<AIRequest | null>(null)
const formBusy = ref(false)

function openNew() {
  beingEdited.value = null
  formOpen.value = true
}

function openEdit(r: AIRequest) {
  beingEdited.value = r
  formOpen.value = true
}

async function submitForm(payload: AIRequestWritePayload) {
  formBusy.value = true
  try {
    if (beingEdited.value) {
      await store.update(beingEdited.value.id, payload)
      toast.success(t('toast.reqUpdated'))
    } else {
      await store.create(payload)
      toast.success(t('toast.reqCreated'))
    }
    formOpen.value = false
  } catch (e) {
    toast.error(t('toast.errSave'), (e as Error).message)
  } finally {
    formBusy.value = false
  }
}

async function remove(r: AIRequest) {
  if (!window.confirm(t('req.list.deleteConfirm', { name: r.name }))) return
  try {
    await store.remove(r.id)
    toast.success(t('toast.reqDeleted'))
  } catch (e) {
    toast.error(t('toast.errDelete'), (e as Error).message)
  }
}

/** Whether the provider of one request is usable right now. */
function runnable(r: AIRequest): boolean {
  const p = r.provider
  if (!p) return false
  return p.enabled && p.has_api_key && !p.secret_error
}

// ---- execution ----
const resultOpen = ref(false)
const customizeOpen = ref(false)
const customizeItems = ref<AIRequest[]>([])
const runBusy = ref(false)

function openCustomize(ids: string[]) {
  if (ids.length === 0) return
  customizeItems.value = ids
    .map((id) => store.requests.find((r) => r.id === id))
    .filter((r): r is AIRequest => r !== undefined)
  customizeOpen.value = true
}

function runSelected() {
  openCustomize(store.runnableSelected)
}

function runOne(r: AIRequest) {
  if (!runnable(r)) return
  openCustomize([r.id])
}

async function fire(overrides: RequestContentOverrides) {
  runBusy.value = true
  try {
    // Both a single row and a selection go through the same batch
    // endpoint; the ids are simply fewer in the former case.
    await store.runIds(customizeItems.value.map((r) => r.id), overrides)
    customizeOpen.value = false
    resultOpen.value = true
  } catch (e) {
    toast.error(t('toast.errRun'), (e as Error).message)
  } finally {
    runBusy.value = false
  }
}
</script>

<template>
  <section class="reqs">
    <header class="reqs__head">
      <div>
        <h1>{{ t('req.list.title') }}</h1>
        <p class="text-muted">{{ t('req.list.subtitle') }}</p>
      </div>
      <div class="reqs__actions">
        <Button v-if="isAdmin" @click="openNew" :disabled="providers.providers.length === 0">
          {{ t('req.list.new') }}
        </Button>
      </div>
    </header>

    <div v-if="providers.providers.length === 0 && !providers.loading" class="reqs__alert" role="alert">
      <strong>{{ t('req.list.noProvidersTitle') }}</strong>
      <span>{{ t('req.list.noProvidersDesc') }}</span>
    </div>

    <div class="reqs__toolbar">
      <input
        type="checkbox"
        class="reqs__check-all"
        :checked="allSelected"
        :aria-label="t('req.list.selectAll')"
        @change="store.selectAll()"
      />
      <span class="reqs__selected text-mono">
        {{ t('req.list.selectedCount', { n: store.runnableSelected.length }) }}
      </span>
      <span class="reqs__spacer" />
      <Button
        v-if="isAdmin"
        size="sm"
        variant="primary"
        :disabled="store.runnableSelected.length === 0"
        :loading="store.polling"
        @click="runSelected"
      >
        {{ t('req.list.runSelected') }}
      </Button>
      <Button size="sm" variant="ghost" :disabled="store.selected.length === 0" @click="store.clearSelection()">
        {{ t('req.list.clearSelection') }}
      </Button>
    </div>

    <div v-if="store.loading" class="text-muted">{{ t('common.loading') }}</div>

    <template v-else>
      <EmptyState
        v-if="store.requests.length === 0"
        :title="t('req.list.emptyTitle')"
        :description="t('req.list.emptyDesc')"
        glyph="⇉"
      >
        <template #action>
          <Button v-if="isAdmin" :disabled="providers.providers.length === 0" @click="openNew">
            {{ t('req.list.new') }}
          </Button>
        </template>
      </EmptyState>

      <div v-else class="reqs__table-wrap">
        <table class="reqs__table">
          <thead>
            <tr>
              <th class="reqs__pick" />
              <th>{{ t('req.list.col.name') }}</th>
              <th>{{ t('req.list.col.provider') }}</th>
              <th>{{ t('req.list.col.prompt') }}</th>
              <th class="reqs__order">{{ t('req.list.col.order') }}</th>
              <th class="reqs__row-actions">{{ t('req.list.col.actions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in store.requests" :key="r.id" class="reqs__row">
              <td class="reqs__pick">
                <input
                  type="checkbox"
                  :checked="store.selected.includes(r.id)"
                  :aria-label="r.name"
                  @change="store.toggleSelection(r.id)"
                />
              </td>
              <td>
                <div class="reqs__name">{{ r.name }}</div>
                <div v-if="r.description" class="reqs__desc text-muted">{{ r.description }}</div>
                <span
                  v-if="r.thinking_override !== null && r.thinking_override !== undefined"
                  class="reqs__tag text-mono"
                >
                  {{ r.thinking_override ? t('req.list.thinkingOn') : t('req.list.thinkingOff') }}
                </span>
              </td>
              <td>
                <template v-if="r.provider">
                  <div class="reqs__provider">
                    <span class="reqs__provider-name">{{ r.provider.name }}</span>
                    <Badge tone="accent">{{ r.provider.protocol }}</Badge>
                  </div>
                  <div class="reqs__model text-mono text-muted">{{ r.model }}</div>
                </template>
                <span v-else class="reqs__missing">{{ t('req.list.providerMissing') }}</span>
              </td>
              <td class="reqs__prompt text-mono">{{ r.user_prompt }}</td>
              <td class="reqs__order text-mono">{{ r.order }}</td>
              <td class="reqs__row-actions">
                <Button
                  v-if="isAdmin"
                  size="sm"
                  variant="ghost"
                  :disabled="!runnable(r)"
                  @click="runOne(r)"
                >
                  {{ t('req.list.runOne') }}
                </Button>
                <Button v-if="isAdmin" size="sm" variant="ghost" @click="openEdit(r)">
                  {{ t('common.edit') }}
                </Button>
                <Button v-if="isAdmin" size="sm" variant="danger" @click="remove(r)">
                  {{ t('common.delete') }}
                </Button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <p class="reqs__summary text-mono text-muted">
        {{ t('req.list.summary', { n: store.requests.length, p: providers.providers.length }) }}
      </p>
    </template>

    <RequestFormModal
      :open="formOpen"
      :request="beingEdited"
      :providers="providers.providers"
      :busy="formBusy"
      @close="formOpen = false"
      @submit="submitForm"
    />

    <RunContentModal
      :open="customizeOpen"
      :items="customizeItems"
      :busy="runBusy"
      @close="customizeOpen = false"
      @submit="fire"
    />

    <BatchResultModal
      :open="resultOpen"
      :batch="store.batch"
      :polling="store.polling"
      @close="resultOpen = false"
    />
  </section>
</template>

<style scoped>
.reqs { display: flex; flex-direction: column; gap: var(--space-5); }
.reqs__head {
  display: flex;
  justify-content: space-between;
  align-items: flex-end;
  gap: var(--space-4);
}
.reqs__actions { display: flex; gap: var(--space-3); }
.reqs__toolbar {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-3) var(--space-4);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  background: var(--surface);
}
.reqs__check-all { width: 16px; }
.reqs__selected { font-size: var(--fs-12); }
.reqs__spacer { flex: 1; }
.reqs__alert {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-4);
  border: 1px solid var(--accent);
  border-radius: var(--radius-lg);
  background: var(--surface-2);
  font-size: var(--fs-13);
  line-height: var(--lh-base);
}
.reqs__alert strong { color: var(--accent); font-weight: var(--fw-semibold); }
.reqs__table-wrap { border: 1px solid var(--border); border-radius: var(--radius-lg); overflow: auto; background: var(--surface); }
.reqs__table { width: 100%; border-collapse: collapse; font-size: var(--fs-13); }
.reqs__table th {
  text-align: left;
  font-weight: var(--fw-medium);
  color: var(--muted);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  padding: var(--space-3);
  border-bottom: 1px solid var(--border);
  background: var(--surface-2);
  white-space: nowrap;
}
.reqs__table td {
  padding: var(--space-3);
  border-bottom: 1px solid var(--border);
  vertical-align: middle;
}
.reqs__row:last-child td { border-bottom: 0; }
.reqs__row:hover { background: var(--surface-2); }
.reqs__pick { width: 36px; }
.reqs__order { width: 60px; }
.reqs__row-actions { width: 230px; white-space: nowrap; }
.reqs__row-actions :deep(button) { margin-right: var(--space-1); }
.reqs__name { font-weight: var(--fw-medium); }
.reqs__desc { font-size: var(--fs-11); margin-top: 2px; }
.reqs__tag {
  display: inline-block;
  margin-top: var(--space-1);
  font-size: var(--fs-10);
  color: var(--muted);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-pill);
  padding: 1px var(--space-2);
}
.reqs__provider { display: flex; align-items: center; gap: var(--space-2); }
.reqs__provider-name { font-weight: var(--fw-medium); }
.reqs__model { font-size: var(--fs-11); margin-top: 2px; }
.reqs__missing { color: var(--danger); font-size: var(--fs-11); }
.reqs__prompt {
  color: var(--muted);
  max-width: 360px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.reqs__summary { font-size: var(--fs-11); margin: 0; }
</style>
