<script setup lang="ts">
interface Props {
  label: string
  hint?: string
  error?: string
  required?: boolean
  id?: string
}
withDefaults(defineProps<Props>(), { required: false })
</script>

<template>
  <div class="field" :class="{ 'field--invalid': !!error }">
    <label class="field__label text-mono" :for="id">
      {{ label }}<span v-if="required" class="field__req">*</span>
    </label>
    <slot />
    <p v-if="error" class="field__error">{{ error }}</p>
    <p v-else-if="hint" class="field__hint">{{ hint }}</p>
  </div>
</template>

<style scoped>
.field {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.field__label {
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--muted);
}
.field__req {
  color: var(--accent);
  margin-left: var(--space-1);
}
.field__hint {
  font-size: var(--fs-11);
  color: var(--muted-2);
}
.field__error {
  font-size: var(--fs-11);
  color: var(--danger);
}
.field :deep(input),
.field :deep(textarea),
.field :deep(select) {
  width: 100%;
  background: var(--surface-2);
  color: var(--text);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-md);
  padding: var(--space-2) var(--space-3);
  font-size: var(--fs-13);
  font-family: var(--font-body);
  transition: border-color var(--dur-fast) var(--ease-out),
    box-shadow var(--dur-fast) var(--ease-out);
}
.field :deep(input:focus),
.field :deep(textarea:focus),
.field :deep(select:focus) {
  outline: none;
  border-color: var(--accent);
  box-shadow: 0 0 0 2px var(--accent-soft);
}
.field--invalid :deep(input),
.field--invalid :deep(textarea),
.field--invalid :deep(select) {
  border-color: var(--danger);
}
</style>