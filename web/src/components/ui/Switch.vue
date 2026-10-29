<script setup lang="ts">
/**
 * Switch — 表单开关控件，视觉语言与 FormField 对齐：
 * mono 标签 + surface-2 轨道 + accent 橙色激活态。
 * 底层保留真实 checkbox 以保证键盘可达性与表单语义。
 */
interface Props {
  label: string
  description?: string
  /** undefined is treated as off so optional payload fields can bind directly. */
  modelValue?: boolean
  id?: string
  disabled?: boolean
}
withDefaults(defineProps<Props>(), { disabled: false, modelValue: false })

const emit = defineEmits<{ 'update:modelValue': [value: boolean] }>()

function onChange(e: Event) {
  emit('update:modelValue', (e.target as HTMLInputElement).checked)
}
</script>

<template>
  <label class="switch" :class="{ 'switch--on': modelValue }" :for="id">
    <input
      class="switch__input"
      type="checkbox"
      role="switch"
      :id="id"
      :checked="modelValue"
      :disabled="disabled"
      @change="onChange"
    />
    <span class="switch__track" aria-hidden="true">
      <span class="switch__thumb" />
    </span>
    <span class="switch__body">
      <span class="switch__label">{{ label }}</span>
      <span v-if="description" class="switch__desc">{{ description }}</span>
    </span>
  </label>
</template>

<style scoped>
.switch {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
  cursor: pointer;
  padding: var(--space-2) 0;
}

/* 真实 checkbox 隐藏但仍可聚焦；聚焦环投到轨道上 */
.switch__input {
  position: absolute;
  width: 1px;
  height: 1px;
  opacity: 0;
}

.switch__track {
  flex: none;
  width: 40px;
  height: 22px;
  border-radius: var(--radius-pill);
  background: var(--surface-3);
  border: 1px solid var(--border-strong);
  position: relative;
  margin-top: 2px;
  transition: background var(--dur-fast) var(--ease-out),
    border-color var(--dur-fast) var(--ease-out);
}

.switch__thumb {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 16px;
  height: 16px;
  border-radius: var(--radius-pill);
  background: var(--muted);
  transition: transform var(--dur-fast) var(--ease-spring),
    background var(--dur-fast) var(--ease-out);
}

.switch--on .switch__track {
  background: var(--accent);
  border-color: var(--accent);
}

.switch--on .switch__thumb {
  transform: translateX(18px);
  background: #fff;
}

.switch__input:focus-visible + .switch__track {
  outline: none;
  box-shadow: 0 0 0 2px var(--accent-soft);
}

.switch--on .switch__input:focus-visible + .switch__track {
  box-shadow: 0 0 0 2px var(--accent-soft);
}

.switch__body {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: var(--fs-13);
  color: var(--text);
}

.switch__label {
  line-height: var(--lh-base);
}

.switch__desc {
  font-size: var(--fs-11);
  color: var(--muted-2);
  line-height: var(--lh-base);
}

@media (prefers-reduced-motion: reduce) {
  .switch__track,
  .switch__thumb {
    transition: none;
  }
}
</style>
