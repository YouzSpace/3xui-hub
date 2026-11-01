<script setup>
/**
 * 列表分页控件（纯展示层分页，每页 50 条）：上一页 / 下一页 + 当前第 N 页 · 共 M 条。
 * 信息布局沿用 DiscountCodes 的分页条，样式走现有 Button 组件与 CSS token，不引 UI 库。
 */
import Button from '@/components/ui/Button.vue'

defineProps({
  page: { type: Number, required: true },
  lastPage: { type: Number, default: 1 },
  total: { type: Number, default: 0 },
  loading: { type: Boolean, default: false },
})

defineEmits(['prev', 'next'])
</script>

<template>
  <div class="ch-pagination">
    <span class="ch-pagination-info">共 {{ total }} 条 · 第 {{ page }} / {{ Math.max(1, lastPage) }} 页</span>
    <div class="ch-pagination-btns">
      <Button variant="secondary" size="sm" :disabled="loading || page <= 1" @click="$emit('prev')">上一页</Button>
      <Button variant="secondary" size="sm" :disabled="loading || page >= Math.max(1, lastPage)" @click="$emit('next')">下一页</Button>
    </div>
  </div>
</template>

<style scoped>
.ch-pagination {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  padding: var(--space-4) var(--space-6);
  border-top: 1px solid var(--border-subtle);
}
.ch-pagination-info {
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.ch-pagination-btns {
  display: flex;
  gap: var(--space-2);
}
</style>
