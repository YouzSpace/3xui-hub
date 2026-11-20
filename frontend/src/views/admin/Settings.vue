<script setup>
/**
 * Admin 设置页：修改管理员用户名 + 密码 + 谷歌二步验证。
 */
import { ref, computed, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const admin = ref(null)
const saving = ref(false)
const savedUsername = ref(false)
const savedPassword = ref(false)

// 用户名表单
const usernameForm = ref({ username: '', password: '' })

// 密码表单
const passwordForm = ref({ old_password: '', new_password: '', confirm_password: '' })

// 2FA
const tfaSecret = ref('')
const tfaQrUrl = ref('')
const tfaOtp = ref('')
const tfaDisablePassword = ref('')
const tfaStep = ref('idle') // idle | scan | verify
// 二维码优先用后端给的 otpauth:// 链接；后端没给（旧版）时回落到本地按 secret 拼，
// 保证任何部署组合下都能出可扫的码，而不是拿空串去请求二维码服务（扫出来是空码）。
const tfaQrDisplay = computed(() => {
  if (tfaQrUrl.value) return tfaQrUrl.value
  if (!tfaSecret.value) return ''
  return `otpauth://totp/${encodeURIComponent('ControlHub:' + (admin.value?.username || 'admin'))}`
    + `?secret=${encodeURIComponent(tfaSecret.value)}&issuer=ControlHub`
})

const load = async () => {
  try {
    admin.value = await adminApi.getSettings()
  } catch (e) {
    // ignore
  }
}

const saveUsername = async () => {
  if (!usernameForm.value.username || !usernameForm.value.password) {
    toast.error('请填写完整')
    return
  }
  saving.value = true
  try {
    const res = await adminApi.updateUsername(usernameForm.value)
    admin.value.username = res.username
    toast.success('用户名已修改')
    usernameForm.value = { username: '', password: '' }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段）
    savedUsername.value = true
    setTimeout(() => { savedUsername.value = false }, 900)
  } catch (e) {
    toast.error(e.message || '修改失败')
  } finally {
    saving.value = false
  }
}

const savePassword = async () => {
  if (!passwordForm.value.old_password || !passwordForm.value.new_password) {
    toast.error('请填写完整')
    return
  }
  if (passwordForm.value.new_password !== passwordForm.value.confirm_password) {
    toast.error('两次新密码不一致')
    return
  }
  if (passwordForm.value.new_password.length < 6) {
    toast.error('新密码至少 6 位')
    return
  }
  saving.value = true
  try {
    await adminApi.changePassword({
      old_password: passwordForm.value.old_password,
      new_password: passwordForm.value.new_password,
    })
    toast.success('密码已修改')
    passwordForm.value = { old_password: '', new_password: '', confirm_password: '' }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段）
    savedPassword.value = true
    setTimeout(() => { savedPassword.value = false }, 900)
  } catch (e) {
    toast.error(e.message || '修改失败')
  } finally {
    saving.value = false
  }
}

// 2FA
const generate2fa = async () => {
  try {
    const res = await adminApi.google2faGenerate()
    tfaSecret.value = res.secret
    tfaQrUrl.value = res.qr_url
    tfaOtp.value = ''
    tfaStep.value = 'scan'
  } catch (e) {
    toast.error('生成失败')
  }
}

const enable2fa = async () => {
  if (!tfaSecret.value) {
    toast.error('请先点击「开启谷歌验证」生成密钥')
    return
  }
  if (!tfaOtp.value || tfaOtp.value.length !== 6) {
    toast.error('请输入6位验证码')
    return
  }
  saving.value = true
  try {
    // 必须把 secret 一起提交：后端用它算 TOTP 并落库，只传 otp 会「请先生成密钥」
    await adminApi.google2faEnable(tfaSecret.value, tfaOtp.value)
    toast.success('谷歌验证已启用')
    admin.value.google2fa_enabled = true
    tfaStep.value = 'idle'
    tfaSecret.value = ''
    tfaQrUrl.value = ''
    tfaOtp.value = ''
  } catch (e) {
    toast.error(e.message || '验证失败')
  } finally {
    saving.value = false
  }
}

const disable2fa = async () => {
  if (!tfaDisablePassword.value) {
    toast.error('请输入当前密码')
    return
  }
  saving.value = true
  try {
    await adminApi.google2faDisable(tfaDisablePassword.value)
    toast.success('谷歌验证已关闭')
    admin.value.google2fa_enabled = false
    tfaDisablePassword.value = ''
  } catch (e) {
    toast.error(e.message || '关闭失败')
  } finally {
    saving.value = false
  }
}

const cancel2fa = () => {
  tfaStep.value = 'idle'
  tfaSecret.value = ''
  tfaQrUrl.value = ''
  tfaOtp.value = ''
}

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="st">
    <h1 class="st-title">安全</h1>

    <!-- 用户名 -->
    <Card>
      <h2 class="st-section-title">管理员账户</h2>
      <p v-if="admin" class="st-info">当前用户名：<strong>{{ admin.username }}</strong></p>
      <div class="st-form">
        <label class="ch-field">
          <span>新用户名</span>
          <Input v-model="usernameForm.username" placeholder="至少 3 位" />
        </label>
        <label class="ch-field">
          <span>当前密码（验证身份）</span>
          <Input v-model="usernameForm.password" type="password" placeholder="输入当前密码" />
        </label>
        <div class="st-actions">
          <Button variant="primary" size="sm" :loading="saving" :success="savedUsername" @click="saveUsername">
            修改用户名
          </Button>
        </div>
      </div>
    </Card>

    <!-- 密码 -->
    <Card style="margin-top: var(--space-4);">
      <h2 class="st-section-title">修改密码</h2>
      <div class="st-form">
        <label class="ch-field">
          <span>原密码</span>
          <Input v-model="passwordForm.old_password" type="password" placeholder="输入当前密码" />
        </label>
        <label class="ch-field">
          <span>新密码</span>
          <Input v-model="passwordForm.new_password" type="password" placeholder="至少 6 位" />
        </label>
        <label class="ch-field">
          <span>确认新密码</span>
          <Input v-model="passwordForm.confirm_password" type="password" placeholder="再输一次" />
        </label>
        <div class="st-actions">
          <Button variant="primary" size="sm" :loading="saving" :success="savedPassword" @click="savePassword">
            修改密码
          </Button>
        </div>
      </div>
    </Card>

    <!-- 谷歌二步验证 -->
    <Card style="margin-top: var(--space-4);">
      <div class="st-tfa-header">
        <h2 class="st-section-title" style="margin-bottom: 0;">谷歌二步验证</h2>
        <Badge :variant="admin?.google2fa_enabled ? 'success' : 'info'" dot>
          {{ admin?.google2fa_enabled ? '已启用' : '未启用' }}
        </Badge>
      </div>

      <!-- 未启用：显示开启流程 -->
      <template v-if="!admin?.google2fa_enabled">
        <p class="st-info">使用 Google Authenticator 等应用扫描二维码，每次登录输入动态验证码。</p>

        <div v-if="tfaStep === 'idle'" class="st-actions">
          <Button variant="primary" size="sm" @click="generate2fa">开启谷歌验证</Button>
        </div>

        <div v-if="tfaStep === 'scan'" class="st-tfa-setup">
          <p class="st-info">1. 用 Google Authenticator 扫描下方二维码：</p>
          <div class="st-tfa-qr">
            <img
              v-if="tfaQrDisplay"
              :src="`https://api.qrserver.com/v1/create-qr-code/?size=200x200&data=${encodeURIComponent(tfaQrDisplay)}`"
              alt="2FA QR"
            />
            <p v-else class="st-info">二维码生成失败，请用下方密钥手动添加。</p>
          </div>
          <p class="st-info">2. 或手动输入密钥：<code class="st-tfa-secret">{{ tfaSecret }}</code></p>
          <p class="st-info">3. 输入应用显示的6位验证码：</p>
          <div class="st-tfa-verify">
            <Input v-model="tfaOtp" mono placeholder="000000" maxlength="6" />
            <Button variant="primary" size="sm" :loading="saving" @click="enable2fa">验证并启用</Button>
          </div>
          <Button variant="ghost" size="sm" @click="cancel2fa">取消</Button>
        </div>
      </template>

      <!-- 已启用：显示关闭 -->
      <template v-else>
        <p class="st-info">谷歌验证已启用，登录时需要输入动态验证码。如需关闭请输入当前密码。</p>
        <div class="st-tfa-disable">
          <Input v-model="tfaDisablePassword" type="password" placeholder="当前密码" />
          <Button variant="danger" size="sm" :loading="saving" @click="disable2fa">关闭谷歌验证</Button>
        </div>
      </template>
    </Card>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Settings.css"></style>
