<script setup>
/**
 * 找回密码 — 与登录/注册同款精致优雅风格
 */
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import Input from '@/components/ui/Input.vue'
import Icon from '@/components/ui/Icon.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'

const siteConfig = useSiteConfigStore()
const router = useRouter()
const toast = useToast()

const email = ref('')
const emailCode = ref('')
const password = ref('')
const confirmPassword = ref('')
const loading = ref(false)

// 邮箱验证码相关
const emailCodeSending = ref(false)
const emailCodeCountdown = ref(0)
let emailCodeTimer = null

const onReset = async () => {
  if (!email.value.trim()) { toast.error('请输入邮箱'); return }
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.value.trim())) { toast.error('邮箱格式不正确'); return }
  if (password.value.length < 6) { toast.error('密码至少 6 位'); return }
  if (password.value !== confirmPassword.value) { toast.error('两次密码不一致'); return }
  if (!emailCode.value.trim()) { toast.error('请输入邮箱验证码'); return }

  loading.value = true
  try {
    await userApi.resetPassword(email.value.trim(), emailCode.value.trim(), password.value)
    toast.success('密码已重置，请重新登录')
    router.push('/login')
  } catch (e) {
    toast.error(e.message || '重置失败')
  } finally { loading.value = false }
}

const sendEmailCode = async () => {
  if (!email.value.trim()) { toast.error('请输入邮箱'); return }
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.value.trim())) { toast.error('邮箱格式不正确'); return }
  emailCodeSending.value = true
  try {
    await userApi.sendResetCode(email.value.trim())
    emailCodeCountdown.value = 60
    emailCodeTimer = setInterval(() => {
      emailCodeCountdown.value--
      if (emailCodeCountdown.value <= 0) {
        clearInterval(emailCodeTimer)
        emailCodeTimer = null
      }
    }, 1000)
    toast.success('验证码已发送')
  } catch (e) {
    toast.error(e.message || '发送失败')
  } finally { emailCodeSending.value = false }
}
</script>

<template>
  <div class="fp-page">
    <div class="fp-bg">
      <div class="fp-bg-circle fp-bg-circle--1"></div>
      <div class="fp-bg-circle fp-bg-circle--2"></div>
    </div>
    <div class="fp-top">
      <router-link to="/" class="fp-back"><Icon name="chevron-left" :size="16" /><span>返回首页</span></router-link>
      <ThemeToggle />
    </div>
    <div class="fp-card">
      <div class="fp-head">
        <span class="fp-logo">
          <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="" class="fp-logo-img" />
          <Icon v-else name="shield" :size="24" />
        </span>
        <h1 class="fp-title">找回密码</h1>
        <p class="fp-sub">通过邮箱验证码重置密码</p>
      </div>
      <form class="fp-form" @submit.prevent="onReset">
        <label class="fp-field">
          <span>邮箱</span>
          <Input v-model="email" type="email" placeholder="name@example.com" :disabled="loading" />
        </label>
        <label class="fp-field">
          <div class="fp-field-head">
            <span>邮箱验证码</span>
            <span class="fp-field-tip">收不到邮箱？检查垃圾箱</span>
          </div>
          <div class="fp-captcha-row">
            <Input v-model="emailCode" placeholder="输入邮箱验证码" :disabled="loading" class="fp-captcha-input" />
            <button type="button" class="fp-send-code-btn" :disabled="emailCodeSending || emailCodeCountdown > 0" @click="sendEmailCode">
              {{ emailCodeCountdown > 0 ? `${emailCodeCountdown}s` : '发送验证码' }}
            </button>
          </div>
        </label>
        <label class="fp-field"><span>新密码</span><Input v-model="password" type="password" placeholder="至少 6 位" :disabled="loading" /></label>
        <label class="fp-field"><span>确认新密码</span><Input v-model="confirmPassword" type="password" placeholder="再次输入新密码" :disabled="loading" /></label>
        <button type="submit" class="fp-btn" :disabled="loading">
          <span v-if="loading" class="fp-btn-spinner"></span><span v-else>重置密码</span>
        </button>
        <div class="fp-links"><router-link class="fp-link" to="/login">想起密码了？去登录</router-link></div>
      </form>
    </div>
  </div>
</template>

