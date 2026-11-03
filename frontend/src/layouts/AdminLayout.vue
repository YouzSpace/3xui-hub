<script setup>
/**
 * AdminLayout — 后台外壳：侧栏（展开 256px / 折叠 64px）+ 顶栏 + 主区。
 * 排版对齐参考项目的 admin layout：
 *   侧栏 sticky h-screen，折叠态 w-16、展开 w-64，宽度过渡 300ms；
 *   品牌区 py-6 px-6（折叠态 px-2），标题 20px semibold + 12px muted 副标题；
 *   导航区 px-3，菜单项 px-3 py-2 / 圆角 8px / 14px，图标 16px（子项 14px）；
 *   分组按钮可折叠，子项缩进 pl-9（36px）；
 *   底部 border-t + 版本行 + 折叠按钮；
 *   顶栏 px-4/8 py-3/4 border-b，主区 px-4 py-4 / md:px-8 md:py-6；
 *   窗口 < 1280px 自动折叠（用户手动切换过则以用户选择为准）。
 * Toast provide 在此（design M12.2）。
 */
import { ref, computed, provide, onMounted, onBeforeUnmount } from 'vue'
import { RouterLink, useRouter, useRoute } from 'vue-router'
import Icon from '@/components/ui/Icon.vue'
import Button from '@/components/ui/Button.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useToast } from '@/components/ui/Toast.vue'
import { useAdminStore } from '@/stores/admin'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { adminApi } from '@/api/admin'
import { applyPanelColors, clearPanelColors } from '@/utils/panelColors'

provide('toast', useToast())

const admin = useAdminStore()
const siteConfig = useSiteConfigStore()
const router = useRouter()
const route = useRoute()

const open = ref(false)
const navSearch = ref('')
const sidebarCollapsed = ref(false)
const sidebarUserToggled = ref(false)
// 移动端（<768px）侧栏是抽屉，必须显示文字；收起态只在桌面生效
const isNarrow = ref(false)

const NAV_EXPANDED_KEY = 'admin_nav_group_expanded'
const SIDEBAR_COLLAPSED_KEY = 'admin_sidebar_collapsed'
// 参考项目在 <1280px 自动折叠侧栏
const SIDEBAR_AUTO_COLLAPSE_WIDTH = 1280

// ===== 菜单信息架构 =====
// 按管理员的业务动线分组，不按代码模块分组：
//   业务管理 = 卖什么、谁买的（用户/套餐/订单/商城/营销）
//   节点管理 = 底层节点与 xray 配置
//   上游货源 = 进货渠道（机场订阅、Webshare 代理）
//   站点运营 = 用户能看到的页面与文案
//   系统设置 = 基建、安全、审计
const navItems = [
  { to: '/admin', name: 'admin-dashboard', label: '仪表盘', icon: 'dashboard' },
]

