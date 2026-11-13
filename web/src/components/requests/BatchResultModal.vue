<script setup lang="ts">
/**
 * Batch result panel. The server answers the run endpoints with 202 and a
 * batch id; the requests themselves keep running in the background, so
 * this panel polls the batch and renders the per-request outcomes as they
 * land.
 */

import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Badge from '../ui/Badge.vue'
import Button from '../ui/Button.vue'
import Modal from '../ui/Modal.vue'
import type { BatchItemResult, RequestBatch } from '../../api/requests'

interface Props {
  open: boolean
  batch: RequestBatch | null
  polling?: boolean
}

const props = withDefaults(defineProps<Props>(), { polling: false })
const emit = defineEmits<{ close: [] }>()

const { t } = useI18n()

/** Ids of the results whose captured content is expanded. */
const expanded = ref<string[]>([])

const isRunning = computed(() => props.batch?.state === 'running')

function toggle(id: string) {
  const idx = expanded.value.indexOf(id)
  if (idx >= 0) expanded.value.splice(idx, 1)
  else expanded.value.push(id)
}

function isOpen(id: string) {
  return expanded.value.includes(id)
}

function statusTone(status: string): 'ok' | 'danger' | 'warn' | 'muted' {
  if (status === 'success') return 'ok'
  if (status === 'unauthorized') return 'warn'
  if (status === 'failure') return 'danger'
  return 'muted'
}

function statusLabel(status: string) {
  return t(`runs.status.${status}`)
}

function skipReason(reason: string) {
  const map: Record<string, string> = {
    not_found: t('req.batch.skipNotFound'),
    secret_error: t('req.batch.skipSecretError'),
    no_api_key: t('req.batch.skipNoApiKey'),
    provider_missing: t('req.batch.skipProviderMissing'),
  }
  return map[reason] ?? reason
}

function hasContent(r: BatchItemResult) {
  return !!(r.output || r.thinking || r.input)
}
</script>

<template>
  <Modal :open="open" :title="t('req.batch.title')" size="lg" @close="emit('close')">
    <div v-if="!batch" class="batch__empty text-muted">{{ t('common.loading') }}</div>

    <div v-else class="batch">
      <div class="batch__summary">
        <div class="batch__stat">
          <span class="batch__stat-value">{{ batch.completed }} / {{ batch.total }}</span>
          <span class="batch__stat-label">{{ t('req.batch.progress') }}</span>
        </div>
        <div class="batch__stat">
          <span class="batch__stat-value batch__stat-value--ok">{{ batch.succeeded }}</span>
          <span class="batch__stat-label">{{ t('req.batch.succeeded') }}</span>
        </div>
        <div class="batch__stat">
          <span class="batch__stat-value batch__stat-value--danger">{{ batch.failed }}</span>
          <span class="batch__stat-label">{{ t('req.batch.failed') }}</span>
        </div>
        <div class="batch__stat">
          <span class="batch__stat-value">{{ batch.skipped.length }}</span>
          <span class="batch__stat-label">{{ t('req.batch.skipped') }}</span>
        </div>
        <Badge v-if="isRunning" tone="accent">{{ t('req.batch.running') }}</Badge>
        <Badge v-else tone="muted">{{ t('req.batch.done') }}</Badge>
      </div>

      <div v-if="batch.skipped.length > 0" class="batch__skipped">
        <strong>{{ t('req.batch.skippedTitle') }}</strong>
        <ul>
          <li v-for="s in batch.skipped" :key="s.id">
            <span class="text-mono">{{ s.name || s.id }}</span>
            <span class="text-muted"> — {{ skipReason(s.reason) }}</span>
          </li>
        </ul>
      </div>

      <p v-if="batch.results.length === 0 && isRunning" class="text-muted batch__waiting">
        {{ t('req.batch.waiting') }}
      </p>

      <ul class="batch__results">
        <li v-for="r in batch.results" :key="r.request_id" class="batch__item">
          <div class="batch__item-head">
            <Badge :tone="statusTone(r.status)">{{ statusLabel(r.status) }}</Badge>
            <span class="batch__item-name">{{ r.request_name }}</span>
            <span class="batch__item-group text-muted">{{ r.provider_name }}</span>
            <span class="batch__item-metrics text-mono text-muted">
              {{ r.duration_ms }} ms · TTFT {{ r.ttft_ms }} ms · {{ r.chunks }} chunks
            </span>
            <button
              v-if="hasContent(r)"
              class="batch__toggle"
              type="button"
              @click="toggle(r.request_id)"
            >
              {{ isOpen(r.request_id) ? t('req.batch.hideContent') : t('req.batch.showContent') }}
            </button>
          </div>

          <p v-if="r.error" class="batch__error text-mono">{{ r.error }}</p>
          <p v-if="r.x_request_id" class="batch__reqid text-mono text-muted">
            {{ t('runs.row.reqId') }}: {{ r.x_request_id }}
          </p>

          <div v-if="isOpen(r.request_id)" class="batch__content">
            <div v-if="r.input" class="batch__content-block">
              <span class="batch__content-label">{{ t('runs.row.content.input') }}</span>
              <pre class="batch__pre">{{ r.input }}</pre>
            </div>
            <div v-if="r.thinking" class="batch__content-block">
              <span class="batch__content-label">{{ t('runs.row.content.thinking') }}</span>
              <pre class="batch__pre">{{ r.thinking }}</pre>
            </div>
            <div v-if="r.output" class="batch__content-block">
              <span class="batch__content-label">{{ t('runs.row.content.output') }}</span>
              <pre class="batch__pre">{{ r.output }}</pre>
            </div>
          </div>
        </li>
      </ul>
    </div>

    <template #footer>
      <Button variant="ghost" @click="emit('close')">{{ t('common.close') }}</Button>
    </template>
  </Modal>