<style scoped>
.fp-page { min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 24px; position: relative; overflow: hidden; background: #fafafa; }
.fp-bg { position: absolute; inset: 0; pointer-events: none; }
.fp-bg-circle { position: absolute; border-radius: 50%; filter: blur(80px); opacity: 0.15; }
.fp-bg-circle--1 { width: 400px; height: 400px; background: #2563eb; top: -100px; left: -100px; }
.fp-bg-circle--2 { width: 300px; height: 300px; background: #7c3aed; bottom: -80px; right: -80px; }

.fp-top { position: absolute; top: 24px; left: 24px; right: 24px; display: flex; align-items: center; justify-content: space-between; z-index: 1; }
.fp-back { display: flex; align-items: center; gap: 6px; font-size: 13px; font-weight: 500; color: #86868b; text-decoration: none; transition: color 0.3s; }
.fp-back:hover { color: #1d1d1f; }

.fp-card { width: 100%; max-width: 420px; background: #fff; border: 1px solid rgba(0,0,0,0.04); border-radius: 28px; padding: 48px 36px 36px; box-shadow: 0 8px 60px -12px rgba(0,0,0,0.10); position: relative; z-index: 1; }
.fp-head { text-align: center; margin-bottom: 36px; }
.fp-logo { display: inline-flex; align-items: center; justify-content: center; width: 52px; height: 52px; border-radius: 16px; background: var(--accent-muted); color: var(--accent); margin-bottom: 16px; }
.fp-logo:has(img) { background: transparent; }
.fp-logo-img { width: 52px; height: 52px; object-fit: contain; border-radius: 16px; }
.fp-title { margin: 0 0 6px; font-size: 22px; font-weight: 800; letter-spacing: -0.02em; }
.fp-sub { margin: 0; font-size: 14px; color: #86868b; }

.fp-form { display: flex; flex-direction: column; gap: 18px; }
.fp-field { display: flex; flex-direction: column; gap: 8px; }
.fp-field span { font-size: 13px; font-weight: 600; color: #1d1d1f; }
.fp-field-head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
.fp-field-tip { font-size: 12px; font-weight: 400; color: #ef4444; }

.fp-captcha-row { display: flex; gap: 12px; align-items: center; }
.fp-captcha-input { flex: 1; }
.fp-send-code-btn { padding: 10px 16px; border-radius: 10px; font-size: 13px; font-weight: 600; border: 1px solid var(--border-default); background: var(--bg-page); color: var(--text-primary); cursor: pointer; white-space: nowrap; transition: all 0.2s; }
.fp-send-code-btn:hover:not(:disabled) { border-color: var(--accent); color: var(--accent); }
.fp-send-code-btn:disabled { opacity: 0.5; cursor: not-allowed; }

.fp-btn { width: 100%; padding: 14px; border-radius: 9999px; font-size: 15px; font-weight: 600; border: none; background: #1d1d1f; color: #fff; cursor: pointer; transition: all 0.4s cubic-bezier(0.16, 1, 0.3, 1); font-family: inherit; margin-top: 4px; display: flex; align-items: center; justify-content: center; }
.fp-btn:hover { background: #333; box-shadow: 0 8px 30px rgba(0,0,0,0.15); }
.fp-btn:active { transform: scale(0.98); }
.fp-btn:disabled { opacity: 0.6; cursor: not-allowed; }
.fp-btn-spinner { width: 18px; height: 18px; border: 2px solid rgba(255,255,255,0.3); border-top-color: #fff; border-radius: 50%; animation: spin 0.6s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }

.fp-links { text-align: center; }
.fp-link { font-size: 13px; color: #86868b; text-decoration: none; transition: color 0.3s; }
.fp-link:hover { color: #1d1d1f; }

@media (max-width: 480px) {
  .fp-card { padding: 36px 24px 28px; border-radius: 24px; }
  .fp-top { top: 16px; left: 16px; right: 16px; }
}

/* 暗色模式 */
html.dark .fp-page { background: #0a0a0a; }
html.dark .fp-bg-circle--1 { opacity: 0.08; }
html.dark .fp-bg-circle--2 { opacity: 0.08; }
html.dark .fp-back { color: #86868b; }
html.dark .fp-back:hover { color: #e5e5e5; }
html.dark .fp-card { background: rgba(20,20,20,0.9); border-color: rgba(255,255,255,0.06); box-shadow: 0 8px 60px -12px rgba(0,0,0,0.5); }
html.dark .fp-title { color: #fff; }
html.dark .fp-sub { color: #86868b; }
html.dark .fp-field span { color: #e5e5e5; }
html.dark .fp-field-tip { color: #f87171; }
html.dark .fp-btn { background: #fff; color: #0a0a0a; }
html.dark .fp-btn:hover { background: #e5e5e5; box-shadow: 0 8px 30px rgba(255,255,255,0.1); }
html.dark .fp-link { color: #86868b; }
html.dark .fp-link:hover { color: #e5e5e5; }
</style>
