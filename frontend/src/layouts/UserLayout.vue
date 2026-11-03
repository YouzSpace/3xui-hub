<script setup>
/**
 * 用户端外壳（新 UI）：
 * - 桌面：左侧栏（Logo + 仪表盘/商店/我的 + 底部主题切换 / 退出登录）
 * - 移动：顶部 sticky 头（Logo + 铃铛公告）+ 底部浮动 Tab Bar
 * - 顶部铃铛只弹公告弹窗（不跳页）；公告全屏页由「我的」页入口承载
 */
import { computed, provide, ref, watch, onMounted } from 'vue'
import '@/assets/styles/user-ui.css'
import UserIcon from '@/components/user/UserIcon.vue'
import AnnouncementSheet from '@/components/user/AnnouncementSheet.vue'
import { useAuthStore } from '@/stores/auth'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { useThemeStore } from '@/stores/theme'
import { useRouter, useRoute } from 'vue-router'

const auth = useAuthStore()
const siteConfig = useSiteConfigStore()
const theme = useThemeStore()
const router = useRouter()
const route = useRoute()

const navItems = [
  {
    to: '/dashboard',
    name: 'user-dashboard',
    label: '仪表盘',
    icon: 'home',
  },
  { to: '/dashboard/shop', name: 'user-shop', label: '商店', icon: 'cart' },
  { to: '/dashboard/proxy', name: 'user-proxy', label: '代理', icon: 'globe', requiresProxyStore: true },
  { to: '/dashboard/more', name: 'user-more', label: '我的', icon: 'person' },
]

// 代理商城默认关闭：开关没开时「代理」入口不出现（后端接口同时拒绝，
// 直接输 URL 会被路由守卫弹回仪表盘 —— 不是只把菜单藏起来）
const visibleNavItems = computed(() =>
  navItems.filter((item) => !item.requiresProxyStore || siteConfig.proxyStoreEnabled)
)

const titles = {
  'user-dashboard': '仪表盘',
  'user-shop': '套餐商店',
  'user-proxy': '代理商城',
  'user-proxy-mine': '我的代理',
  'user-more': '我的',
  'user-announcement': '系统公告',
  'user-orders': '我的订单',
  'user-tutorials': '使用文档',
  'user-tutorial': '',
  'user-invite': '邀请好友',
  'user-balance': '账户余额',
}

const pageTitle = computed(() => titles[route.name] ?? '')

// 移动端表头：仪表盘页展示品牌名，其他页展示页面标题（二者互斥）
const isHome = computed(() => route.name === 'user-dashboard')

// 二级页（使用文档/文档详情/公告/订单）：表头左侧显示返回，不额外占一行
const subPageBack = computed(() => ({
  'user-tutorials': { name: 'user-more' },
  'user-tutorial': { name: 'user-tutorials' },
  'user-announcement': { name: 'user-more' },
  'user-orders': { name: 'user-shop' },
  'user-invite': { name: 'user-more' },
  'user-balance': { name: 'user-more' },
}[route.name] || null))

// 有历史记录就回上一页，否则按二级页自身的上级页兜底（直接分享链接打开时无历史）
const goBack = () => {
  if (window.history.state?.back) router.back()
  else if (subPageBack.value) router.replace(subPageBack.value)
}

const activeTab = computed(() => {
  if (route.name === 'user-shop' || route.name === 'user-orders') return 'user-shop'
  if (
    route.name === 'user-more' ||
    route.name === 'user-announcement' ||
    route.name === 'user-tutorials' ||
    route.name === 'user-tutorial' ||
    route.name === 'user-invite' ||
    route.name === 'user-balance'
  ) return 'user-more'
  return 'user-dashboard'
})

// 右上角铃铛：只打开公告弹窗，不跳全屏页（点它只表示"我现在要看"，不写 localStorage）
const showAnnounce = ref(false)
const openAnnouncement = () => {
  showAnnounce.value = true
}
provide('openAnnouncement', openAnnouncement)

const hasAnnouncement = computed(() => !!(siteConfig.config.announcement || '').trim())

// 发布新公告后，首次进入用户端自动弹出；同一篇公告点过「不再提示」则不再弹
const maybeAutoShowAnnouncement = async () => {
  try { await siteConfig.fetch() } catch (e) { /* 拉取失败就跳过 */ }
  const ann = (siteConfig.config.announcement || '').trim()
  if (!ann) return
  let dismissed = ''
  try { dismissed = localStorage.getItem('announcement_dismissed') || '' } catch (e) { dismissed = '' }
  if (dismissed !== ann) showAnnounce.value = true
}
onMounted(maybeAutoShowAnnouncement)

const onLogout = () => {
  auth.logout()
  router.push({ name: 'user-login' })
}

// 切换页面回到顶部
watch(
  () => route.fullPath,
  () => {
    const el = document.querySelector('.ul-scroll')
    if (el) el.scrollTop = 0
  }
)
</script>

