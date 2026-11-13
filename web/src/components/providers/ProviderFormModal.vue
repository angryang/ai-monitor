<script setup lang="ts">
/**
 * Create / edit an AI service provider.
 *
 * A provider is the reusable description of one upstream AI endpoint:
 * protocol, base url, credentials, the set of models it offers and the
 * protocol options. A provider carries a list of candidate models; the
 * operator can add / remove rows (the first is the primary model) and test
 * any single model before saving. Connectivity checks are explicit actions
 * now — saving a provider never triggers one and there is no force-save.
 */

import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Button from '../ui/Button.vue'
import FormField from '../ui/FormField.vue'
import Badge from '../ui/Badge.vue'
import Switch from '../ui/Switch.vue'
import Modal from '../ui/Modal.vue'
import type {
  Protocol,
  Provider,
  ProviderCheck,
  ProviderWritePayload,
} from '../../api/providers'

interface Props {
  open: boolean
  /** null → create mode, otherwise the provider being edited. */
  provider: Provider | null
  busy?: boolean
  checking?: boolean
  /** The verdict of the last connectivity check, if there was one. */
  checkResult?: ProviderCheck | null
}

const props = withDefaults(defineProps<Props>(), { busy: false, checking: false, checkResult: null })
const emit = defineEmits<{
  close: []
  submit: [payload: ProviderWritePayload]
  /** Test one specific model of the current payload. */
  check: [payload: ProviderWritePayload, model: string]
}>()

const { t } = useI18n()

const isEdit = computed(() => props.provider !== null)

const form = ref<ProviderWritePayload>(blank())
const errors = ref<Record<string, string>>({})

function blank(): ProviderWritePayload {
  return {
    name: '',
    description: '',
    enabled: true,
    protocol: 'openai',
    base_url: '',
    api_key: '',
    models: [''],
    timeout: '30s',
    insecure_skip_verify: false,
    thinking_enabled: false,
  }
}

// Re-hydrate the form every time the modal opens so a cancelled edit
// never leaks into the next one.
watch(
  () => props.open,
  (open) => {
    if (!open) return
    errors.value = {}
    const p = props.provider
    form.value = p
      ? {
          name: p.name,
          description: p.description,
          enabled: p.enabled,
          protocol: p.protocol,
          base_url: p.base_url,
          // The server never returns the key — an empty field means
          // "keep the stored one".
          api_key: '',
          // Guard against a null / empty list from an older binary.
          models: p.models?.length ? [...p.models] : [''],
          timeout: p.timeout,
          insecure_skip_verify: p.insecure_skip_verify,
          thinking_enabled: p.thinking_enabled,
        }
      : blank()
  },
  { immediate: true },
)

function addModel() {
  form.value.models.push('')
}

function removeModel(index: number) {
  // Always keep at least one row so a model can be entered.
  if (form.value.models.length <= 1) return
  form.value.models.splice(index, 1)
}

function validate(): boolean {
  const e: Record<string, string> = {}
  if (!form.value.name.trim()) e.name = t('validation.requiredField', { label: t('provider.form.name') })
  if (!form.value.base_url.trim()) {
    e.base_url = t('validation.requiredField', { label: t('provider.form.baseUrl') })
  } else if (!/^https?:\/\//.test(form.value.base_url)) {
    e.base_url = t('validation.urlProtocol')
  }
  const trimmed = form.value.models.map((m) => m.trim())
  if (trimmed.length === 0 || trimmed.some((m) => !m)) {
    e.models = t('provider.form.emptyModel')
  } else if (new Set(trimmed).size !== trimmed.length) {
    e.models = t('provider.form.duplicateModel')
  }
  if (!isEdit.value && !form.value.api_key?.trim()) {
    e.api_key = t('validation.requiredField', { label: t('provider.form.apiKey') })
  }
  errors.value = e
  return Object.keys(e).length === 0
}

/** The payload as it would be sent — the test buttons use it too. */
function payload(): ProviderWritePayload {
  const models = [...new Set(form.value.models.map((m) => m.trim()).filter((m) => m))]
  const out: ProviderWritePayload = {
    name: form.value.name.trim(),
    description: form.value.description ?? '',
    enabled: form.value.enabled ?? true,
    protocol: form.value.protocol as Protocol,
    base_url: form.value.base_url.trim(),
    models,
    timeout: form.value.timeout || '30s',
    insecure_skip_verify: !!form.value.insecure_skip_verify,
    thinking_enabled: !!form.value.thinking_enabled,
  }
  if (form.value.api_key) out.api_key = form.value.api_key
  return out
}

function submit() {
  if (!validate()) return
  emit('submit', payload())
}

/** Run a connectivity check against one named model. */
function checkModel(model: string) {
  if (!validate()) return
  emit('check', payload(), model.trim())
}
</script>

