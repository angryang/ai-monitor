<script setup lang="ts">
/**
 * Create / edit one AI request. A provider offers a set of candidate
 * models; this request selects exactly one of them. Everything else about
 * the upstream (URL, credentials, timeout) lives on the provider, so one
 * edit there reaches every request that uses it.
 */

import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Button from '../ui/Button.vue'
import FormField from '../ui/FormField.vue'
import Switch from '../ui/Switch.vue'
import Modal from '../ui/Modal.vue'
import type { Provider } from '../../api/providers'
import type { AIRequest, AIRequestWritePayload } from '../../api/requests'

/** The part of the payload the form owns directly; `model` is kept separate. */
type RequestForm = Omit<AIRequestWritePayload, 'model'>

interface Props {
  open: boolean
  /** null → create mode, otherwise the request being edited. */
  request: AIRequest | null
  /** Available providers for the dropdown. */
  providers: Provider[]
  busy?: boolean
}

const props = withDefaults(defineProps<Props>(), { busy: false })
const emit = defineEmits<{
  close: []
  submit: [payload: AIRequestWritePayload]
}>()

const { t } = useI18n()

const isEdit = computed(() => props.request !== null)

const form = ref<RequestForm>(blank())
/** The model this request selects out of the chosen provider's candidates. */
const model = ref('')
const errors = ref<Record<string, string>>({})
/** null → inherit the provider setting; true/false → explicit override. */
const thinkingOverride = ref<boolean | null>(null)

function blank(): RequestForm {
  return {
    name: '',
    description: '',
    provider_id: '',
    system_prompt: '',
    user_prompt: '',
    prompt_vars: {},
    order: 0,
  }
}

// Prompt variables are edited as one "key=value" per line — the UI shape
// the task form already uses for the same concept.
const varsText = ref('')

/** The selected provider's default, used as the inherit baseline. */
const chosenProvider = computed<Provider | null>(
  () => props.providers.find((p) => p.id === form.value.provider_id) ?? null,
)

/** Candidate models of the selected provider; the model dropdown is bound to it. */
const modelOptions = computed<string[]>(() => chosenProvider.value?.models ?? [])

// Whenever the provider (or its model list) changes, keep the selected model
// valid: drop it back to the first candidate when the old one is no longer
// offered. This also covers providers arriving after the form was opened.
watch(modelOptions, (options) => {
  if (!options.includes(model.value)) model.value = options[0] ?? ''
})

watch(
  () => props.open,
  (open) => {
    if (!open) return
    errors.value = {}
    const r = props.request
    if (r) {
      form.value = {
        name: r.name,
        description: r.description,
        provider_id: r.provider_id,
        system_prompt: r.system_prompt,
        user_prompt: r.user_prompt,
        prompt_vars: { ...r.prompt_vars },
        order: r.order,
      }
      model.value = r.model
      varsText.value = Object.entries(r.prompt_vars ?? {})
        .map(([k, v]) => `${k}=${v}`)
        .join('\n')
      thinkingOverride.value = r.thinking_override ?? null
    } else {
      form.value = blank()
      // Preselect the first enabled provider AND its primary model so a
      // fresh form is usable without an extra click.
      const first = props.providers.find((p) => p.enabled) ?? props.providers[0]
      if (first) {
        form.value.provider_id = first.id
        model.value = first.models?.[0] ?? ''
      } else {
        model.value = ''
      }
      varsText.value = ''
      thinkingOverride.value = null
    }
  },
  { immediate: true },
)

/** Parse the "key=value" lines back into a map. */
function parseVars(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of varsText.value.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed) continue
    const eq = trimmed.indexOf('=')
    if (eq <= 0) continue
    out[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1).trim()
  }
  return out
}

function validate(): boolean {
  const e: Record<string, string> = {}
  if (!form.value.name.trim()) e.name = t('validation.requiredField', { label: t('req.form.name') })
  if (!form.value.provider_id) e.provider_id = t('validation.requiredField', { label: t('req.form.provider') })
  if (!model.value) e.model = t('validation.requiredField', { label: t('req.form.model') })
  if (!form.value.user_prompt.trim()) {
    e.user_prompt = t('validation.requiredField', { label: t('req.form.userPrompt') })
  }
  errors.value = e
  return Object.keys(e).length === 0
}