const groups = [
  {
    id: 'business',
    label: '业务管理',
    icon: 'briefcase',
    items: [
      { to: '/admin/users', name: 'admin-users', label: '用户', icon: 'users' },
      { to: '/admin/plans', name: 'admin-plans', label: '套餐', icon: 'package' },
      { to: '/admin/orders', name: 'admin-orders', label: '订单', icon: 'order' },
      { to: '/admin/codes', name: 'admin-codes', label: '码管理', icon: 'ticket' },
      { to: '/admin/balance', name: 'admin-balance', label: '余额', icon: 'wallet' },
      { to: '/admin/invite-settings', name: 'admin-invite-settings', label: '邀请机制', icon: 'gift' },
    ],
  },
  {
    id: 'nodes',
    label: '节点管理',
    icon: 'server',
    items: [
      { to: '/admin/nodes', name: 'admin-nodes', label: '节点列表', icon: 'server' },
      { to: '/admin/nodes/multiplier', name: 'admin-node-multipliers', label: '流量倍率', icon: 'traffic' },
      { to: '/admin/nodes/xray', name: 'admin-xray', label: 'Xray 高级配置', icon: 'activity' },
      { to: '/admin/geodata', name: 'admin-geodata', label: 'Geo 数据', icon: 'globe' },
    ],
  },
  {
    id: 'upstream',
    label: '上游货源',
    icon: 'layers',
    items: [
      { to: '/admin/upstream/third-party', name: 'admin-upstream-third-party', label: '第三方机场', icon: 'globe' },
      { to: '/admin/upstream/webshare', name: 'admin-upstream-webshare', label: 'Webshare 代理', icon: 'server' },
      { to: '/admin/upstream/proxy-store', name: 'admin-upstream-proxy-store', label: '商城商品', icon: 'cart' },
      { to: '/admin/upstream/proxy-ops', name: 'admin-upstream-proxy-ops', label: '代理运营', icon: 'shield' },
    ],
  },
  {
    id: 'site',
    label: '站点运营',
    icon: 'edit',
    items: [
      { to: '/admin/site-settings', name: 'admin-site-settings', label: '网站信息', icon: 'edit' },
      { to: '/admin/appearance', name: 'admin-appearance', label: '主页与配色', icon: 'palette' },
      { to: '/admin/announcement', name: 'admin-announcement', label: '公告', icon: 'edit' },
      { to: '/admin/tutorials', name: 'admin-tutorials', label: '教程', icon: 'copy' },
      { to: '/admin/subscription-settings', name: 'admin-subscription-settings', label: '订阅配置', icon: 'refresh' },
    ],
  },
  {
    id: 'system',
    label: '系统设置',
    icon: 'settings',
    items: [
      { to: '/admin/system-status', name: 'admin-system-status', label: '系统监控', icon: 'activity' },
      { to: '/admin/audit-logs', name: 'admin-audit-logs', label: '操作审计', icon: 'search' },
      { to: '/admin/settings', name: 'admin-settings', label: '管理员账户', icon: 'shield' },
      { to: '/admin/domains', name: 'admin-domains', label: '域名与证书', icon: 'globe' },
      { to: '/admin/payments', name: 'admin-payments', label: '支付配置', icon: 'payment' },
      { to: '/admin/app-version', name: 'admin-app-version', label: 'App 版本', icon: 'package' },
      { to: '/admin/email', name: 'admin-email', label: '邮箱服务', icon: 'mail' },
      { to: '/admin/telegram', name: 'admin-telegram', label: 'Telegram 机器人', icon: 'send' },
      { to: '/admin/backup', name: 'admin-backup', label: '数据备份', icon: 'inbox' },
    ],
  },
]

/**
 * 分组展开状态。
 * 默认只展开「当前页所在组」，其余折叠 —— 5 组 24 项全展开会直接把侧栏撑满，
 * 找东西反而更慢。用户手动开合过的组记在 localStorage，优先于默认值。
 */
const readExpandedGroups = () => {
  if (typeof window === 'undefined') return {}
  try {
    const raw = localStorage.getItem(NAV_EXPANDED_KEY)
    if (!raw) return {}
    const parsed = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {}
    const out = {}
    for (const [k, v] of Object.entries(parsed)) {
      if (typeof v === 'boolean') out[k] = v
    }
    return out
  } catch {
    return {}
  }
}

const expandedGroups = ref(readExpandedGroups())

// 搜索过滤：项名命中即保留；搜索时所有组强制展开（否则命中被折叠藏起来）
const keyword = computed(() => navSearch.value.trim().toLowerCase())

const matchItems = (items) =>
  items.filter((item) => !keyword.value || item.label.toLowerCase().includes(keyword.value))

/** 当前路由所在分组 id（默认展开 + 高亮用） */
const currentGroupId = computed(
  () => groups.find((g) => g.items.some((i) => route.path === i.to))?.id || ''
)

/** 只保留「当前组」或「搜索命中」的组 */
const visibleGroups = computed(() =>
  groups
    .map((g) => ({ ...g, visibleItems: matchItems(g.items) }))
    .filter((g) => g.id === currentGroupId.value || g.visibleItems.length > 0)
)

const showNavItems = computed(() => matchItems(navItems))

const isGroupExpanded = (id) => {
  if (keyword.value) return true
  const saved = expandedGroups.value[id]
  if (saved === undefined) return id === currentGroupId.value
  return saved
}

const isGroupActive = (group) => group.items.some((i) => route.path === i.to)