<template>
  <Modal
    :open="open"
    :title="isEdit ? t('provider.form.editTitle') : t('provider.form.newTitle')"
    size="md"
    @close="emit('close')"
  >
    <div class="pform">
      <FormField :label="t('provider.form.name')" required :error="errors.name" id="prov-name">
        <input id="prov-name" v-model="form.name" type="text" autocomplete="off" />
      </FormField>

      <FormField :label="t('provider.form.description')" id="prov-desc">
        <input id="prov-desc" v-model="form.description" type="text" autocomplete="off" />
      </FormField>

      <div class="pform__row">
        <FormField :label="t('provider.form.protocol')" required id="prov-proto">
          <select id="prov-proto" v-model="form.protocol">
            <option value="openai">openai</option>
            <option value="anthropic">anthropic</option>
            <option value="gemini">gemini</option>
          </select>
        </FormField>

        <FormField :label="t('provider.form.baseUrl')" required :error="errors.base_url" id="prov-url">
          <input id="prov-url" v-model="form.base_url" type="text" autocomplete="off" placeholder="https://api.example.com/v1" />
        </FormField>
      </div>

      <FormField
        :label="t('provider.form.models')"
        required
        :error="errors.models"
        :hint="t('provider.form.modelsHint')"
      >
        <div class="pform__models">
          <div v-for="(m, i) in form.models" :key="i" class="pform__model-row">
            <Badge v-if="i === 0" tone="accent">{{ t('provider.form.primaryModel') }}</Badge>
            <input
              :id="`prov-model-${i}`"
              v-model="form.models[i]"
              type="text"
              autocomplete="off"
              :aria-label="t('provider.form.models') + ' ' + (i + 1)"
            />
            <Button size="sm" variant="ghost" :disabled="checking" @click="checkModel(form.models[i])">
              {{ t('provider.form.testModel') }}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              :disabled="form.models.length <= 1"
              @click="removeModel(i)"
            >
              {{ t('provider.form.removeModel') }}
            </Button>
          </div>
          <Button size="sm" variant="ghost" class="pform__add" @click="addModel">
            + {{ t('provider.form.addModel') }}
          </Button>
        </div>
      </FormField>

      <FormField
        :label="isEdit ? t('provider.form.apiKeyKeep') : t('provider.form.apiKey')"
        :required="!isEdit"
        :error="errors.api_key"
        :hint="t('provider.form.apiKeyHint')"
        id="prov-key"
      >
        <input id="prov-key" v-model="form.api_key" type="password" autocomplete="off" />
      </FormField>

      <FormField :label="t('provider.form.timeout')" :hint="t('provider.form.timeoutHint')" id="prov-timeout">
        <input id="prov-timeout" v-model="form.timeout" type="text" autocomplete="off" placeholder="30s" />
      </FormField>

      <Switch
        v-model="form.enabled"
        :label="t('provider.form.enabled')"
        :description="t('provider.form.enabledHint')"
      />
      <Switch
        v-model="form.insecure_skip_verify"
        :label="t('provider.form.insecureSkipVerify')"
        :description="t('provider.form.insecureSkipVerifyHint')"
      />
      <Switch
        v-model="form.thinking_enabled"
        :label="t('provider.form.thinkingLabel')"
        :description="t('provider.form.thinkingHint')"
      />

      <div class="pform__check">
        <Button size="sm" variant="ghost" :loading="checking" @click="checkModel(form.models[0] ?? '')">
          {{ checking ? t('provider.form.checking') : t('provider.form.check') }}
        </Button>

        <div v-if="checkResult" class="pform__verdict" :data-ok="checkResult.ok">
          <span class="text-mono pform__tested">{{ t('provider.check.model', { model: checkResult.model }) }}</span>
          <strong>
            {{
              checkResult.ok
                ? t('provider.check.ok', { ms: checkResult.latency_ms })
                : t('provider.check.failed', { kind: t(`provider.kind.${checkResult.kind}`) })
            }}
          </strong>
          <span v-if="checkResult.status_code" class="text-mono">
            HTTP {{ checkResult.status_code }}
          </span>
          <span v-if="checkResult.url" class="text-mono pform__url">{{ checkResult.url }}</span>
          <p v-if="!checkResult.ok && checkResult.message" class="pform__msg">{{ checkResult.message }}</p>
        </div>
      </div>
    </div>

    <template #footer>
      <Button variant="ghost" @click="emit('close')">{{ t('common.cancel') }}</Button>
      <Button :loading="busy" @click="submit">
        {{ isEdit ? t('common.save') : t('common.create') }}
      </Button>
    </template>
  </Modal>
</template>

<style scoped>
.pform { display: flex; flex-direction: column; gap: var(--space-4); }
.pform__row { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-4); }
.pform__models { display: flex; flex-direction: column; gap: var(--space-2); }
.pform__model-row { display: flex; align-items: center; gap: var(--space-2); }
.pform__model-row :deep(input) { flex: 1; min-width: 0; }
.pform__model-row :deep(.btn) { flex-shrink: 0; }
.pform__add { align-self: flex-start; }
.pform__check {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding-top: var(--space-2);
  border-top: 1px solid var(--border);
}
.pform__verdict {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-3) var(--space-4);
  border: 1px solid var(--danger);
  border-radius: var(--radius-md);
  background: rgba(216, 68, 63, 0.1);
  font-size: var(--fs-12);
}
.pform__verdict[data-ok='true'] {
  border-color: var(--ok);
  background: rgba(20, 160, 109, 0.1);
}
.pform__verdict strong { font-weight: var(--fw-semibold); }
.pform__verdict[data-ok='true'] strong { color: var(--ok); }
.pform__verdict[data-ok='false'] strong { color: var(--danger); }
.pform__tested { font-size: var(--fs-11); color: var(--muted); }
.pform__url { font-size: var(--fs-11); color: var(--muted); word-break: break-all; }
.pform__msg {
  margin: var(--space-1) 0 0;
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  color: var(--muted);
  word-break: break-all;
}
</style>
