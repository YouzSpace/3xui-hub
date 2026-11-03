<script setup>
import { ref } from 'vue'
import { useRoute } from 'vue-router'
import InviteSettings from './InviteSettings.vue'
import AffiliateRebates from './AffiliateRebates.vue'

// 「邀请机制」统一入口：邀请设置（折扣 / 刷新周期 / 开关 / 校验限流）与
// 返佣记录（只读流水）一个页面两个 tab —— 与「码管理」（Codes.vue）同一套结构。
// 支持 ?tab=rebates 直接落到返佣记录。
const route = useRoute()
const tab = ref(route.query.tab === 'rebates' ? 'rebates' : 'settings')
</script>

<template>
  <div class="iv">
    <div class="iv-tabs" role="tablist">
      <button
        class="iv-tab"
        :class="{ 'is-active': tab === 'settings' }"
        role="tab"
        :aria-selected="tab === 'settings'"
        @click="tab = 'settings'"
      >
        邀请设置
      </button>
      <button
        class="iv-tab"
        :class="{ 'is-active': tab === 'rebates' }"
        role="tab"
        :aria-selected="tab === 'rebates'"
        @click="tab = 'rebates'"
      >
        返佣记录
      </button>
    </div>
    <InviteSettings v-if="tab === 'settings'" />
    <AffiliateRebates v-else />
  </div>
</template>

<style scoped>
.iv-tabs {
  display: flex;
  gap: 4px;
  margin-bottom: var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
}
.iv-tab {
  padding: 8px 16px;
  border: none;
  background: transparent;
  cursor: pointer;
  font-family: inherit;
  font-size: var(--text-sm);
  color: var(--text-muted);
  border-bottom: 2px solid transparent;
  margin-bottom: -1px;
  transition:
    color var(--duration-fast) var(--ease-out),
    border-color var(--duration-fast) var(--ease-out);
}
.iv-tab:hover {
  color: var(--text-primary);
}
.iv-tab.is-active {
  color: var(--text-primary);
  border-bottom-color: var(--accent);
  font-weight: var(--font-medium);
}
</style>
