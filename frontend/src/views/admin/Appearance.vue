<script setup>
/**
 * Admin · 主页与配色（容器页）。
 *
 * 主页自定义和面板配色本质是同一件事：前端长什么样。原来侧栏「自定义」组里
 * 摆两个平级入口，现在收成一个，进去用 Tab 切换。
 *
 * 两个子页面原样复用，切换时重建（各自有本地状态与预览）。
 */
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import HomeCustom from './HomeCustom.vue'
import PanelColors from './PanelColors.vue'

const route = useRoute()
const router = useRouter()

const tabs = [
  { key: 'home', label: '主页', comp: HomeCustom },
  { key: 'colors', label: '配色', comp: PanelColors },
]

const active = computed(() => {
  const key = String(route.query.tab || 'home')
  return tabs.some((t) => t.key === key) ? key : 'home'
})

const current = computed(() => tabs.find((t) => t.key === active.value)?.comp)

const switchTab = (key) => {
  if (key === active.value) return
  router.replace({ path: '/admin/appearance', query: { ...route.query, tab: key } }).catch(() => {})
}
</script>

<template>
  <div class="ap">
    <div class="ap-tabs" role="tablist">
      <button
        v-for="t in tabs"
        :key="t.key"
        class="ap-tab"
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
.ap-tabs {
  display: flex;
  gap: 4px;
  margin-bottom: var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
}
.ap-tab {
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
.ap-tab:hover {
  color: var(--text-primary);
}
.ap-tab.is-active {
  color: var(--text-primary);
  border-bottom-color: var(--accent);
  font-weight: var(--font-medium);
}
</style>
