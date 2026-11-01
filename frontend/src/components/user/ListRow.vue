<script setup>
/**
 * ListRow — iOS 列表行（稿中样式）。icon / right 走插槽；点击用 @press 事件。
 */
import UserIcon from '@/components/user/UserIcon.vue'

defineProps({
  label: { type: String, required: true },
  sub: { type: String, default: '' },
  last: { type: Boolean, default: false },
  danger: { type: Boolean, default: false },
  iconBg: { type: String, default: '' },
  iconColor: { type: String, default: '' },
})
const emit = defineEmits(['press'])
</script>

<template>
  <button
    type="button"
    class="lr uu-press"
    :class="{ 'lr--last': last }"
    @click="emit('press')"
  >
    <div
      class="lr-icon"
      :style="{
        background: iconBg || 'var(--fill3)',
        color: iconColor || 'var(--lbl2)',
      }"
    >
      <slot name="icon" />
    </div>
    <div class="lr-main">
      <p class="lr-label" :class="{ 'lr-label--danger': danger }">{{ label }}</p>
      <p v-if="sub" class="lr-sub">{{ sub }}</p>
    </div>
    <span v-if="$slots.right" class="lr-right">
      <slot name="right" />
    </span>
    <span v-else class="lr-chevron">
      <UserIcon name="chevronRight" :size="16" />
    </span>
  </button>
</template>

<style scoped>
.lr {
  display: flex;
  align-items: center;
  gap: 14px;
  width: 100%;
  padding: 12px 16px;
  border: none;
  border-bottom: 1px solid var(--sep);
  background: transparent;
  text-align: left;
  font-family: inherit;
}
.lr--last {
  border-bottom: none;
}
.lr-icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 36px;
  height: 36px;
  border-radius: 12px;
  flex-shrink: 0;
}
.lr-main {
  flex: 1;
  min-width: 0;
}
.lr-label {
  margin: 0;
  font-size: 17px;
  font-weight: 400;
  color: var(--lbl1);
}
.lr-label--danger {
  color: var(--red);
}
.lr-sub {
  margin: 1px 0 0;
  font-size: 13px;
  color: var(--lbl3);
}
.lr-right {
  color: var(--lbl3);
  font-size: 15px;
}
.lr-chevron {
  color: var(--lbl4);
  display: inline-flex;
}
</style>
