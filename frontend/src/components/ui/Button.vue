<script setup>
/**
 * Button.vue
 * props: variant (primary|secondary|danger|ghost) / size (sm|md|lg) / loading / disabled
 * 尺寸对齐参考项目 shadcn Button：h-9 / sm h-8 / lg h-10，圆角 --radius-md，
 * 字重 medium，focus 走 1px ring 而非发光。颜色全部走 CSS 变量，无硬编码色值。
 */
import Icon from '@/components/ui/Icon.vue'

const props = defineProps({
  variant: { type: String, default: 'primary', validator: (v) => ['primary', 'secondary', 'danger', 'ghost'].includes(v) },
  size: { type: String, default: 'md', validator: (v) => ['sm', 'md', 'lg'].includes(v) },
  loading: { type: Boolean, default: false },
  success: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false },
  type: { type: String, default: 'button' },
})

const sizeClass = {
  sm: 'btn-sm',
  md: 'btn-md',
  lg: 'btn-lg',
}[props.size]
</script>

<template>
  <button
    :type="type"
    class="ch-btn"
    :class="[`btn-${variant}`, sizeClass, { 'is-loading': loading, 'is-success': success, 'is-disabled': disabled }]"
    :disabled="disabled || loading || success"
  >
    <span v-if="loading" class="ch-btn-spinner" aria-hidden="true"></span>
    <Icon v-else-if="success" name="check" :size="16" class="ch-btn-check" aria-hidden="true" />
    <slot />
  </button>
</template>

<style scoped>
.ch-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: var(--space-2);
  font-family: var(--font-sans);
  font-weight: var(--font-medium);
  border: none;
  border-radius: var(--radius-md);
  cursor: pointer;
  white-space: nowrap;
  transition: background-color var(--duration-fast) var(--ease-out),
    border-color var(--duration-fast) var(--ease-out), color var(--duration-fast) var(--ease-out),
    box-shadow var(--duration-fast) var(--ease-out);
}
.ch-btn:focus-visible {
  outline: none;
  box-shadow: var(--glow-accent);
}
.ch-btn :deep(svg) {
  flex-shrink: 0;
}
.btn-md {
  height: 36px;
  padding: 0 var(--space-4);
  font-size: var(--text-sm);
}
.btn-sm {
  height: 32px;
  padding: 0 var(--space-3);
  font-size: var(--text-xs);
}
.btn-lg {
  height: 40px;
  padding: 0 var(--space-8);
  font-size: var(--text-sm);
}

/* primary — 近黑底白字，hover 90% 不透明 */
.btn-primary {
  background: var(--accent);
  color: var(--text-inverse);
  box-shadow: var(--shadow-sm);
}
.btn-primary:hover:not(.is-disabled):not(.is-loading) {
  background: var(--accent-hover);
}
.btn-primary:active:not(.is-disabled):not(.is-loading) {
  background: var(--accent-active);
}

/* secondary — 靠底色（浅灰底），无边框，hover 加深 */
.btn-secondary {
  background: var(--bg-elevated);
  border: none;
  color: var(--text-primary);
}
.btn-secondary:hover:not(.is-disabled):not(.is-loading) {
  filter: brightness(0.96);
}

/* danger */
.btn-danger {
  background: var(--danger);
  color: var(--text-inverse);
  box-shadow: var(--shadow-sm);
}
.btn-danger:hover:not(.is-disabled):not(.is-loading) {
  filter: brightness(0.95);
}

/* ghost */
.btn-ghost {
  background: transparent;
  color: var(--text-secondary);
}
.btn-ghost:hover:not(.is-disabled):not(.is-loading) {
  background: var(--bg-hover);
  color: var(--text-primary);
}

.is-disabled,
.is-loading {
  opacity: 0.5;
  cursor: not-allowed;
}

.ch-btn-spinner {
  width: 14px;
  height: 14px;
  border-radius: var(--radius-full);
  border: 2px solid currentColor;
  border-right-color: transparent;
  animation: ch-btn-spin 0.6s linear infinite;
}
/* 成功态：对勾描边画出（Submit Morph 的成功段），绿色 + 轻微弹入 */
.ch-btn-check {
  color: currentColor;
  animation: ch-btn-check-pop 0.35s var(--ease-spring);
}
.ch-btn.is-success {
  background: var(--success);
  color: var(--text-inverse);
}
@keyframes ch-btn-check-pop {
  0% { transform: scale(0.4); opacity: 0; }
  100% { transform: scale(1); opacity: 1; }
}
@keyframes ch-btn-spin {
  to {
    transform: rotate(360deg);
  }
}
</style>