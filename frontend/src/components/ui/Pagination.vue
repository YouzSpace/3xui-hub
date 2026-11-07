<script setup>
/**
 * 列表分页控件（纯展示层分页，每页 50 条）：上一页 / 下一页 + 当前第 N 页 · 共 M 条。
 * 信息布局沿用 DiscountCodes 的分页条，样式走现有 Button 组件与 CSS token，不引 UI 库。
 *
 * 游标分页（deep-page 优化）：后端游标模式返回 total=-1、last_page=0（深页不查 COUNT）。
 * 此时组件拿不到总页数，靠 hasNext（= 还有 next_cursor）决定「下一页」是否可点，
 * 信息文案退化为「第 N 页」不显示总数 —— 这也解释了老版本「共 -1 条 + 第二页翻不动」：
 * total=-1 被原样渲染、lastPage=0 把 Math.max(1,0)=1 与当前页比较恒为真，下一页被禁。
 */
import Button from '@/components/ui/Button.vue'

defineProps({
  page: { type: Number, required: true },
  lastPage: { type: Number, default: 1 },
  total: { type: Number, default: 0 },
  loading: { type: Boolean, default: false },
  // 游标模式：true = 还有下一页（后端给了 next_cursor）。total=-1 时以此为准。
  hasNext: { type: Boolean, default: false },
})

defineEmits(['prev', 'next'])
</script>

<template>
  <div class="ch-pagination">
    <span class="ch-pagination-info">
      <template v-if="total < 0">第 {{ page }} 页</template>
      <template v-else>共 {{ total }} 条 · 第 {{ page }} / {{ Math.max(1, lastPage) }} 页</template>
    </span>
    <div class="ch-pagination-btns">
      <Button variant="secondary" size="sm" :disabled="loading || page <= 1" @click="$emit('prev')">上一页</Button>
      <!-- lastPage=0（游标模式）时以 hasNext 为准；有总页数时按页码比较 -->
      <Button
        variant="secondary"
        size="sm"
        :disabled="loading || (total >= 0 ? page >= Math.max(1, lastPage) : !hasNext)"
        @click="$emit('next')"
      >下一页</Button>
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
