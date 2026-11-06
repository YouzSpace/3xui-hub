<script setup>
/**
 * TutorialDetail — 用户端教程详情
 */
import { ref, onMounted, computed } from 'vue'
import { useRoute } from 'vue-router'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { userApi } from '@/api/user'

const route = useRoute()
const tutorial = ref(null)
const loading = ref(true)

const renderedContent = computed(() => {
  if (!tutorial.value?.content) return ''
  return DOMPurify.sanitize(marked.parse(tutorial.value.content))
})

// 返回统一由外壳表头（UserLayout）承担，此处不再重复放按钮

onMounted(async () => {
  try {
    tutorial.value = await userApi.tutorial(route.params.id)
  } catch (e) {
    tutorial.value = null
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <div class="td">
    <div v-if="loading" class="td-loading">加载中...</div>

    <div v-else-if="tutorial" class="td-content">
      <h1 class="td-title">{{ tutorial.title }}</h1>
      <div class="td-meta">{{ new Date(tutorial.created_at).toLocaleDateString() }}</div>
      <div class="td-body" v-html="renderedContent"></div>
    </div>

    <div v-else class="td-empty">
      <p>教程不存在或已下架</p>
    </div>
  </div>
</template>

<style scoped>
.td { padding-top: 8px; }

/* 手机阅读页贴边（用户 2026-11-06 定）：外层 ul-scroll 已把左右边距归零，
   这里卡片改为直角贴边、去外框阴影；桌面（≥768px）恢复圆角卡片+内边距 */
.td-content {
  background: var(--bg1, #fff);
  border-radius: 0;
  padding: 16px;
}
@media (min-width: 768px) {
  .td-content {
    border: 1px solid rgba(0,0,0,0.04);
    border-radius: 16px;
    padding: 12px;
    box-shadow: 0 2px 12px -2px rgba(0,0,0,0.06);
  }
}
.td-title { margin: 0 0 8px; font-size: 24px; font-weight: 800; color: #1d1d1f; letter-spacing: -0.02em; }
.td-meta { font-size: 13px; color: #999; margin-bottom: 24px; }

.td-body { font-size: 15px; line-height: 1.8; color: #333; }
.td-body :deep(h1) { font-size: 24px; font-weight: 800; margin: 32px 0 16px; color: #1d1d1f; }
.td-body :deep(h2) { font-size: 20px; font-weight: 700; margin: 28px 0 12px; color: #1d1d1f; }
.td-body :deep(h3) { font-size: 17px; font-weight: 700; margin: 24px 0 10px; color: #1d1d1f; }
.td-body :deep(p) { margin: 0 0 16px; }
.td-body :deep(a) { color: #2563eb; text-decoration: none; }
.td-body :deep(a:hover) { text-decoration: underline; }
.td-body :deep(code) {
  background: #f1f5f9; padding: 2px 6px; border-radius: 4px;
  font-size: 13px; font-family: 'SF Mono', 'Fira Code', monospace;
}
.td-body :deep(pre) {
  background: #1e293b; color: #e2e8f0; padding: 16px; border-radius: 10px;
  overflow-x: auto; margin: 0 0 16px;
}
.td-body :deep(pre code) { background: none; padding: 0; color: inherit; }
.td-body :deep(ul), .td-body :deep(ol) { padding-left: 24px; margin: 0 0 16px; }
.td-body :deep(li) { margin-bottom: 6px; }
.td-body :deep(blockquote) {
  border-left: 3px solid #2563eb; padding: 12px 16px; margin: 0 0 16px;
  background: #f8fafc; border-radius: 0 8px 8px 0; color: #666;
}
.td-body :deep(img) { max-width: 100%; border-radius: 8px; margin: 8px 0; }
.td-body :deep(table) { width: 100%; border-collapse: collapse; margin: 0 0 16px; }
.td-body :deep(th), .td-body :deep(td) { padding: 10px 14px; border: 1px solid #e2e8f0; text-align: left; font-size: 14px; }
.td-body :deep(th) { background: #f8fafc; font-weight: 600; }

.td-loading, .td-empty { text-align: center; color: #999; font-size: 14px; padding: 60px 0; }

/* 暗色模式 */
html.dark .td-content { background: rgba(20,20,20,0.8); border-color: rgba(255,255,255,0.06); }
html.dark .td-title { color: #fff; }
html.dark .td-body { color: #d1d5db; }
html.dark .td-body :deep(h1), html.dark .td-body :deep(h2), html.dark .td-body :deep(h3) { color: #fff; }
html.dark .td-body :deep(a) { color: #60a5fa; }
html.dark .td-body :deep(code) { background: rgba(255,255,255,0.06); }
html.dark .td-body :deep(pre) { background: #0f172a; }
html.dark .td-body :deep(blockquote) { background: rgba(255,255,255,0.04); border-color: #60a5fa; color: #999; }
html.dark .td-body :deep(th), html.dark .td-body :deep(td) { border-color: rgba(255,255,255,0.08); }
html.dark .td-body :deep(th) { background: rgba(255,255,255,0.04); }
</style>
