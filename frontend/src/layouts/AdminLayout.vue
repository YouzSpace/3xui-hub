<script setup>
/**
 * AdminLayout — 侧边栏 240px + 主区，移动端汉堡折叠滑出。
 * Toast provide 在此（design M12.2）。
 */
import { ref, provide, onMounted, onUnmounted } from 'vue'
import { RouterLink, useRouter, useRoute } from 'vue-router'
import Icon from '@/components/ui/Icon.vue'
import Button from '@/components/ui/Button.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useToast } from '@/components/ui/Toast.vue'
import { useAdminStore } from '@/stores/admin'
import { useSiteConfigStore } from '@/stores/siteConfig'

provide('toast', useToast())

const admin = useAdminStore()
const siteConfig = useSiteConfigStore()
const router = useRouter()
const route = useRoute()
const open = ref(false)
const settingsOpen = ref(false)

const navItems = [
  { to: '/admin', name: 'admin-dashboard', label: '仪表盘', icon: 'dashboard' },
  { to: '/admin/users', name: 'admin-users', label: '用户', icon: 'users' },
  { to: '/admin/nodes', name: 'admin-nodes', label: '节点', icon: 'server' },
  { to: '/admin/plans', name: 'admin-plans', label: '套餐', icon: 'package' },
  { to: '/admin/orders', name: 'admin-orders', label: '订单', icon: 'order' },
]

const settingsItems = [
  { to: '/admin/settings', name: 'admin-settings', label: '安全', icon: 'shield' },
  { to: '/admin/payments', name: 'admin-payments', label: '支付', icon: 'payment' },
  { to: '/admin/backup', name: 'admin-backup', label: '备份', icon: 'order' },
  { to: '/admin/site-settings', name: 'admin-site-settings', label: '网站信息', icon: 'edit' },
]

const isActiveSettings = () => {
  return settingsItems.some(item => route.path === item.to)
}

// 初始化时如果当前在设置页面，自动展开
if (isActiveSettings()) {
  settingsOpen.value = true
}

const onLogout = async () => {
  await admin.logout()
  router.push({ name: 'admin-login' })
}
</script>

<template>
  <div class="al-wrap">
    <!-- 移动端遮罩 -->
    <div v-if="open" class="al-overlay" @click="open = false"></div>

    <aside class="al-sidebar" :class="{ 'al-sidebar--open': open }">
      <div class="al-brand">
        <span class="al-brand-logo">
          <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="logo" class="al-brand-logo-img" />
          <Icon v-else name="shield" :size="20" />
        </span>
        <span class="al-brand-name">{{ siteConfig.title }}</span>
        <ThemeToggle class="al-brand-toggle" />
      </div>

      <nav class="al-nav">
        <!-- 普通菜单项 -->
        <RouterLink
          v-for="item in navItems"
          :key="item.name"
          :to="item.to"
          class="al-nav-item"
          @click="open = false"
        >
          <Icon :name="item.icon" :size="18" />
          <span>{{ item.label }}</span>
        </RouterLink>

        <!-- 设置（可展开） -->
        <div class="al-nav-group">
          <button
            class="al-nav-item al-nav-item-btn"
            :class="{ 'router-link-exact-active': isActiveSettings() }"
            @click="settingsOpen = !settingsOpen"
          >
            <Icon name="settings" :size="18" />
            <span>设置</span>
            <Icon :name="settingsOpen ? 'chevron-up' : 'chevron-down'" :size="14" class="al-nav-arrow" />
          </button>
          <div v-show="settingsOpen" class="al-nav-sub">
            <RouterLink
              v-for="item in settingsItems"
              :key="item.name"
              :to="item.to"
              class="al-nav-sub-item"
              @click="open = false"
            >
              <Icon :name="item.icon" :size="16" />
              <span>{{ item.label }}</span>
            </RouterLink>
          </div>
        </div>
      </nav>

      <div class="al-sidebar-foot">
        <Button variant="ghost" size="sm" @click="onLogout">
          <Icon name="logout" :size="16" /> 退出
        </Button>
      </div>
    </aside>

    <div class="al-main">
      <header class="al-header">
        <button class="al-menu-btn" type="button" aria-label="菜单" @click="open = !open">
          <Icon name="menu" :size="22" />
        </button>
        <span class="al-header-title">管理控制台</span>
        <span class="al-header-spacer"></span>
        <ThemeToggle />
      </header>
      <main class="al-content">
        <RouterView />
      </main>
    </div>
  </div>