<template>
  <div class="uu-root ul-wrap">
    <div class="ul-shell">
      <!-- 桌面左侧栏 -->
      <aside class="ul-sidebar">
        <div class="ul-brand">
          <span class="ul-logo" :class="{ 'ul-logo--img': !!siteConfig.logo }">
            <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="logo" class="ul-logo-img" />
            <UserIcon v-else name="shield" :size="22" />
          </span>
          <p class="ul-name">{{ siteConfig.title || 'ControlHub' }}</p>
        </div>

        <nav class="ul-nav">
          <RouterLink
            v-for="item in visibleNavItems"
            :key="item.name"
            :to="item.to"
            class="ul-nav-item uu-press"
            :class="{ 'ul-nav-item--active': activeTab === item.name }"
          >
            <UserIcon :name="item.icon" :size="26" :filled="activeTab === item.name" />
            <span>{{ item.label }}</span>
          </RouterLink>
        </nav>

        <div class="ul-side-foot">
          <button type="button" class="ul-side-btn uu-press" @click="theme.toggle()">
            <UserIcon :name="theme.isDark ? 'sun' : 'moon'" :size="20" />
            {{ theme.isDark ? '浅色模式' : '深色模式' }}
          </button>
          <button type="button" class="ul-side-btn ul-side-btn--danger uu-press" @click="onLogout">
            <UserIcon name="logout" :size="20" />
            退出登录
          </button>
        </div>
      </aside>

      <!-- 主区域 -->
      <main class="ul-main">
        <header class="ul-header">
          <!-- 二级页返回（桌面 + 移动同一位置，不额外占高度） -->
          <button
            v-if="subPageBack"
            type="button"
            class="ul-back uu-press"
            aria-label="返回"
            @click="goBack"
          >
            <UserIcon name="chevronLeft" :size="20" />
          </button>

          <!-- 移动端 Logo（品牌名只在仪表盘页显示） -->
          <div class="ul-header-brand" :class="{ 'ul-header-brand--back': !!subPageBack }">
            <span class="ul-logo ul-logo--sm" :class="{ 'ul-logo--img': !!siteConfig.logo }">
              <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="logo" class="ul-logo-img ul-logo-img--sm" />
              <UserIcon v-else name="shield" :size="18" />
            </span>
            <span v-if="isHome" class="ul-name ul-name--sm">{{ siteConfig.title || 'ControlHub' }}</span>
          </div>

          <!-- 移动端页面标题（仪表盘页不显示，该页用品牌名） -->
          <div
            class="ul-header-title"
            :class="{ 'ul-header-title--mobile': !!pageTitle && !isHome }"
          >{{ pageTitle }}</div>

          <div class="ul-header-actions">
            <button type="button" class="ul-icon-btn uu-press" @click="openAnnouncement">
              <UserIcon name="bell" :size="22" :dot="hasAnnouncement" />
            </button>
          </div>
        </header>

        <div class="ul-scroll">
          <h1 v-if="pageTitle" class="ul-page-title ul-page-title--mobile">{{ pageTitle }}</h1>
          <RouterView v-slot="{ Component }">
            <Transition name="page-fade" mode="out-in">
              <component :is="Component" />
            </Transition>
          </RouterView>
        </div>
      </main>
    </div>

    <!-- 移动端底部 Tab Bar -->
    <nav class="ul-tabbar">
      <RouterLink
        v-for="item in visibleNavItems"
        :key="item.name"
        :to="item.to"
        class="ul-tabbar-item uu-press"
        :class="{ 'ul-tabbar-item--active': activeTab === item.name }"
      >
        <UserIcon :name="item.icon" :size="26" :filled="activeTab === item.name" />
        <span class="ul-tabbar-label">{{ item.label }}</span>
      </RouterLink>
    </nav>

    <!-- 公告弹层（右上角铃铛 / 新公告自动弹出共用） -->
    <AnnouncementSheet v-model="showAnnounce" />
  </div>
</template>

<style scoped>
.ul-wrap {
  min-height: 100vh;
  min-height: 100dvh;
  background: var(--ground);
  color: var(--lbl1);
  font-family: var(--uu-font-sans);
  font-size: 17px;
  line-height: 1.47;
}

.ul-shell {
  display: flex;
  width: 100%;
  max-width: 1080px;
  margin: 0 auto;
  min-height: 100dvh;
}

/* ========== 桌面左侧栏 ========== */
.ul-sidebar {
  display: none;
  flex-direction: column;
  flex-shrink: 0;
  position: sticky;
  top: 0;
  height: 100dvh;
  width: var(--sidebar-w);
  padding: 28px 12px 0 16px;
}

.ul-brand {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 0 12px;
  margin-bottom: 32px;
}

.ul-logo {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  border-radius: 16px;
  background: var(--blue);
  color: #fff;
  flex-shrink: 0;
  overflow: hidden;
}
/* 配置了自定义 logo 图片时去掉蓝色底衬 */
.ul-logo--img {
  background: transparent;
}
.ul-logo--sm {
  width: 32px;
  height: 32px;
  border-radius: 12px;
}
.ul-logo-img {
  width: 100%;
  height: 100%;
  object-fit: contain;
}
.ul-logo-img--sm {
  padding: 4px;
}

