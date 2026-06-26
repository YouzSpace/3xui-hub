<script setup>
/**
 * 用户注册 — 精致优雅风格
 */
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import Input from '@/components/ui/Input.vue'
import Icon from '@/components/ui/Icon.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useAuthStore } from '@/stores/auth'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'

const auth = useAuthStore()
const siteConfig = useSiteConfigStore()
const router = useRouter()
const toast = useToast()

const emailPrefix = ref('')
const emailDomain = ref('')
const password = ref('')
const confirmPassword = ref('')
const captcha = ref('')
const captchaSrc = ref('')
const loading = ref(false)

const domains = ['qq.com', '163.com', '126.com', 'gmail.com', 'outlook.com', 'hotmail.com', 'foxmail.com', 'yeah.net', 'sina.com']

const email = computed(() => {
  if (emailPrefix.value && emailDomain.value) return `${emailPrefix.value}@${emailDomain.value}`
  return ''
})

const showDomainList = ref(false)
const selectDomain = (d) => { emailDomain.value = d; showDomainList.value = false }
const refreshCaptcha = () => { captchaSrc.value = userApi.captchaUrl() }
onMounted(refreshCaptcha)

const onRegister = async () => {
  if (!email.value) { toast.error('请输入邮箱'); return }
  if (password.value.length < 6) { toast.error('密码至少 6 位'); return }
  if (password.value !== confirmPassword.value) { toast.error('两次密码不一致'); return }
  if (!captcha.value.trim()) { toast.error('请输入验证码'); return }
  loading.value = true
  try {
    await auth.register(email.value, password.value, captcha.value.trim())
    await auth.fetchMe()
    toast.success('注册成功')
    router.push('/dashboard')
  } catch (e) {
    toast.error(e.message || '注册失败')
    refreshCaptcha(); captcha.value = ''
  } finally { loading.value = false }
}
</script>

<template>
  <div class="reg-page">
    <div class="reg-bg">
      <div class="reg-bg-circle reg-bg-circle--1"></div>
      <div class="reg-bg-circle reg-bg-circle--2"></div>
    </div>
    <div class="reg-top">
      <router-link to="/" class="reg-back"><Icon name="chevron-left" :size="16" /><span>返回首页</span></router-link>
      <ThemeToggle />
    </div>
    <div class="reg-card">
      <div class="reg-head">
        <span class="reg-logo">
          <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="" class="reg-logo-img" />
          <Icon v-else name="shield" :size="24" />
        </span>
        <h1 class="reg-title">创建账号</h1>
        <p class="reg-sub">注册即刻开始使用</p>
      </div>
      <form class="reg-form" @submit.prevent="onRegister">
        <label class="reg-field">
          <span>邮箱</span>
          <div class="reg-email-row">
            <Input v-model="emailPrefix" placeholder="用户名" :disabled="loading" class="reg-email-prefix" />
            <span class="reg-email-at">@</span>
            <div class="reg-email-domain-wrap">
              <input v-model="emailDomain" placeholder="选择或输入域名" :disabled="loading" class="reg-email-domain-input" @focus="showDomainList = true" @blur="setTimeout(() => showDomainList = false, 200)" />
              <div v-if="showDomainList" class="reg-domain-list">
                <div v-for="d in domains" :key="d" class="reg-domain-item" @mousedown.prevent="selectDomain(d)">{{ d }}</div>
              </div>
            </div>
          </div>
        </label>
        <label class="reg-field"><span>密码</span><Input v-model="password" type="password" placeholder="至少 6 位" :disabled="loading" /></label>
        <label class="reg-field"><span>确认密码</span><Input v-model="confirmPassword" type="password" placeholder="再次输入密码" :disabled="loading" /></label>
        <label class="reg-field">
          <span>验证码</span>
          <div class="reg-captcha-row">
            <Input v-model="captcha" placeholder="输入验证码" :disabled="loading" class="reg-captcha-input" />
            <img :src="captchaSrc" class="reg-captcha-img" alt="验证码" @click="refreshCaptcha" />
          </div>
        </label>
        <button type="submit" class="reg-btn" :disabled="loading">
          <span v-if="loading" class="reg-btn-spinner"></span><span v-else>注册</span>
        </button>
        <div class="reg-links"><router-link class="reg-link" to="/login">已有账号？去登录</router-link></div>
      </form>
    </div>
  </div>
</template>

