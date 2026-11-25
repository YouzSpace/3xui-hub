<script setup>
/**
 * ListGroup — iOS 列表分组（稿中样式）：分组标题 + 卡片容器 + 脚注。
 * 标题行右侧可选插槽 #label-extra（放小字说明/操作，如「仅显示最近 30 天」）。
 */
defineProps({
  label: { type: String, default: '' },
  footnote: { type: String, default: '' },
})
</script>

<template>
  <div class="lg">
    <div v-if="label || $slots['label-extra']" class="lg-head">
      <p v-if="label" class="lg-label">{{ label }}</p>
      <div v-if="$slots['label-extra']" class="lg-label-extra">
        <slot name="label-extra" />
      </div>
    </div>
    <div class="lg-card uu-mat-regular">
      <slot />
    </div>
    <p v-if="footnote" class="lg-footnote">{{ footnote }}</p>
  </div>
</template>

<style scoped src="@/assets/styles/components/user/ListGroup.css"></style>
