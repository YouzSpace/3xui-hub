<script setup>
/**
 * Input.vue
 * 尺寸对齐参考项目 shadcn Input：h-9 px-3 py-1 / sm h-8，圆角 --radius-md，
 * 边框用 --border-subtle，focus 走 1px ring（不用发光描边）。
 * mono prop forces JetBrains Mono
 */
import { computed } from 'vue'
import Icon from '@/components/ui/Icon.vue'

const props = defineProps({
  modelValue: { type: [String, Number], default: '' },
  type: { type: String, default: 'text' },
  placeholder: { type: String, default: '' },
  mono: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false },
  // readonly：可选中复制但不可改（如节点密钥这类只供拷贝的值）。
  // 与 disabled 的区别：disabled 会变半透明、且无法选中文本。
  readonly: { type: Boolean, default: false },
  size: { type: String, default: 'md', validator: (v) => ['sm', 'md'].includes(v) },
  valid: { type: Boolean, default: false },
  invalid: { type: Boolean, default: false },
})

const emit = defineEmits(['update:modelValue'])

const inputClass = computed(() => ({
  'ch-input': true,
  'ch-input--mono': props.mono,
  'ch-input--disabled': props.disabled,
  [`ch-input--${props.size}`]: true,
}))

const onInput = (e) => emit('update:modelValue', e.target.value)
</script>

<template>
  <div class="ch-input-wrap" :class="{ 'ch-input-wrap--valid': valid, 'ch-input-wrap--invalid': invalid }">
    <input
      :type="type"
      :class="inputClass"
      :value="modelValue"
      :placeholder="placeholder"
      :disabled="disabled"
      :readonly="readonly"
      @input="onInput"
    />
    <Icon v-if="valid" name="check" :size="16" class="ch-input-check" aria-hidden="true" />
  </div>
</template>

<style scoped>
.ch-input {
  display: block;
  width: 100%;
  min-width: 0;
  background: var(--bg-input);
  border: 1px solid transparent;
  border-radius: var(--radius-md);
  color: var(--text-primary);
  font-family: var(--font-sans);
  font-size: var(--text-sm);
  outline: none;
  transition: border-color var(--duration-fast) var(--ease-out),
    background-color var(--duration-fast) var(--ease-out),
    box-shadow var(--duration-fast) var(--ease-out);
}
.ch-input::placeholder {
  color: var(--text-muted);
}
.ch-input--md {
  height: 36px;
  padding: 0 var(--space-3);
}
.ch-input--sm {
  height: 32px;
  padding: 0 var(--space-2);
  font-size: var(--text-xs);
}
.ch-input--mono {
  font-family: var(--font-mono);
}
.ch-input:focus {
  border-color: var(--accent);
  background: var(--bg-base);
  box-shadow: 0 0 0 2px var(--accent-muted);
}
.ch-input--disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
/* 校验态：valid 打勾（绿色），invalid 红边 + 摇头 */
.ch-input-wrap {
  position: relative;
  display: block;
  width: 100%;
}
.ch-input-check {
  position: absolute;
  right: 10px;
  top: 50%;
  transform: translateY(-50%);
  color: var(--success);
  pointer-events: none;
  animation: ch-input-check-in 0.25s var(--ease-spring);
}
.ch-input-wrap--valid .ch-input {
  padding-right: 34px;
}
.ch-input-wrap--invalid .ch-input {
  border-color: var(--danger);
  animation: ch-input-shake 0.4s var(--ease-out);
}
@keyframes ch-input-check-in {
  0% { transform: translateY(-50%) scale(0.4); opacity: 0; }
  100% { transform: translateY(-50%) scale(1); opacity: 1; }
}
@keyframes ch-input-shake {
  0%, 100% { transform: translateX(0); }
  20% { transform: translateX(-5px); }
  40% { transform: translateX(5px); }
  60% { transform: translateX(-3px); }
  80% { transform: translateX(3px); }
}
</style>