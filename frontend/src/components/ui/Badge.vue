<script setup>
/**
 * Badge.vue — pill status badge.
 * variants: success | warning | danger | info | vless | trojan
 * Each maps to a `--*-muted` background + `--*` text color.
 */
import { computed } from 'vue'

const props = defineProps({
  variant: {
    type: String,
    default: 'info',
    validator: (v) => ['success', 'warning', 'danger', 'info', 'vless', 'trojan'].includes(v),
  },
  dot: { type: Boolean, default: false },
})

const variantClass = computed(() => `ch-badge--${props.variant}`)
</script>

<template>
  <span class="ch-badge" :class="variantClass">
    <span v-if="dot" class="ch-badge-dot" aria-hidden="true"></span>
    <slot />
  </span>
</template>

<style scoped>
.ch-badge {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  padding: 2px 10px;
  border: 1px solid transparent;
  border-radius: var(--radius-md);
  font-size: var(--text-xs);
  font-weight: var(--font-semibold);
  line-height: 1.5;
  white-space: nowrap;
}
.ch-badge-dot {
  width: 6px;
  height: 6px;
  border-radius: var(--radius-full);
  background: currentColor;
}
.ch-badge--success {
  background: var(--success-muted);
  color: var(--success);
  border-color: var(--success);
}
.ch-badge--warning {
  background: var(--warning-muted);
  color: var(--warning);
  border-color: var(--warning);
}
.ch-badge--danger {
  background: var(--danger-muted);
  color: var(--danger);
  border-color: var(--danger);
}
.ch-badge--info {
  background: var(--info-muted);
  color: var(--info);
  border-color: var(--info);
}
.ch-badge--vless {
  background: var(--proto-vless-muted);
  color: var(--proto-vless);
}
.ch-badge--trojan {
  background: var(--proto-trojan-muted);
  color: var(--proto-trojan);
}
</style>
