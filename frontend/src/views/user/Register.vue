<script setup>
/**
 * 用户注册 — 精致优雅风格
 */
import { ref, computed, watch, onMounted } from 'vue'
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

const email = ref('')
const password = ref('')
const confirmPassword = ref('')
const captcha = ref('')
const captchaSrc = ref('')
const loading = ref(false)

// 邮箱验证相关
const emailCode = ref('')
const emailCodeSending = ref(false)
const emailCodeSent = ref(false)
const emailCodeCountdown = ref(0)
// emailError：邮箱域名相关的常驻错误（白名单外 / 一次性邮箱），显示在邮箱输入框下方。
// 与 toast 的区别：toast 几秒就消失，用户回头看只会觉得"点了没反应"。
const emailError = ref('')
let emailCodeTimer = null

// 用户改了邮箱就清掉旧的域名错误——否则改对了红字还挂着，自相矛盾
watch(email, () => { emailError.value = '' })

const useEmailVerify = computed(() => siteConfig.registerEmailVerify)

const refreshCaptcha = () => { captchaSrc.value = userApi.captchaUrl() }
onMounted(refreshCaptcha)

const onRegister = async () => {
  if (!email.value.trim()) { toast.error('请输入邮箱'); return }
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.value.trim())) { toast.error('邮箱格式不正确'); return }
  if (password.value.length < 6) { toast.error('密码至少 6 位'); return }
  if (password.value !== confirmPassword.value) { toast.error('两次密码不一致'); return }
  
  if (useEmailVerify.value) {
    if (!emailCode.value.trim()) { toast.error('请输入邮箱验证码'); return }
  } else {
    if (!captcha.value.trim()) { toast.error('请输入验证码'); return }
  }
  
  loading.value = true
  try {
    const fullEmail = email.value.trim()
    if (useEmailVerify.value) {
      // 邮箱验证注册
      await auth.registerWithEmailVerify(fullEmail, password.value, emailCode.value.trim())
    } else {
      // 图形码验证注册
      await auth.register(fullEmail, password.value, captcha.value.trim())
    }
    await auth.fetchMe()
    toast.success('注册成功')
    router.push('/dashboard')
  } catch (e) {
    const msg = e.message || '注册失败'
    if (msg === '邮箱验证码错误') {
      toast.warning('验证码错误，请重新获取')
    } else {
      toast.error(msg)
    }
    if (!useEmailVerify.value) {
      refreshCaptcha()
      captcha.value = ''
    }
  } finally { loading.value = false }
}

const sendEmailCode = async () => {
  if (!email.value.trim()) { toast.error('请输入邮箱'); return }
  if (!useEmailVerify.value) { toast.error('未启用邮箱验证'); return }
  // 重发前清掉上一次的错误：用户改了邮箱后旧红字不该继续挂着
  emailError.value = ''
  emailCodeSending.value = true
  try {
    await userApi.sendEmailVerifyCode(email.value.trim())
    emailCodeSent.value = true
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
    const msg = e?.message || '发送失败'
    // 域名不符 / 一次性邮箱：错误常驻在邮箱输入框下方，不用 toast
    // （后端返回的文案本身就是完整的可执行提示，直接展示即可）
    if (msg.includes('常用邮箱') || msg.includes('一次性') || msg.includes('临时邮箱')) {
      emailError.value = msg
    } else {
      toast.error(msg)
    }
  } finally { emailCodeSending.value = false }
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
          <Input v-model="email" type="email" placeholder="name@example.com" :disabled="loading" />
          <!-- 邮箱域名不在白名单等错误：常驻红字，不用 toast（toast 几秒就消失，用户看不到为什么发不出去） -->
          <span v-if="emailError" class="reg-field-error">{{ emailError }}</span>
        </label>
        <label class="reg-field"><span>密码</span><Input v-model="password" type="password" placeholder="至少 6 位" :disabled="loading" /></label>
        <label class="reg-field"><span>确认密码</span><Input v-model="confirmPassword" type="password" placeholder="再次输入密码" :disabled="loading" /></label>
        <label v-if="!useEmailVerify" class="reg-field">
          <span>验证码</span>
          <div class="reg-captcha-row">
            <Input v-model="captcha" placeholder="输入验证码" :disabled="loading" class="reg-captcha-input" />
            <img :src="captchaSrc" class="reg-captcha-img" alt="验证码" @click="refreshCaptcha" />
          </div>
        </label>
        <label v-else class="reg-field">
          <div class="reg-field-head">
            <span>邮箱验证码</span>
            <span class="reg-field-tip">收不到邮箱？检查垃圾箱</span>
          </div>
          <div class="reg-captcha-row">
            <Input v-model="emailCode" placeholder="输入邮箱验证码" :disabled="loading" class="reg-captcha-input" />
            <button type="button" class="reg-send-code-btn" :disabled="emailCodeSending || emailCodeCountdown > 0" @click="sendEmailCode">
              {{ emailCodeCountdown > 0 ? '重新获取' : (emailCodeSent ? '重新发送' : '发送验证码') }}
            </button>
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

<style scoped src="@/assets/styles/pages/user/Register.css"></style>
