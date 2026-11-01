<script setup>
/**
 * Skeleton.vue — 骨架屏扫光（ui-motion #15）。
 * 列表加载时用「灰底 + 高光斜扫」替代白屏转圈，结构已知所以提前占位。
 * 只动 transform（扫光），不触发重排；prefers-reduced-motion 时降级为静态。
 */
defineProps({
  rows: { type: Number, default: 5 },
  height: { type: String, default: '40px' },
  animate: { type: Boolean, default: true },
})
</script>

<template>
  <div class="skel" :class="{ 'skel--static': !animate }" aria-hidden="true">
    <div v-for="i in rows" :key="i" class="skel-row" :style="{ height }">
      <span class="skel-bar"></span>
    </div>
  </div>
</template>

<style scoped>
.skel {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-4);
}
.skel-row {
  width: 100%;
}
.skel-bar {
  display: block;
  height: 100%;
  border-radius: var(--radius-md);
  background: var(--bg-elevated);
  position: relative;
  overflow: hidden;
}
/* 扫光：一道高光从左扫到右，周期 1.4s，只动 transform */
.skel-bar::after {
  content: '';
  position: absolute;
  inset: 0;
  background: linear-gradient(90deg, transparent 20%, var(--bg-hover) 50%, transparent 80%);
  transform: translateX(-100%);
  animation: skel-shine 1.4s var(--ease-out) infinite;
}
.skel--static .skel-bar::after {
  display: none;
}
@keyframes skel-shine {
  100% {
    transform: translateX(100%);
  }
}
@media (prefers-reduced-motion: reduce) {
  .skel-bar::after {
    animation: none;
  }
}
</style>
