import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useAdminStore } from '@/stores/admin'
import { useSiteConfigStore } from '@/stores/siteConfig'

const routes = [
  // ===== 用户端 =====
  {
    path: '/',
    name: 'home',
    component: () => import('@/views/user/Home.vue'),
  },
  {
    path: '/login',
    name: 'user-login',
    component: () => import('@/views/user/Login.vue'),
    meta: { guest: true },
  },
  {
    path: '/register',
    name: 'user-register',
    component: () => import('@/views/user/Register.vue'),
    meta: { guest: true },
  },
  {
    path: '/forgot-password',
    name: 'user-forgot-password',
    component: () => import('@/views/user/ForgotPassword.vue'),
    meta: { guest: true },
  },
  {
    path: '/dashboard',
    component: () => import('@/layouts/UserLayout.vue'),
    meta: { requiresAuth: true },
    children: [
      {
        path: '',
        name: 'user-dashboard',
        component: () => import('@/views/user/Dashboard.vue'),
      },
      {
        path: 'shop',
        name: 'user-shop',
        component: () => import('@/views/user/Shop.vue'),
      },
      {
        path: 'more',
        name: 'user-more',
        component: () => import('@/views/user/More.vue'),
      },
      {
        path: 'balance',
        name: 'user-balance',
        component: () => import('@/views/user/Balance.vue'),
      },
      {
        path: 'announcement',
        name: 'user-announcement',
        component: () => import('@/views/user/Announcement.vue'),
      },
      {
        path: 'orders',
        name: 'user-orders',
        component: () => import('@/views/user/Orders.vue'),
      },
      {
        path: 'proxy',
        name: 'user-proxy',
        component: () => import('@/views/user/ProxyShop.vue'),
      },
      {
        path: 'proxy/mine',
        name: 'user-proxy-mine',
        component: () => import('@/views/user/ProxyMine.vue'),
      },
      {
        path: 'tutorials',
        name: 'user-tutorials',
        component: () => import('@/views/user/Tutorials.vue'),
      },
      {
        path: 'invite',
        name: 'user-invite',
        component: () => import('@/views/user/Invite.vue'),
      },
      {
        path: 'tutorial/:id',
        name: 'user-tutorial',
        component: () => import('@/views/user/TutorialDetail.vue'),
      },
      {
        path: 'contact',
        name: 'user-contact',
        component: () => import('@/views/user/Contact.vue'),
      },
    ],
  },

  // ===== Admin =====
  {
    path: '/admin/login',
    name: 'admin-login',
    component: () => import('@/views/admin/Login.vue'),
    meta: { guest: true },
  },
  {
    path: '/admin',
    component: () => import('@/layouts/AdminLayout.vue'),
    meta: { requiresAdmin: true },
    children: [
      { path: '', name: 'admin-dashboard', component: () => import('@/views/admin/Dashboard.vue') },
      // 独立监控页已并入仪表盘（流量排行 + 汇总曲线）：旧路径重定向，收藏/旧链接不 404
      { path: 'monitor', redirect: '/admin' },
      { path: 'users', name: 'admin-users', component: () => import('@/views/admin/Users.vue') },
      { path: 'nodes', name: 'admin-nodes', component: () => import('@/views/admin/Nodes.vue') },
      { path: 'nodes/multiplier', name: 'admin-node-multipliers', component: () => import('@/views/admin/NodeMultipliers.vue') },
      // 上游（外部货源接入）：第三方机场订阅 / Webshare 代理
      { path: 'upstream/third-party', name: 'admin-upstream-third-party', component: () => import('@/views/admin/ThirdParty.vue') },
      { path: 'upstream/webshare', name: 'admin-upstream-webshare', component: () => import('@/views/admin/UpstreamWebshare.vue') },
      { path: 'upstream/proxy-store', name: 'admin-upstream-proxy-store', component: () => import('@/views/admin/ProxyStore.vue') },
      { path: 'upstream/proxy-ops', name: 'admin-upstream-proxy-ops', component: () => import('@/views/admin/ProxyOps.vue') },
      // xray 高级配置：四个子页收进一个容器（顶部 Tab 切换），旧路径重定向保留
      { path: 'nodes/xray', name: 'admin-xray', component: () => import('@/views/admin/XrayAdvanced.vue') },
      { path: 'nodes/xray-inbounds', redirect: { path: '/admin/nodes/xray', query: { tab: 'inbounds' } } },
      { path: 'nodes/xray-outbounds', redirect: { path: '/admin/nodes/xray', query: { tab: 'outbounds' } } },
      { path: 'nodes/xray-routing', redirect: { path: '/admin/nodes/xray', query: { tab: 'routing' } } },
      { path: 'nodes/xray-warp', redirect: { path: '/admin/nodes/xray', query: { tab: 'warp' } } },
      { path: 'plans', name: 'admin-plans', component: () => import('@/views/admin/Plans.vue') },
      { path: 'orders', name: 'admin-orders', component: () => import('@/views/admin/Orders.vue') },
      { path: 'settings', name: 'admin-settings', component: () => import('@/views/admin/Settings.vue') },
      { path: 'domains', name: 'admin-domains', component: () => import('@/views/admin/Domains.vue') },
      { path: 'payments', name: 'admin-payments', component: () => import('@/views/admin/Payments.vue') },
      { path: 'backup', name: 'admin-backup', component: () => import('@/views/admin/Backup.vue') },
      { path: 'announcement', name: 'admin-announcement', component: () => import('@/views/admin/Announcement.vue') },
      { path: 'tutorials', name: 'admin-tutorials', component: () => import('@/views/admin/Tutorials.vue') },
      { path: 'email', name: 'admin-email', component: () => import('@/views/admin/Email.vue') },
      { path: 'telegram', name: 'admin-telegram', component: () => import('@/views/admin/TelegramBot.vue') },
      // 码管理：折扣码（打折）+ 余额码（充余额）统一一个页面两个 tab
      { path: 'codes', name: 'admin-codes', component: () => import('@/views/admin/Codes.vue') },
      // 旧路径重定向到统一「码管理」页
      { path: 'discount-codes', redirect: { path: '/admin/codes', query: { tab: 'discount' } } },
      { path: 'giftcards', redirect: { path: '/admin/codes', query: { tab: 'balance' } } },
      // 余额管理（余额域）：用户余额查询 + 人工调账（与用户列表页分开，见 prompts/balance-layout-20261103）
      { path: 'balance', name: 'admin-balance', component: () => import('@/views/admin/Balance.vue') },
      { path: 'site-settings', name: 'admin-site-settings', component: () => import('@/views/admin/SiteSettings.vue') },
      // 主页 + 配色收进一个容器（顶部 Tab），旧路径重定向保留
      { path: 'appearance', name: 'admin-appearance', component: () => import('@/views/admin/Appearance.vue') },
      { path: 'home-custom', redirect: { path: '/admin/appearance', query: { tab: 'home' } } },
      { path: 'panel-colors', redirect: { path: '/admin/appearance', query: { tab: 'colors' } } },
      { path: 'subscription-settings', name: 'admin-subscription-settings', component: () => import('@/views/admin/SubscriptionSettings.vue') },
      { path: 'app-version', name: 'admin-app-version', component: () => import('@/views/admin/AppVersion.vue') },
      { path: 'system-status', name: 'admin-system-status', component: () => import('@/views/admin/SystemStatus.vue') },
      { path: 'audit-logs', name: 'admin-audit-logs', component: () => import('@/views/admin/AuditLogs.vue') },
      // 邀请机制：邀请设置 + 返佣记录统一一个页面两个 tab（对齐「码管理」）
      { path: 'invite-settings', name: 'admin-invite-settings', component: () => import('@/views/admin/Invite.vue') },
      { path: 'geodata', name: 'admin-geodata', component: () => import('@/views/admin/Geodata.vue') },
    ],
  },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.beforeEach(async (to) => {
  if (to.meta.requiresAuth) {
    const auth = useAuthStore()
    if (!auth.ready) await auth.fetchMe()
    if (!auth.user) return { name: 'user-login', query: { redirect: to.fullPath } }

    // 代理商城默认关闭：直接输 URL 也进不去（后端接口同样会拒，
    // 这里只是别让用户看到空页面 / 报错弹窗）
    // 只拦「代理商城」页；「我的代理」不拦 —— 开关关闭只是停售，
    // 已购买的用户仍要能进去看凭据和 IP。
    if (to.name === 'user-proxy') {
      const siteConfig = useSiteConfigStore()
      if (!siteConfig.loaded) await siteConfig.fetch()
      if (!siteConfig.proxyStoreEnabled) return { name: 'user-dashboard' }
    }
  }

  if (to.meta.requiresAdmin) {
    const admin = useAdminStore()
    if (!admin.loggedIn) {
      const ok = await admin.probe()
      if (!ok) return { name: 'admin-login' }
    }
  }

  if (to.meta.guest) {
    const auth = useAuthStore()
    if (auth.token && !auth.ready) await auth.fetchMe()
    if (to.name === 'user-login' && auth.token && auth.user) return { name: 'user-dashboard' }
  }
})

export default router
