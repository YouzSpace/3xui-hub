<script setup>
/**
 * ListGroup — iOS 列表分组（稿中样式）：分组标题 + 卡片容器 + 脚注。
 */
defineProps({
  label: { type: String, default: '' },
  footnote: { type: String, default: '' },
})
</script>

<template>
  <div class="lg">
    <p v-if="label" class="lg-label">{{ label }}</p>
    <div class="lg-card uu-mat-regular">
      <slot />
    </div>
    <p v-if="footnote" class="lg-footnote">{{ footnote }}</p>
  </div>
</template>

<style scoped>
.lg {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.lg-label {
  margin: 0;
  padding: 0 16px;
  font-size: 13px;
  font-weight: 500;
  color: var(--lbl3);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}
.lg-card {
  border-radius: 16px;
  overflow: hidden;
}
.lg-footnote {
  margin: 0;
  padding: 0 16px;
  font-size: 12px;
  line-height: 1.5;
  color: var(--lbl3);
}
</style>