function submit() {
  if (!validate()) return
  const payload: AIRequestWritePayload = {
    name: form.value.name.trim(),
    description: form.value.description ?? '',
    provider_id: form.value.provider_id,
    model: model.value,
    system_prompt: form.value.system_prompt ?? '',
    user_prompt: form.value.user_prompt,
    prompt_vars: parseVars(),
    order: form.value.order ?? 0,
  }
  // Only send the override when the operator made one — omitting it keeps
  // the stored value on PATCH.
  if (thinkingOverride.value !== null) payload.thinking_override = thinkingOverride.value
  emit('submit', payload)
}
</script>

<template>
  <Modal :open="open" :title="isEdit ? t('req.form.editTitle') : t('req.form.newTitle')" size="md" @close="emit('close')">
    <div class="rform">
      <FormField :label="t('req.form.name')" required :error="errors.name" id="req-name">
        <input id="req-name" v-model="form.name" type="text" autocomplete="off" />
      </FormField>

      <FormField :label="t('req.form.description')" id="req-desc">
        <input id="req-desc" v-model="form.description" type="text" autocomplete="off" />
      </FormField>

      <FormField
        :label="t('req.form.provider')"
        required
        :error="errors.provider_id"
        :hint="t('req.form.providerHint')"
        id="req-provider"
      >
        <select id="req-provider" v-model="form.provider_id">
          <option value="" disabled>{{ t('req.form.providerPlaceholder') }}</option>
          <option v-for="p in providers" :key="p.id" :value="p.id" :disabled="!p.enabled">
            {{ p.name }} · {{ p.protocol }} · {{ t('req.form.modelCount', { n: p.models?.length ?? 0 }) }}{{ p.enabled ? '' : ' (' + t('common.disabled') + ')' }}
          </option>
        </select>
        <p v-if="providers.length === 0" class="rform__no-provider">
          {{ t('req.form.noProviders') }}
        </p>
      </FormField>

      <FormField
        :label="t('req.form.model')"
        required
        :error="errors.model"
        :hint="t('req.form.modelHint')"
        id="req-model"
      >
        <select id="req-model" v-model="model">
          <option value="" disabled>{{ t('req.form.modelPlaceholder') }}</option>
          <option v-for="m in modelOptions" :key="m" :value="m">{{ m }}</option>
        </select>
      </FormField>

      <FormField :label="t('req.form.systemPrompt')" id="req-system">
        <textarea id="req-system" v-model="form.system_prompt" rows="2" />
      </FormField>

      <FormField :label="t('req.form.userPrompt')" required :error="errors.user_prompt" id="req-user">
        <textarea id="req-user" v-model="form.user_prompt" rows="4" />
      </FormField>

      <FormField :label="t('req.form.promptVars')" :hint="t('req.form.promptVarsHint')" id="req-vars">
        <textarea id="req-vars" v-model="varsText" rows="3" placeholder="who=world&#10;lang=zh" />
      </FormField>

      <FormField :label="t('req.form.order')" id="req-order">
        <input id="req-order" v-model.number="form.order" type="number" min="0" step="1" />
      </FormField>

      <div class="rform__thinking">
        <Switch
          :model-value="thinkingOverride === true"
          :label="t('req.form.thinkingOverride')"
          :description="
            thinkingOverride === null
              ? t('req.form.thinkingInherit', {
                  state: chosenProvider?.thinking_enabled ? t('common.enabled') : t('common.disabled'),
                })
              : t('req.form.thinkingExplicit')
          "
          @update:model-value="(v: boolean) => (thinkingOverride = v ? true : null)"
        />
        <button
          v-if="thinkingOverride !== null"
          class="rform__thinking-reset"
          type="button"
          @click="thinkingOverride = null"
        >
          {{ t('req.form.thinkingReset') }}
        </button>
      </div>
    </div>

    <template #footer>
      <Button variant="ghost" @click="emit('close')">{{ t('common.cancel') }}</Button>
      <Button :loading="busy" :disabled="providers.length === 0" @click="submit">
        {{ isEdit ? t('common.save') : t('common.create') }}
      </Button>
    </template>
  </Modal>
</template>

<style scoped>
.rform { display: flex; flex-direction: column; gap: var(--space-4); }
.rform__thinking { display: flex; flex-direction: column; gap: var(--space-2); }
.rform__thinking-reset {
  align-self: flex-start;
  background: transparent;
  border: 0;
  color: var(--accent);
  cursor: pointer;
  font-size: var(--fs-11);
  font-family: var(--font-mono);
  padding: 0;
}
.rform__thinking-reset:hover { text-decoration: underline; }
.rform__no-provider { color: var(--danger); font-size: var(--fs-11); margin: var(--space-2) 0 0; }
</style>
