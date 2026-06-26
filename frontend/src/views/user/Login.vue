<script setup>
/**
 * 用户登录 — 精致优雅风格
 */
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Input from '@/components/ui/Input.vue'
import Icon from '@/components/ui/Icon.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useAuthStore } from '@/stores/auth'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { useToast } from '@/components/ui/Toast.vue'

const auth = useAuthStore()
const siteConfig = useSiteConfigStore()
const route = useRoute()
const router = useRouter()
const toast = useToast()

const mode = ref('email')
const email = ref('')
const password = ref('')
const token = ref('')
const loading = ref(false)

const onEmailLogin = async () => {
  if (!email.value.trim() || !password.value) {
    toast.error('请输入邮箱和密码')
    return
  }
  loading.value = true
  try {
    await auth.loginEmail(email.value.trim(), password.value)
    await auth.fetchMe()
    toast.success('登录成功')
    router.push(route.query.redirect || '/dashboard')
  } catch (e) {
    toast.error(e.message || '登录失败')
  } finally {
    loading.value = false
  }
}

const onTokenLogin = async () => {
  if (!token.value.trim()) {
    toast.error('请输入 Token')
    return
  }
  loading.value = true
  try {
    await auth.login(token.value.trim())
    await auth.fetchMe()
    toast.success('登录成功')
    router.push(route.query.redirect || '/dashboard')
  } catch (e) {
    toast.error(e.message || '登录失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <div class="login-bg">
      <div class="login-bg-circle login-bg-circle--1"></div>
      <div class="login-bg-circle login-bg-circle--2"></div>
    </div>

    <div class="login-top">
      <router-link to="/" class="login-back">
        <Icon name="chevron-left" :size="16" />
        <span>返回首页</span>
      </router-link>
      <ThemeToggle />
    </div>

    <div class="login-card">
      <div class="login-head">
        <span class="login-logo">
          <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="" class="login-logo-img" />
          <Icon v-else name="shield" :size="24" />
        </span>
        <h1 class="login-title">{{ siteConfig.title }}</h1>
        <p class="login-sub">登录你的账户</p>
      </div>

      <form v-if="mode === 'email'" class="login-form" @submit.prevent="onEmailLogin">
        <label class="login-field">
          <span>邮箱</span>
          <Input v-model="email" type="email" placeholder="your@email.com" :disabled="loading" />
        </label>
        <label class="login-field">
          <span>密码</span>
          <Input v-model="password" type="password" placeholder="请输入密码" :disabled="loading" />
        </label>
        <button type="submit" class="login-btn" :disabled="loading">
          <span v-if="loading" class="login-btn-spinner"></span>
          <span v-else>登录</span>
        </button>
        <div class="login-links">
          <a class="login-link" @click.prevent="mode = 'token'">或使用 Token 登录</a>
          <router-link class="login-link" to="/register">注册账号</router-link>
        </div>
      </form>

      <form v-else class="login-form" @submit.prevent="onTokenLogin">
        <label class="login-field">
          <span>订阅 Token</span>
          <Input v-model="token" mono placeholder="sub_xxxxxxxx" :disabled="loading" />
        </label>
        <button type="submit" class="login-btn" :disabled="loading">
          <span v-if="loading" class="login-btn-spinner"></span>
          <span v-else>登录</span>
        </button>
        <div class="login-links">
          <a class="login-link" @click.prevent="mode = 'email'">返回邮箱登录</a>
        </div>
      </form>
    </div>
  </div>
</template>

<style scoped>
.login-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  position: relative;
  overflow: hidden;
  background: #fafafa;
}
.login-bg { position: absolute; inset: 0; pointer-events: none; }
.login-bg-circle { position: absolute; border-radius: 50%; filter: blur(80px); opacity: 0.15; }
.login-bg-circle--1 { width: 400px; height: 400px; background: #2563eb; top: -100px; right: -100px; }
.login-bg-circle--2 { width: 300px; height: 300px; background: #7c3aed; bottom: -80px; left: -80px; }

.login-top {
  position: absolute; top: 24px; left: 24px; right: 24px;
  display: flex; align-items: center; justify-content: space-between; z-index: 1;
}
.login-back {
  display: flex; align-items: center; gap: 6px;
  font-size: 13px; font-weight: 500; color: #86868b; text-decoration: none; transition: color 0.3s;
}
.login-back:hover { color: #1d1d1f; }

.login-card {
  width: 100%; max-width: 400px;
  background: #fff; border: 1px solid rgba(0,0,0,0.04); border-radius: 28px;
  padding: 48px 36px 36px;
  box-shadow: 0 8px 60px -12px rgba(0,0,0,0.10);
  position: relative; z-index: 1;
}
.login-head { text-align: center; margin-bottom: 36px; }
.login-logo {
  display: inline-flex; align-items: center; justify-content: center;
  width: 52px; height: 52px; border-radius: 16px;
  background: #eff6ff; color: #2563eb; margin-bottom: 16px;
}
.login-logo:has(img) { background: transparent; }
.login-logo-img { width: 52px; height: 52px; object-fit: contain; border-radius: 16px; }
.login-title { margin: 0 0 6px; font-size: 22px; font-weight: 800; letter-spacing: -0.02em; }
.login-sub { margin: 0; font-size: 14px; color: #86868b; }

.login-form { display: flex; flex-direction: column; gap: 20px; }
.login-field { display: flex; flex-direction: column; gap: 8px; }
.login-field span { font-size: 13px; font-weight: 600; color: #1d1d1f; }

.login-btn {
  width: 100%; padding: 14px; border-radius: 9999px;
  font-size: 15px; font-weight: 600; border: none;
  background: #1d1d1f; color: #fff;
  cursor: pointer; transition: all 0.4s cubic-bezier(0.16, 1, 0.3, 1);
  font-family: inherit; margin-top: 4px;
  display: flex; align-items: center; justify-content: center;
}
.login-btn:hover { background: #333; box-shadow: 0 8px 30px rgba(0,0,0,0.15); }
.login-btn:active { transform: scale(0.98); }
.login-btn:disabled { opacity: 0.6; cursor: not-allowed; }
.login-btn-spinner {
  width: 18px; height: 18px;
  border: 2px solid rgba(255,255,255,0.3); border-top-color: #fff;
  border-radius: 50%; animation: spin 0.6s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

.login-links { display: flex; justify-content: space-between; }
.login-link { font-size: 13px; color: #86868b; cursor: pointer; text-decoration: none; transition: color 0.3s; }
.login-link:hover { color: #1d1d1f; }

@media (max-width: 480px) {
  .login-card { padding: 36px 24px 28px; border-radius: 24px; }
  .login-top { top: 16px; left: 16px; right: 16px; }
}

/* 暗色模式 */
html.dark .login-page { background: #0a0a0a; }
html.dark .login-bg-circle--1 { opacity: 0.08; }
html.dark .login-bg-circle--2 { opacity: 0.08; }
html.dark .login-back { color: #86868b; }
html.dark .login-back:hover { color: #e5e5e5; }
html.dark .login-card { background: rgba(20,20,20,0.9); border-color: rgba(255,255,255,0.06); box-shadow: 0 8px 60px -12px rgba(0,0,0,0.5); }
html.dark .login-title { color: #fff; }
html.dark .login-sub { color: #86868b; }
html.dark .login-field span { color: #e5e5e5; }
html.dark .login-btn { background: #fff; color: #0a0a0a; }
html.dark .login-btn:hover { background: #e5e5e5; box-shadow: 0 8px 30px rgba(255,255,255,0.1); }
html.dark .login-link { color: #86868b; }
html.dark .login-link:hover { color: #e5e5e5; }
</style>