const toggleGroup = (id) => {
  // 用 isGroupExpanded 取反，而不是 !expandedGroups[id]：
  // 后者在键缺失时得到 undefined，取反为 true，与「默认只展开当前组」的语义相反。
  expandedGroups.value[id] = !isGroupExpanded(id)
  if (typeof window !== 'undefined') {
    localStorage.setItem(NAV_EXPANDED_KEY, JSON.stringify(expandedGroups.value))
  }
}

const toggleSidebar = () => {
  sidebarCollapsed.value = !sidebarCollapsed.value
  sidebarUserToggled.value = true
  if (typeof window !== 'undefined') {
    localStorage.setItem(SIDEBAR_COLLAPSED_KEY, String(sidebarCollapsed.value))
  }
}

const handleResize = () => {
  isNarrow.value = window.innerWidth < 768
  if (sidebarUserToggled.value) return
  sidebarCollapsed.value = window.innerWidth < SIDEBAR_AUTO_COLLAPSE_WIDTH
}

onMounted(() => {
  const saved = localStorage.getItem(SIDEBAR_COLLAPSED_KEY)
  if (saved !== null) {
    sidebarCollapsed.value = saved === 'true'
    sidebarUserToggled.value = true
  } else {
    sidebarCollapsed.value = window.innerWidth < SIDEBAR_AUTO_COLLAPSE_WIDTH
  }
  isNarrow.value = window.innerWidth < 768
  window.addEventListener('resize', handleResize)

  // 管理后台配色：作用域类挂 body（用户端/公开首页没有这个类，颜色零外溢），
  // 再拉取管理员配置注入；拉不到就保持出厂配色，不阻塞进入后台。
  document.body.classList.add('admin-app')
  adminApi
    .getPanelColors()
    .then((cfg) => applyPanelColors(cfg))
    .catch(() => {})
})

onBeforeUnmount(() => {
  window.removeEventListener('resize', handleResize)
  // 离开管理端：撤掉作用域类与注入样式，回登录页/用户端不残留
  document.body.classList.remove('admin-app')
  clearPanelColors()
})

const onLogout = async () => {
  await admin.logout()
  router.push({ name: 'admin-login' })
}
</script>

