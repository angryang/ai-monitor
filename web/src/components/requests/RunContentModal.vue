<script setup lang="ts">
/**
 * Customise the content of one batch before it is fired.
 *
 * The dialog is prefilled with each request's saved content. Anything the
 * operator edits becomes a per-item override attached to this run only —
 * the predefined requests in the store are never rewritten. "Use the
 * predefined content" is a one-click escape hatch for the common case.
 */

import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Badge from '../ui/Badge.vue'
import Button from '../ui/Button.vue'
import Modal from '../ui/Modal.vue'
import type { AIRequest, RequestContentOverride, RequestContentOverrides } from '../../api/requests'

interface Props {
  open: boolean
  /** Requests about to be fired, in the order they will appear. */
  items: AIRequest[]
  busy?: boolean
}

const props = withDefaults(defineProps<Props>(), { busy: false })
const emit = defineEmits<{
  close: []
  submit: [overrides: RequestContentOverrides]
}>()

const { t } = useI18n()

interface Draft {
  item: AIRequest
  userPrompt: string
  systemPrompt: string
  varsText: string
  advanced: boolean
}

const drafts = ref<Draft[]>([])
const bulk = ref('')
const errors = ref<Record<string, string>>({})

function varsTextOf(vars: Record<string, string> | undefined): string {
  return Object.entries(vars ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join('\n')
}

watch(
  () => props.open,
  (open) => {
    if (!open) return
    bulk.value = ''
    errors.value = {}
    drafts.value = props.items.map((item) => ({
      item,
      userPrompt: item.user_prompt ?? '',
      systemPrompt: item.system_prompt ?? '',
      varsText: varsTextOf(item.prompt_vars),
      advanced: false,
    }))
  },
  { immediate: true },
)

/** Parse the "key=value" lines back into a map. */
function parseVars(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed) continue
    const eq = trimmed.indexOf('=')
    if (eq <= 0) continue
    out[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1).trim()
  }
  return out
}

function varsEqual(d: Draft): boolean {
  const current = parseVars(d.varsText)
  const saved = d.item.prompt_vars ?? {}
  const keys = new Set([...Object.keys(current), ...Object.keys(saved)])
  for (const key of keys) {
    if ((current[key] ?? '') !== (saved[key] ?? '')) return false
  }
  return true
}

function isModified(d: Draft): boolean {
  return (
    d.userPrompt !== (d.item.user_prompt ?? '') ||
    d.systemPrompt !== (d.item.system_prompt ?? '') ||
    !varsEqual(d)
  )
}

const modifiedCount = computed(() => drafts.value.filter(isModified).length)

function applyBulk() {
  if (!bulk.value.trim()) return
  for (const d of drafts.value) d.userPrompt = bulk.value
  errors.value = {}
}

function resetOne(d: Draft) {
  d.userPrompt = d.item.user_prompt ?? ''
  d.systemPrompt = d.item.system_prompt ?? ''
  d.varsText = varsTextOf(d.item.prompt_vars)
  delete errors.value[d.item.id]
}

function resetAll() {
  for (const d of drafts.value) resetOne(d)
  bulk.value = ''
}

/** Only the fields that actually changed are sent as an override. */
function buildOverrides(): RequestContentOverrides {
  const out: RequestContentOverrides = {}
  for (const d of drafts.value) {
    if (!isModified(d)) continue
    const o: RequestContentOverride = {}
    if (d.userPrompt !== (d.item.user_prompt ?? '')) o.user_prompt = d.userPrompt
    if (d.systemPrompt !== (d.item.system_prompt ?? '')) o.system_prompt = d.systemPrompt
    if (!varsEqual(d)) o.prompt_vars = parseVars(d.varsText)
    out[d.item.id] = o
  }
  return out
}

function validate(): boolean {
  const e: Record<string, string> = {}
  for (const d of drafts.value) {
    if (!d.userPrompt.trim()) e[d.item.id] = t('req.customize.errUserPrompt')
  }
  errors.value = e
  return Object.keys(e).length === 0
}

/** `usePredefined` skips the validation of the drafts and sends no overrides. */
function submit(usePredefined: boolean) {
  if (usePredefined) {
    emit('submit', {})
    return
  }
  if (!validate()) return
  emit('submit', buildOverrides())
}
</script>

