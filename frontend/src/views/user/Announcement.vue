<script setup>
/**
 * Announcement — 系统公告全屏页。
 * 内容来自 siteConfig.config.announcement，沿用既有的纯文本 + pre-wrap 渲染方式。
 */
import { computed } from 'vue'
import { useSiteConfigStore } from '@/stores/siteConfig'

const siteConfig = useSiteConfigStore()
const content = computed(() => (siteConfig.config.announcement || '').trim())
</script>

<template>
  <div class="ann uu-slide-up">
    <!-- 内容卡片 -->
    <section class="ann-card uu-mat-regular">
      <div class="ann-box">
        <p v-if="content" class="ann-content">{{ content }}</p>
        <p v-else class="ann-empty">暂无公告</p>
      </div>
    </section>
  </div>
</template>

<style scoped>
.ann {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

/* ===== 内容卡片 ===== */
.ann-card {
  border-radius: 24px;
  padding: 16px;
}
.ann-box {
  padding: 16px;
  border-radius: 16px;
  background: var(--fill3);
  border: 1px solid var(--sep);
}
.ann-content {
  margin: 0;
  font-size: 15px;
  line-height: 1.7;
  color: var(--lbl2);
  white-space: pre-wrap;
  word-break: break-word;
}
.ann-empty {
  margin: 0;
  font-size: 15px;
  color: var(--lbl3);
}
</style>