<template>
  <div class="al-wrap">
    <!-- 移动端遮罩 -->
    <div v-if="open" class="al-overlay" @click="open = false"></div>

    <aside
      class="al-sidebar"
      :class="{
        'al-sidebar--open': open,
        'al-sidebar--collapsed': sidebarCollapsed && !isNarrow,
      }"
    >
      <div class="al-brand">
        <template v-if="isNarrow || !sidebarCollapsed">
          <span class="al-brand-logo">
            <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="logo" class="al-brand-logo-img" />
            <Icon v-else name="shield" :size="20" />
          </span>
          <span class="al-brand-text">
            <span class="al-brand-name">{{ siteConfig.title }}</span>
            <span class="al-brand-sub">管理控制台</span>
          </span>
        </template>
        <span v-else class="al-brand-logo al-brand-logo--collapsed">
          <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="logo" class="al-brand-logo-img" />
          <Icon v-else name="shield" :size="20" />
        </span>
      </div>

      <div v-if="isNarrow || !sidebarCollapsed" class="al-search">
        <Icon name="search" :size="16" class="al-search-icon" />
        <input v-model="navSearch" class="al-search-input" type="text" placeholder="搜索菜单…" />
      </div>

      <nav class="al-nav">
        <!-- ===== 展开态：分组可折叠，子项缩进 ===== -->
        <template v-if="isNarrow || !sidebarCollapsed">
          <RouterLink
            v-for="item in showNavItems"
            :key="item.name"
            :to="item.to"
            class="al-nav-item"
            @click="open = false"
          >
            <Icon :name="item.icon" :size="16" />
            <span>{{ item.label }}</span>
          </RouterLink>

          <div v-for="g in visibleGroups" :key="g.id" class="al-nav-group">
            <button
              class="al-nav-item al-nav-item-btn"
              :class="{ 'is-open': isGroupExpanded(g.id), 'is-current': isGroupActive(g) }"
              @click="toggleGroup(g.id)"
            >
              <Icon :name="g.icon" :size="16" />
              <span>{{ g.label }}</span>
              <Icon
                :name="isGroupExpanded(g.id) ? 'chevron-down' : 'chevron-right'"
                :size="16"
                class="al-nav-arrow"
              />
            </button>
            <div v-show="isGroupExpanded(g.id)" class="al-nav-sub">
              <RouterLink
                v-for="item in g.visibleItems"
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

          <p v-if="!showNavItems.length && !visibleGroups.length" class="al-nav-empty">
            没有匹配的菜单
          </p>
        </template>

        <!-- ===== 收起态：扁平图标条，组间分隔线，无缩进无文字 ===== -->
        <template v-else>
          <RouterLink
            v-for="item in showNavItems"
            :key="`rail-${item.name}`"
            :to="item.to"
            class="al-nav-item al-nav-rail"
            :title="item.label"
            @click="open = false"
          >
            <Icon :name="item.icon" :size="20" />
          </RouterLink>

          <template v-for="g in visibleGroups" :key="`rail-g-${g.id}`">
            <div class="al-nav-sep"></div>
            <RouterLink
              v-for="item in g.visibleItems"
              :key="`rail-${item.name}`"
              :to="item.to"
              class="al-nav-item al-nav-rail"
              :title="item.label"
              @click="open = false"
            >
              <Icon :name="item.icon" :size="20" />
            </RouterLink>
          </template>
        </template>
      </nav>

      <div class="al-sidebar-foot">
        <button
          class="al-collapse-btn"
          type="button"
          :title="sidebarCollapsed ? '展开侧栏' : '收起侧栏'"
          @click="toggleSidebar"
        >
          <Icon :name="sidebarCollapsed ? 'chevrons-right' : 'chevrons-left'" :size="16" />
        </button>
      </div>
    </aside>

    <div class="al-main">
      <header class="al-header">
        <button class="al-icon-btn al-menu-btn" type="button" aria-label="菜单" @click="open = !open">
          <Icon name="menu" :size="20" />
        </button>
        <button
          class="al-icon-btn al-header-collapse"
          type="button"
          :title="sidebarCollapsed ? '展开侧栏' : '收起侧栏'"
          @click="toggleSidebar"
        >
          <Icon :name="sidebarCollapsed ? 'chevrons-right' : 'chevrons-left'" :size="16" />
        </button>
        <span class="al-header-title">管理控制台</span>
        <span class="al-header-spacer"></span>
        <ThemeToggle />
        <Button variant="secondary" size="sm" @click="onLogout">
          <Icon name="logout" :size="16" /> <span class="al-logout-label">退出</span>
        </Button>
      </header>
      <main class="al-content">
        <router-view v-slot="{ Component }">
          <keep-alive>
            <component :is="Component" />
          </keep-alive>
        </router-view>
      </main>
    </div>
  </div>
</template>

<style scoped>
.al-wrap {
  display: flex;
  min-height: 100vh;
  background: var(--bg-base);
}

/* ===== 侧栏 ===== */
.al-sidebar {
  width: 256px;
  flex-shrink: 0;
  background: var(--bg-surface);
  border-right: 1px solid var(--border-subtle);
  /* 侧栏投影：与右侧内容分层，纯边框贴在一起显得平 */
  box-shadow: 1px 0 3px -1px hsl(240 5.9% 10% / 0.06);
  display: flex;
  flex-direction: column;
  position: sticky;
  top: 0;
  height: 100vh;
  transition: width 300ms var(--ease-out);
  overflow: hidden;
}
html.dark .al-sidebar {
  box-shadow: 1px 0 3px -1px hsl(0 0% 0% / 0.4);
}
.al-sidebar--collapsed {
  width: 64px;
}