<template>
  <Modal :open="open" :title="t('req.customize.title')" size="lg" @close="emit('close')">
    <div class="cust">
      <p class="cust__hint text-muted">{{ t('req.customize.hint') }}</p>

      <div class="cust__bulk">
        <label class="cust__label" for="cust-bulk">{{ t('req.customize.bulkLabel') }}</label>
        <textarea
          id="cust-bulk"
          v-model="bulk"
          rows="2"
          :placeholder="t('req.customize.bulkPlaceholder')"
        />
        <div class="cust__bulk-actions">
          <Button size="sm" variant="ghost" :disabled="!bulk.trim()" @click="applyBulk">
            {{ t('req.customize.applyAll') }}
          </Button>
          <span class="text-mono text-muted cust__count">
            {{ t('req.customize.count', { n: drafts.length }) }} ·
            {{ t('req.customize.modifiedCount', { n: modifiedCount }) }}
          </span>
          <Button v-if="modifiedCount > 0" size="sm" variant="ghost" @click="resetAll">
            {{ t('req.customize.resetAll') }}
          </Button>
        </div>
      </div>

      <div class="cust__list">
        <article
          v-for="d in drafts"
          :key="d.item.id"
          class="cust__card"
          :class="{ 'cust__card--modified': isModified(d) }"
        >
          <header class="cust__card-head">
            <strong class="cust__name">{{ d.item.name }}</strong>
            <span v-if="d.item.provider" class="text-mono text-muted cust__group">
              {{ d.item.provider.name }} · {{ d.item.model }}
            </span>
            <Badge v-if="isModified(d)" tone="accent">{{ t('req.customize.modified') }}</Badge>
            <span class="cust__spacer" />
            <button class="cust__link" type="button" @click="d.advanced = !d.advanced">
              {{ d.advanced ? t('req.customize.advancedHide') : t('req.customize.advanced') }}
            </button>
            <button v-if="isModified(d)" class="cust__link" type="button" @click="resetOne(d)">
              {{ t('req.customize.reset') }}
            </button>
          </header>

          <label class="cust__label" :for="`cust-user-${d.item.id}`">{{ t('req.form.userPrompt') }}</label>
          <textarea :id="`cust-user-${d.item.id}`" v-model="d.userPrompt" rows="3" />
          <span v-if="errors[d.item.id]" class="cust__error">{{ errors[d.item.id] }}</span>

          <template v-if="d.advanced">
            <label class="cust__label" :for="`cust-sys-${d.item.id}`">{{ t('req.form.systemPrompt') }}</label>
            <textarea :id="`cust-sys-${d.item.id}`" v-model="d.systemPrompt" rows="2" />
            <label class="cust__label" :for="`cust-vars-${d.item.id}`">{{ t('req.form.promptVars') }}</label>
            <textarea
              :id="`cust-vars-${d.item.id}`"
              v-model="d.varsText"
              rows="2"
              :placeholder="t('req.form.promptVarsHint')"
            />
          </template>
        </article>
      </div>
    </div>

    <template #footer>
      <Button variant="ghost" @click="emit('close')">{{ t('common.cancel') }}</Button>
      <Button variant="ghost" :loading="busy" @click="submit(true)">
        {{ t('req.customize.usePredefined') }}
      </Button>
      <Button :loading="busy" @click="submit(false)">{{ t('req.customize.runCustomized') }}</Button>
    </template>
  </Modal>
</template>

<style scoped>
.cust { display: flex; flex-direction: column; gap: var(--space-4); }
.cust__hint { margin: 0; font-size: var(--fs-13); line-height: var(--lh-base); }
.cust__bulk {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-4);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  background: var(--surface-2);
}
.cust__label {
  display: block;
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--muted);
  margin-top: var(--space-2);
}
.cust__label:first-child { margin-top: 0; }
.cust__bulk-actions { display: flex; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
.cust__count { flex: 1; font-size: var(--fs-11); }
.cust__list { display: flex; flex-direction: column; gap: var(--space-3); max-height: 45vh; overflow: auto; }
.cust__card {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-3) var(--space-4) var(--space-4);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  background: var(--surface);
}
.cust__card--modified { border-color: var(--accent); }
.cust__card-head { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; margin-bottom: var(--space-2); }
.cust__name { font-size: var(--fs-13); font-weight: var(--fw-medium); }
.cust__group { font-size: var(--fs-11); }
.cust__spacer { flex: 1; }
.cust__link {
  background: transparent;
  border: 0;
  color: var(--accent);
  cursor: pointer;
  font-size: var(--fs-11);
  font-family: var(--font-mono);
  padding: 0;
}
.cust__link:hover { text-decoration: underline; }
.cust__error { color: var(--danger); font-size: var(--fs-11); }
</style>