</template>

<style scoped>
.batch { display: flex; flex-direction: column; gap: var(--space-4); }
.batch__summary {
  display: flex;
  align-items: center;
  gap: var(--space-6);
  flex-wrap: wrap;
}
.batch__stat { display: flex; flex-direction: column; gap: 2px; }
.batch__stat-value {
  font-family: var(--font-display);
  font-size: var(--fs-20);
  font-weight: var(--fw-bold);
}
.batch__stat-value--ok { color: var(--ok); }
.batch__stat-value--danger { color: var(--danger); }
.batch__stat-label {
  font-size: var(--fs-11);
  color: var(--muted);
  text-transform: uppercase;
  letter-spacing: 0.06em;
  font-family: var(--font-mono);
}
.batch__skipped {
  padding: var(--space-3) var(--space-4);
  border: 1px solid var(--warn);
  border-radius: var(--radius-md);
  background: rgba(214, 165, 20, 0.08);
  font-size: var(--fs-12);
}
.batch__skipped ul { margin: var(--space-2) 0 0; padding-left: var(--space-5); }
.batch__waiting { padding: var(--space-4) 0; }
.batch__results { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: var(--space-2); }
.batch__item {
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: var(--space-3) var(--space-4);
  background: var(--surface-2);
}
.batch__item-head { display: flex; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
.batch__item-name { font-weight: var(--fw-semibold); }
.batch__item-metrics { font-size: var(--fs-11); margin-left: auto; }
.batch__toggle {
  background: transparent;
  border: 0;
  color: var(--accent);
  cursor: pointer;
  font-size: var(--fs-11);
  font-family: var(--font-mono);
  padding: 0;
}
.batch__toggle:hover { text-decoration: underline; }
.batch__error {
  margin: var(--space-2) 0 0;
  font-size: var(--fs-12);
  color: var(--danger);
  word-break: break-word;
}
.batch__reqid { margin: var(--space-1) 0 0; font-size: var(--fs-11); }
.batch__content { margin-top: var(--space-3); display: flex; flex-direction: column; gap: var(--space-3); }
.batch__content-block { display: flex; flex-direction: column; gap: var(--space-1); }
.batch__content-label {
  font-size: var(--fs-11);
  color: var(--muted);
  text-transform: uppercase;
  letter-spacing: 0.06em;
  font-family: var(--font-mono);
}
.batch__pre {
  margin: 0;
  padding: var(--space-3);
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--fs-12);
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 320px;
  overflow: auto;
}
</style>