<style scoped>
.reg-page { min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 24px; position: relative; overflow: hidden; background: #fafafa; }
.reg-bg { position: absolute; inset: 0; pointer-events: none; }
.reg-bg-circle { position: absolute; border-radius: 50%; filter: blur(80px); opacity: 0.15; }
.reg-bg-circle--1 { width: 400px; height: 400px; background: #2563eb; top: -100px; left: -100px; }
.reg-bg-circle--2 { width: 300px; height: 300px; background: #7c3aed; bottom: -80px; right: -80px; }

.reg-top { position: absolute; top: 24px; left: 24px; right: 24px; display: flex; align-items: center; justify-content: space-between; z-index: 1; }
.reg-back { display: flex; align-items: center; gap: 6px; font-size: 13px; font-weight: 500; color: #86868b; text-decoration: none; transition: color 0.3s; }
.reg-back:hover { color: #1d1d1f; }

.reg-card { width: 100%; max-width: 420px; background: #fff; border: 1px solid rgba(0,0,0,0.04); border-radius: 28px; padding: 48px 36px 36px; box-shadow: 0 8px 60px -12px rgba(0,0,0,0.10); position: relative; z-index: 1; }
.reg-head { text-align: center; margin-bottom: 36px; }
.reg-logo { display: inline-flex; align-items: center; justify-content: center; width: 52px; height: 52px; border-radius: 16px; background: #eff6ff; color: #2563eb; margin-bottom: 16px; }
.reg-logo:has(img) { background: transparent; }
.reg-logo-img { width: 52px; height: 52px; object-fit: contain; border-radius: 16px; }
.reg-title { margin: 0 0 6px; font-size: 22px; font-weight: 800; letter-spacing: -0.02em; }
.reg-sub { margin: 0; font-size: 14px; color: #86868b; }

.reg-form { display: flex; flex-direction: column; gap: 18px; }
.reg-field { display: flex; flex-direction: column; gap: 8px; }
.reg-field span { font-size: 13px; font-weight: 600; color: #1d1d1f; }

.reg-email-row { display: flex; align-items: center; gap: 0; }
.reg-email-prefix { flex: 1; }
.reg-email-at { padding: 0 8px; color: #86868b; font-size: 14px; }
.reg-email-domain-wrap { flex: 1; position: relative; }
.reg-email-domain-input { width: 100%; background: #f5f5f5; border: 1px solid rgba(0,0,0,0.06); border-radius: 12px; color: #1d1d1f; padding: 10px 14px; font-size: 14px; outline: none; box-sizing: border-box; font-family: inherit; transition: border-color 0.3s; }
.reg-email-domain-input:focus { border-color: #2563eb; }
.reg-domain-list { position: absolute; top: calc(100% + 4px); left: 0; right: 0; background: #fff; border: 1px solid rgba(0,0,0,0.06); border-radius: 12px; z-index: 10; max-height: 200px; overflow-y: auto; box-shadow: 0 8px 30px rgba(0,0,0,0.1); }
.reg-domain-item { padding: 10px 14px; font-size: 13px; cursor: pointer; transition: background 0.2s; }
.reg-domain-item:hover { background: #f5f5f5; }

.reg-captcha-row { display: flex; gap: 12px; align-items: center; }
.reg-captcha-input { flex: 1; }
.reg-captcha-img { height: 40px; border-radius: 10px; cursor: pointer; border: 1px solid rgba(0,0,0,0.06); }

.reg-btn { width: 100%; padding: 14px; border-radius: 9999px; font-size: 15px; font-weight: 600; border: none; background: #1d1d1f; color: #fff; cursor: pointer; transition: all 0.4s cubic-bezier(0.16, 1, 0.3, 1); font-family: inherit; margin-top: 4px; display: flex; align-items: center; justify-content: center; }
.reg-btn:hover { background: #333; box-shadow: 0 8px 30px rgba(0,0,0,0.15); }
.reg-btn:active { transform: scale(0.98); }
.reg-btn:disabled { opacity: 0.6; cursor: not-allowed; }
.reg-btn-spinner { width: 18px; height: 18px; border: 2px solid rgba(255,255,255,0.3); border-top-color: #fff; border-radius: 50%; animation: spin 0.6s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }

.reg-links { text-align: center; }
.reg-link { font-size: 13px; color: #86868b; text-decoration: none; transition: color 0.3s; }
.reg-link:hover { color: #1d1d1f; }

@media (max-width: 480px) {
  .reg-card { padding: 36px 24px 28px; border-radius: 24px; }
  .reg-top { top: 16px; left: 16px; right: 16px; }
}

/* 暗色模式 */
html.dark .reg-page { background: #0a0a0a; }
html.dark .reg-bg-circle--1 { opacity: 0.08; }
html.dark .reg-bg-circle--2 { opacity: 0.08; }
html.dark .reg-back { color: #86868b; }
html.dark .reg-back:hover { color: #e5e5e5; }
html.dark .reg-card { background: rgba(20,20,20,0.9); border-color: rgba(255,255,255,0.06); box-shadow: 0 8px 60px -12px rgba(0,0,0,0.5); }
html.dark .reg-title { color: #fff; }
html.dark .reg-sub { color: #86868b; }
html.dark .reg-field span { color: #e5e5e5; }
html.dark .reg-email-domain-input { background: rgba(255,255,255,0.05); border-color: rgba(255,255,255,0.08); color: #e5e5e5; }
html.dark .reg-email-domain-input:focus { border-color: #60a5fa; }
html.dark .reg-domain-list { background: #1a1a1a; border-color: rgba(255,255,255,0.08); }
html.dark .reg-domain-item { color: #e5e5e5; }
html.dark .reg-domain-item:hover { background: rgba(255,255,255,0.05); }
html.dark .reg-captcha-img { border-color: rgba(255,255,255,0.08); }
html.dark .reg-btn { background: #fff; color: #0a0a0a; }
html.dark .reg-btn:hover { background: #e5e5e5; box-shadow: 0 8px 30px rgba(255,255,255,0.1); }
html.dark .reg-link { color: #86868b; }
html.dark .reg-link:hover { color: #e5e5e5; }
</style>
