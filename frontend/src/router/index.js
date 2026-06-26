import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useAdminStore } from '@/stores/admin'

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
    path: '/dashboard',
    component: () => import('@/layouts/UserLayout.vue'),
    meta: { requiresAuth: true },
    children: [
      {
        path: '',
        name: 'user-dashboard',
        component: () => import('@/views/user/Dashboard.vue'),
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
      { path: 'users', name: 'admin-users', component: () => import('@/views/admin/Users.vue') },
      { path: 'nodes', name: 'admin-nodes', component: () => import('@/views/admin/Nodes.vue') },
      { path: 'plans', name: 'admin-plans', component: () => import('@/views/admin/Plans.vue') },
      { path: 'orders', name: 'admin-orders', component: () => import('@/views/admin/Orders.vue') },
      { path: 'settings', name: 'admin-settings', component: () => import('@/views/admin/Settings.vue') },
      { path: 'payments', name: 'admin-payments', component: () => import('@/views/admin/Payments.vue') },
      { path: 'backup', name: 'admin-backup', component: () => import('@/views/admin/Backup.vue') },
      { path: 'site-settings', name: 'admin-site-settings', component: () => import('@/views/admin/SiteSettings.vue') },
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
