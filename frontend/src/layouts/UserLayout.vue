<script setup>
/**
 * 用户端外壳（新 UI）：
 * - 桌面：左侧栏（Logo + 仪表盘/商店/我的 + 底部主题切换 / 退出登录）
 * - 移动：顶部 sticky 头（Logo + 铃铛公告）+ 底部浮动 Tab Bar
 * - 顶部铃铛只弹公告弹窗（不跳页）；公告全屏页由「我的」页入口承载
 */
import { computed, provide, ref, watch, onMounted } from 'vue'
import '@/assets/styles/base/user-ui.css'
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
  { to: '/dashboard/more', name: 'user-more', label: '我的', icon: 'person' },
]

const titles = {
  'user-dashboard': '仪表盘',
  'user-shop': '套餐商店',
  'user-more': '我的',
  'user-announcement': '系统公告',
  'user-orders': '我的订单',
  'user-tutorials': '使用文档',
  'user-tutorial': '',
  'user-contact': '反馈与联系',
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
  'user-contact': { name: 'user-more' },
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
    route.name === 'user-contact' ||
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
            v-for="item in navItems"
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

        <div class="ul-scroll" :class="{ 'ul-scroll--bleed': route.name === 'user-tutorial' }">
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
        v-for="item in navItems"
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

<style scoped src="@/assets/styles/layouts/UserLayout.css"></style>