.al-brand {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-6);
  flex-shrink: 0;
}
.al-brand-logo {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  flex-shrink: 0;
  border-radius: var(--radius-md);
  background: var(--accent);
  color: var(--text-inverse);
}
.al-brand-logo:has(img) {
  background: transparent;
}
/* 收起态：64px 栏里 brand 区内边距收到 8px，logo 维持 32px 不被压缩 */
.al-sidebar--collapsed .al-brand {
  padding: var(--space-2);
  justify-content: center;
}
.al-brand-logo--collapsed {
  width: 32px;
  height: 32px;
  flex: none;
}
.al-brand-logo-img {
  width: 32px;
  height: 32px;
  object-fit: contain;
  border-radius: var(--radius-md);
}
.al-brand-text {
  display: flex;
  flex-direction: column;
  min-width: 0;
  line-height: 1.4;
}
.al-brand-name {
  font-size: var(--text-xl);
  font-weight: var(--font-semibold);
  letter-spacing: -0.01em;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.al-brand-sub {
  font-size: var(--text-xs);
  color: var(--text-muted);
  margin-top: 4px;
  white-space: nowrap;
}

/* 搜索框 */
.al-search {
  position: relative;
  padding: 0 var(--space-3) var(--space-2);
  flex-shrink: 0;
}
.al-search-icon {
  position: absolute;
  left: calc(var(--space-3) + 10px);
  top: 8px;
  color: var(--text-muted);
  pointer-events: none;
}
.al-search-input {
  width: 100%;
  height: 32px;
  padding: 0 var(--space-3) 0 32px;
  background: var(--bg-elevated);
  border: 1px solid transparent;
  border-radius: var(--radius-md);
  color: var(--text-primary);
  font-size: var(--text-xs);
  outline: none;
  transition: border-color var(--duration-fast) var(--ease-out);
}
.al-search-input::placeholder {
  color: var(--text-muted);
}
.al-search-input:focus {
  border-color: var(--border-focus);
}

/* 导航 */
/* 侧栏滚动条全部隐藏（菜单超高时仍可滚，只是不显示滚动条） */
.al-nav, .al-sidebar {
  scrollbar-width: none;
  -ms-overflow-style: none;
}
.al-nav::-webkit-scrollbar, .al-sidebar::-webkit-scrollbar {
  display: none;
}
.al-nav {
  flex: 1;
  padding: 0 var(--space-3) var(--space-3);
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  overflow-y: auto;
  overflow-x: hidden;
  /* 子项不许被纵向压扁：展开「设置」10 项后内容超高，
     若允许压缩，图标/文字会被挤变形（这就是点设置图标缩小的原因） */
  min-height: 0;
}
.al-nav > * {
  flex-shrink: 0;
}
.al-nav :deep(svg) {
  flex: none;
}
.al-nav-item {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  /* 图标不被 flex 压缩，避免文字长时把 svg 挤扁 */
  flex-shrink: 0;
  padding: var(--space-2) var(--space-3);
  min-height: 36px;
  border: none;
  background: transparent;
  border-radius: var(--radius-lg);
  color: var(--text-secondary);
  font-size: var(--text-sm);
  text-decoration: none;
  white-space: nowrap;
  transition: background-color var(--duration-fast) var(--ease-out),
    color var(--duration-fast) var(--ease-out);
}
.al-sidebar--collapsed .al-nav-item {
  justify-content: center;
  padding: var(--space-2);
  gap: 0;
}
.al-nav-item:hover {
  background: var(--bg-hover);
  color: var(--text-primary);
}
.al-nav-item.router-link-exact-active {
  background: var(--accent-muted);
  color: var(--text-primary);
  font-weight: var(--font-medium);
  /* 左侧 2px 指示条：比纯底色更容易一眼定位当前页 */
  box-shadow: inset 2px 0 0 0 var(--accent);
}
/* 折叠态没地方显示指示条，改用更明显的底色 */
.al-sidebar--collapsed .al-nav-item.router-link-exact-active {
  box-shadow: none;
}
.al-nav-item-btn {
  width: 100%;
  cursor: pointer;
  font-family: inherit;
  text-align: left;
}
.al-nav-item-btn.is-open {
  color: var(--text-primary);
}
/* 当前页所在的组：折叠状态下也要能看出「我在这组里」 */
.al-nav-item-btn.is-current {
  color: var(--text-primary);
  font-weight: var(--font-medium);
}
.al-nav-arrow {
  margin-left: auto;
  color: var(--text-muted);
  flex-shrink: 0;
}
.al-nav-sub {
  padding-left: 36px;
}
.al-nav-sub-item {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-3);
  min-height: 36px;
  border-radius: var(--radius-lg);
  color: var(--text-secondary);
  text-decoration: none;
  font-size: var(--text-sm);
  white-space: nowrap;
  transition: background-color var(--duration-fast) var(--ease-out),
    color var(--duration-fast) var(--ease-out);
}