</template>

<style scoped>
.al-wrap {
  display: flex;
  min-height: 100vh;
}
.al-sidebar {
  width: 240px;
  flex-shrink: 0;
  background: var(--bg-surface);
  border-right: 1px solid var(--border-subtle);
  display: flex;
  flex-direction: column;
  position: sticky;
  top: 0;
  height: 100vh;
}
.al-brand {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-4) var(--space-5);
  border-bottom: 1px solid var(--border-subtle);
}
.al-brand-logo {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  border-radius: var(--radius-md);
  background: var(--accent-muted);
  color: var(--accent);
}
.al-brand-logo-img {
  width: 32px;
  height: 32px;
  object-fit: contain;
  border-radius: var(--radius-md);
}
.al-brand-name {
  font-weight: var(--font-semibold);
}
.al-brand-toggle {
  margin-left: auto;
}
.al-nav {
  flex: 1;
  padding: var(--space-3);
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  overflow-y: auto;
}
.al-nav-item {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-3);
  border-radius: var(--radius-md);
  color: var(--text-secondary);
  text-decoration: none;
  transition: background var(--duration-fast) var(--ease-out), color var(--duration-fast) var(--ease-out);
}
.al-nav-item:hover {
  background: var(--bg-hover);
  color: var(--text-primary);
}
.al-nav-item.router-link-exact-active {
  background: var(--accent-muted);
  color: var(--accent);
  font-weight: var(--font-medium);
}
.al-nav-item-btn {
  width: 100%;
  border: none;
  background: transparent;
  cursor: pointer;
  font-size: inherit;
}
.al-nav-arrow {
  margin-left: auto;
}
.al-nav-sub {
  padding-left: var(--space-2);
}
.al-nav-sub-item {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-md);
  color: var(--text-secondary);
  text-decoration: none;
  font-size: var(--text-sm);
  transition: background var(--duration-fast) var(--ease-out), color var(--duration-fast) var(--ease-out);
}
.al-nav-sub-item:hover {
  background: var(--bg-hover);
  color: var(--text-primary);
}
.al-nav-sub-item.router-link-exact-active {
  background: var(--accent-muted);
  color: var(--accent);
  font-weight: var(--font-medium);
}
.al-sidebar-foot {
  padding: var(--space-3);
  border-top: 1px solid var(--border-subtle);
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.al-main {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.al-header {
  display: none;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
}
.al-menu-btn {
  background: transparent;
  border: none;
  color: var(--text-primary);
  cursor: pointer;
  display: inline-flex;
}
.al-header-title {
  font-weight: var(--font-semibold);
}
.al-header-spacer {
  flex: 1;
}
.al-content {
  padding: var(--space-6);
  overflow-x: auto;
}
.al-overlay {
  display: none;
}

@media (max-width: 768px) {
  .al-sidebar {
    position: fixed;
    left: 0;
    top: 0;
    z-index: 900;
    transform: translateX(-100%);
    transition: transform var(--duration-base) var(--ease-out);
  }
  .al-sidebar--open {
    transform: translateX(0);
  }
  .al-header {
    display: flex;
  }
  .al-overlay {
    display: block;
    position: fixed;
    inset: 0;
    background: var(--overlay);
    z-index: 800;
  }
  .al-content {
    padding: var(--space-4);
  }
}
</style>