.ul-name {
  margin: 0;
  font-size: 15px;
  font-weight: 700;
  color: var(--lbl1);
}
.ul-name--sm {
  font-size: 17px;
  font-weight: 600;
  letter-spacing: -0.2px;
}

.ul-nav {
  display: flex;
  flex-direction: column;
  gap: 4px;
  flex: 1;
}

.ul-nav-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 14px;
  border-radius: 16px;
  border: 1px solid transparent;
  background: transparent;
  color: var(--lbl3);
  font-size: 15px;
  font-weight: 400;
  text-decoration: none;
  transition: all 0.2s;
}
.ul-nav-item--active {
  background: var(--mat-regular);
  border-color: var(--border-glass);
  color: var(--blue);
  font-weight: 600;
}

.ul-side-foot {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding-bottom: 24px;
}

.ul-side-btn {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 14px;
  border: none;
  border-radius: 16px;
  background: transparent;
  color: var(--lbl3);
  font-size: 14px;
  font-family: inherit;
}
.ul-side-btn--danger {
  color: var(--red);
}

/* ========== 主区域 ========== */
.ul-main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.ul-header {
  position: sticky;
  top: 12px;
  z-index: 30;
  display: flex;
  align-items: center;
  justify-content: space-between;
  height: 52px;
  margin: 12px 16px 0;
  padding: 0 16px;
  background: var(--nav-bg);
  backdrop-filter: blur(40px) saturate(180%);
  -webkit-backdrop-filter: blur(40px) saturate(180%);
  border: 1px solid var(--border-glass);
  border-radius: 18px;
}

.ul-header-brand {
  display: flex;
  align-items: center;
  gap: 10px;
}

/* 二级页：移动端隐藏 Logo，让返回箭头占最左，右侧铃铛保持 */
.ul-header-brand--back {
  display: none;
}

.ul-back {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 36px;
  height: 36px;
  margin-right: 8px;
  border: none;
  border-radius: 12px;
  background: var(--fill3);
  color: var(--lbl2);
  flex-shrink: 0;
}

/* 表头是 space-between：返回按钮在场时标题靠左，不被推到中间 */
.ul-header-title {
  display: none;
  flex: 1;
  min-width: 0;
  font-size: 17px;
  font-weight: 600;
  color: var(--lbl1);
}

/* 移动端：大标题移到表头，标题占满中间（与 Logo/返回同区） */
.ul-header-title--mobile {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  letter-spacing: -0.2px;
}
.ul-page-title--mobile {
  display: none;
}

.ul-header-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.ul-icon-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 36px;
  height: 36px;
  border: none;
  border-radius: 12px;
  background: var(--fill3);
  color: var(--lbl2);
}
.ul-scroll {
  flex: 1;
  padding: 20px 16px calc(env(safe-area-inset-bottom) + 108px);
}

.ul-page-title {
  margin: 0 0 20px;
  font-size: 34px;
  font-weight: 800;
  letter-spacing: -0.8px;
  color: var(--lbl1);
}

/* ========== 移动端底部 Tab Bar ========== */
.ul-tabbar {
  display: flex;
  align-items: flex-end;
  justify-content: space-around;
  position: fixed;
  left: 12px;
  right: 12px;
  bottom: calc(env(safe-area-inset-bottom) + 12px);
  z-index: 40;
  padding: 4px 8px;
  border-radius: 24px;
  border: 1px solid var(--border-glass);
  background: var(--tab-bg);
  backdrop-filter: blur(40px) saturate(180%);
  -webkit-backdrop-filter: blur(40px) saturate(180%);
}

.ul-tabbar-item {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
  flex: 1;
  padding: 8px 0 4px;
  color: var(--lbl3);
  text-decoration: none;
  transition: color 0.2s;
}
.ul-tabbar-item--active {
  color: var(--blue);
}

.ul-tabbar-label {
  font-size: 10px;
  font-weight: 400;
  letter-spacing: 0.02em;
}
.ul-tabbar-item--active .ul-tabbar-label {
  font-weight: 600;
}

/* ========== 页面切换 ========== */
.page-fade-enter-active {
  transition: opacity 0.2s ease-out;
}
.page-fade-leave-active {
  transition: opacity 0.15s ease-in;
}
.page-fade-enter-from,
.page-fade-leave-to {
  opacity: 0;
}

/* ========== 桌面端 ========== */
@media (min-width: 768px) {
  .ul-sidebar {
    display: flex;
  }
  .ul-header-brand {
    display: none;
  }
  .ul-header-title {
    display: block;
  }
  /* 桌面端标题回到内容区，不用表头里那份 */
  .ul-header-title--mobile {
    display: none;
  }
  .ul-page-title--mobile {
    display: block;
  }
  .ul-header {
    padding: 0 24px;
    margin: 12px 24px 0;
  }
  .ul-scroll {
    padding: 20px 24px 48px;
  }
  .ul-tabbar {
    display: none;
  }
}
</style>
