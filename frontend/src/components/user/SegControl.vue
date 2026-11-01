<script setup>
/**
 * SegControl — iOS 分段控件（稿中样式，滑块 uu-seg-pill 动画）。
 * options: [{ label, value }]，v-model 绑定选中 value。
 */
import { computed } from 'vue'

const props = defineProps({
  options: { type: Array, required: true },
  modelValue: { type: [String, Number], default: '' },
})
const emit = defineEmits(['update:modelValue'])

const idx = computed(() => props.options.findIndex((o) => o.value === props.modelValue))
const pct = computed(() => (props.options.length ? 100 / props.options.length : 100))
const pillStyle = computed(() => ({
  left: `calc(${pct.value * idx.value}% + 2px)`,
  width: `calc(${pct.value}% - 4px)`,
}))
</script>

<template>
  <div class="uu-seg">
    <div class="uu-seg-pill uu-seg-track" :style="pillStyle"></div>
    <button
      v-for="opt in options"
      :key="opt.value"
      type="button"
      class="uu-seg-btn uu-press"
      :class="{ 'uu-seg-btn--active': opt.value === modelValue }"
      @click="emit('update:modelValue', opt.value)"
    >
      {{ opt.label }}
    </button>
  </div>
</template>

<style scoped>
.uu-seg {
  position: relative;
  display: flex;
  padding: 2px;
  border-radius: 12px;
  background: var(--fill3);
  border: 1px solid var(--sep);
}
.uu-seg-track {
  position: absolute;
  top: 2px;
  bottom: 2px;
  border-radius: 10px;
  background: var(--mat-regular);
  border: 1px solid var(--border-glass);
}
.uu-seg-btn {
  position: relative;
  z-index: 1;
  flex: 1;
  padding: 6px 10px;
  border: none;
  border-radius: 8px;
  background: transparent;
  font-size: 13px;
  font-weight: 400;
  font-family: inherit;
  color: var(--lbl3);
  text-align: center;
  white-space: nowrap;
  transition: color 0.2s;
}
.uu-seg-btn--active {
  font-weight: 600;
  color: var(--lbl1);
}
</style>
