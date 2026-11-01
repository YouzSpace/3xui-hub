<script setup>
/**
 * Admin · Xray 高级配置（容器页）。
 *
 * 把原来侧栏里四个平级菜单（入站 / 出站 / 路由 / WARP）收成一个入口，
 * 进去用顶部 Tab 切换 —— 侧栏不再被技术配置项占满。
 *
 * 四个子页面原样复用（各自带节点选择器与保存逻辑），这里只做切换，
 * 不做状态共享：切 Tab 重建组件，避免上一个 Tab 的选中节点串到下一个。
 * 当前 Tab 记在 URL（?tab=），刷新和分享都停在同一个页签。
 */
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import XrayInbounds from './XrayInbounds.vue'
import XrayOutbounds from './XrayOutbounds.vue'
import XrayRouting from './XrayRouting.vue'
import XrayWarp from './XrayWarp.vue'

const route = useRoute()
const router = useRouter()

const tabs = [
  { key: 'inbounds', label: '入站', comp: XrayInbounds },
  { key: 'outbounds', label: '出站', comp: XrayOutbounds },
  { key: 'routing', label: '路由', comp: XrayRouting },
  { key: 'warp', label: 'WARP', comp: XrayWarp },
]

const active = computed(() => {
  const key = String(route.query.tab || 'inbounds')
  return tabs.some((t) => t.key === key) ? key : 'inbounds'
})

const current = computed(() => tabs.find((t) => t.key === active.value)?.comp)

const switchTab = (key) => {
  if (key === active.value) return
  // 保留其它 query（如 relay=1、node=xx）：子页面会读它们决定初始状态
  router.replace({ path: '/admin/nodes/xray', query: { ...route.query, tab: key } }).catch(() => {})
}
</script>

<template>
  <div class="xa">
    <div class="xa-tabs" role="tablist">
      <button
        v-for="t in tabs"
        :key="t.key"
        class="xa-tab"
        :class="{ 'is-active': active === t.key }"
        type="button"
        role="tab"
        :aria-selected="active === t.key"
        @click="switchTab(t.key)"
      >
        {{ t.label }}
      </button>
    </div>

    <component :is="current" :key="active" />
  </div>
</template>

<style scoped>
.xa-tabs {
  display: flex;
  gap: 4px;
  margin-bottom: var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
}
.xa-tab {
  padding: 8px 16px;
  border: none;
  background: transparent;
  cursor: pointer;
  font-family: inherit;
  font-size: var(--text-sm);
  color: var(--text-muted);
  border-bottom: 2px solid transparent;
  margin-bottom: -1px;
  transition: color var(--duration-fast) var(--ease-out),
    border-color var(--duration-fast) var(--ease-out);
}
.xa-tab:hover {
  color: var(--text-primary);
}
.xa-tab.is-active {
  color: var(--text-primary);
  border-bottom-color: var(--accent);
  font-weight: var(--font-medium);
}
</style>