.al-nav-sub-item:hover {
  background: var(--bg-hover);
  color: var(--text-primary);
}
.al-nav-sub-item.router-link-exact-active {
  background: var(--accent-muted);
  color: var(--text-primary);
  font-weight: var(--font-medium);
}
/* 收起态：图标放大到 20px 并强制方形，避免 svg 在 flex 里被压扁 */
/* 收起态（rail）：图标条，居中、无文字、无缩进 */
.al-sidebar--collapsed .al-nav-rail {
  justify-content: center;
  padding: var(--space-2);
  gap: 0;
  min-height: 40px;
}
/* 图标锁死 20px 且不参与 flex 压缩（不然会被挤扁变形） */
.al-nav-rail :deep(svg) {
  width: 20px;
  height: 20px;
  flex: none;
}
/* 组与组之间淡分隔线，否则一列图标分不清归属 */
.al-nav-sep {
  height: 1px;
  margin: var(--space-2) var(--space-2);
  background: var(--border-subtle);
}
.al-nav-empty {
  margin: 0;
  padding: var(--space-4) var(--space-3);
  border: 1px dashed var(--border-subtle);
  border-radius: var(--radius-lg);
  font-size: var(--text-xs);
  color: var(--text-muted);
  text-align: center;
}

/* 侧栏底部 */
.al-sidebar-foot {
  flex-shrink: 0;
  border-top: 1px solid var(--border-subtle);
  display: flex;
  flex-direction: column;
}
.al-collapse-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--space-3);
  width: 100%;
  border: none;
  background: transparent;
  color: var(--text-muted);
  cursor: pointer;
  transition: background-color var(--duration-fast) var(--ease-out),
    color var(--duration-fast) var(--ease-out);
}
.al-collapse-btn:hover {
  background: var(--bg-hover);
  color: var(--text-primary);
}

/* ===== 主区 ===== */
.al-main {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.al-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
  background: var(--bg-surface);
  position: sticky;
  top: 0;
  z-index: 20;
  /* sticky 顶栏的投影：滚动时内容从下方穿过，没有阴影会糊在一起 */
  box-shadow: 0 1px 3px -1px hsl(240 5.9% 10% / 0.08);
  backdrop-filter: saturate(180%) blur(8px);
}
html.dark .al-header {
  box-shadow: 0 1px 3px -1px hsl(0 0% 0% / 0.5);
}
.al-icon-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  border: none;
  border-radius: var(--radius-md);
  background: transparent;
  color: var(--text-secondary);
  cursor: pointer;
  transition: background-color var(--duration-fast) var(--ease-out),
    color var(--duration-fast) var(--ease-out);
}
.al-icon-btn:hover {
  background: var(--bg-hover);
  color: var(--text-primary);
}
.al-menu-btn {
  display: none;
}
.al-header-title {
  font-size: var(--text-sm);
  color: var(--text-muted);
}
.al-header-spacer {
  flex: 1;
}
.al-content {
  flex: 1;
  padding: var(--space-4);
  overflow-x: hidden;
  min-width: 0;
}
.al-overlay {
  display: none;
}

@media (min-width: 768px) {
  .al-header {
    padding: var(--space-4) var(--space-8);
  }
  .al-content {
    padding: var(--space-6) var(--space-8);
  }
}

@media (max-width: 767px) {
  .al-sidebar {
    position: fixed;
    left: 0;
    top: 0;
    z-index: 900;
    width: 256px;
    transform: translateX(-100%);
    transition: transform var(--duration-base) var(--ease-out);
  }
  .al-sidebar--collapsed {
    width: 256px;
  }
  .al-sidebar--open {
    transform: translateX(0);
  }
  /* 移动端抽屉固定 256px：永远展开显示文字，不走 rail 模式 */
  .al-menu-btn {
    display: inline-flex;
  }
  /* 移动端抽屉不需要「收起侧栏」按钮 */
  .al-collapse-btn {
    display: none;
  }
  .al-header-collapse {
    display: none;
  }
  .al-logout-label {
    display: none;
  }
  .al-overlay {
    display: block;
    position: fixed;
    inset: 0;
    background: var(--overlay);
    z-index: 800;
  }
  /* 窄屏贴边：内容区外层留白收到 0，卡片直接贴屏幕边框。
     元素内部本来就有 padding，外面再留一圈纯属浪费手机屏幕宽度。 */
  .al-content {
    padding: 0;
  }
}
</style>