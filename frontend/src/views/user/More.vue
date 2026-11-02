<script setup>
/**
 * 我的（新 UI）：资料卡 / 偏好设置 / 服务与支持 / 退出登录
 * 使用文档已改为全屏二级页（user-tutorials），此页只留入口。
 * 反馈链接、公告均为现有真实功能。
 */
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { useThemeStore } from '@/stores/theme'
import { userApi } from '@/api/user'
import UserIcon from '@/components/user/UserIcon.vue'
import ListGroup from '@/components/user/ListGroup.vue'
import ListRow from '@/components/user/ListRow.vue'
import BottomSheet from '@/components/user/BottomSheet.vue'

const auth = useAuthStore()
const siteConfig = useSiteConfigStore()
const theme = useThemeStore()
const router = useRouter()

const user = computed(() => auth.user || {})

// 是否买过代理：决定「我的代理」入口是否出现
// （商城可以关闭售卖，但买过的人随时要能看自己的凭据和 IP）
const hasProxy = ref(false)
userApi
  .proxyEntitlements()
  .then((list) => {
    hasProxy.value = Array.isArray(list) && list.length > 0
  })
  .catch(() => {
    hasProxy.value = false
  })

const avatarLetter = computed(() => (user.value.email?.[0] || 'U').toUpperCase())

// 公告：跳转全屏公告页
const openAnnouncement = () => {
  router.push({ name: 'user-announcement' })
}

const openFeedback = () => {
  const link = siteConfig.config.feedback_link
  if (link) window.open(link, '_blank')
}

const onLogout = () => {
  auth.logout()
  router.push({ name: 'user-login' })
}

/**
 * 复制到剪贴板（三级降级，与 Dashboard 同实现）。
 * navigator.clipboard 只在安全上下文可用；App WebView / iframe / http 页面里
 * 它直接不存在或抛 NotAllowedError，所以失败时退到 textarea + execCommand。
 */
const copyText = async (text) => {
  if (!text) return false
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch (e) {
      /* 被策略禁用：落到下方兜底 */
    }
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.top = '-9999px'
  document.body.appendChild(ta)
  ta.select()
  ta.setSelectionRange(0, text.length) // iOS Safari 只有 select() 选不中
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch (e) {
    ok = false
  }
  ta.remove()
  return ok
}

/* ── 重置订阅 ──────────────────────────────────────────────────
 * 换的是「订阅凭据」（users.token + users.uuid），**不动登录态**：
 * 当前页面继续可用、不退出登录；但下次用旧 token 登录会失败。
 *
 * 三步：点按钮 → 二次确认 → 结果弹窗（按格式给多个复制按钮 + 关闭）。
 * 结果弹窗里的地址**只认接口返回值**，绝不复用页面里可能还留着的旧 token，
 * 否则用户会复制到已经失效的旧链接。
 */
const resetOpen = ref(false)       // 确认弹层
const resetResultOpen = ref(false) // 结果弹层
const resetBusy = ref(false)
const resetError = ref('')
const resetToken = ref('')         // 新的订阅 token（只用它拼地址，不用 user.token）
const resetCopiedKey = ref('')     // 刚复制的是哪一个格式（'base' | 'clash' | 'singbox'）

/** 订阅格式开关：与 Dashboard 同一口径（站点配置决定显示哪几个按钮）。 */
const subFormats = ref({ clash: '0', singbox: '0' })
const loadSubFormats = async () => {
  try {
    const data = await userApi.siteConfig()
    subFormats.value = {
      clash: data?.sub_clash_enabled || '0',
      singbox: data?.sub_singbox_enabled || '0',
    }
  } catch (e) {
    // 静默失败：至少还保留「V2Ray 通用」那个按钮
  }
}

/**
 * 按格式拼订阅地址。**必须用 resetToken**（接口刚返回的新 token），
 * 不能用 auth.user.token —— 那个可能是页面加载时的旧值，复制到的是已失效的链接。
 */
const subUrlOf = (kind) => {
  if (!resetToken.value) return ''
  const base = `${location.origin}/api/sub/${resetToken.value}`
  if (kind === 'clash') return `${base}?clash=1`
  if (kind === 'singbox') return `${base}?singbox=1`
  return base
}

/** 要展示/复制的那几个格式（按站点开关生成，以后加格式只在这里加一项）。 */
const resetTargets = computed(() => {
  const list = [{ key: 'base', label: 'V2Ray / 通用', sub: 'v2rayN / NekoBox 等' }]
  if (subFormats.value.clash === '1') {
    list.push({ key: 'clash', label: 'Clash', sub: 'Clash Verge / Mihomo' })
  }
  if (subFormats.value.singbox === '1') {
    list.push({ key: 'singbox', label: 'Sing-box', sub: 'SFM / Sing-box 客户端' })
  }
  return list
})

const openResetConfirm = () => {
  resetError.value = ''
  resetOpen.value = true
}

const doReset = async () => {
  if (resetBusy.value) return
  resetBusy.value = true
  resetError.value = ''
  try {
    const res = await userApi.resetSubscription()
    // 只认接口返回的 token；拼地址统一走 subUrlOf()
    resetToken.value = res?.token || ''
    // 同步本地 user 状态，避免「订阅管理」卡片还显示旧链接
    if (auth.user && res?.token) auth.user.token = res.token
    resetCopiedKey.value = ''
    resetOpen.value = false
    resetResultOpen.value = true
  } catch (e) {
    // 限频（24h 内重复重置）也走这里：后端回的是 429 文案，直接展示
    resetError.value = e?.message || '重置失败，请稍后再试'
  } finally {
    resetBusy.value = false
  }
}

const copySubOf = async (kind) => {
  const url = subUrlOf(kind)
  if (!url) return
  const ok = await copyText(url)
  if (!ok) {
    resetError.value = '复制失败，请长按地址手动选择复制'
    return
  }
  resetCopiedKey.value = kind
  setTimeout(() => {
    if (resetCopiedKey.value === kind) resetCopiedKey.value = ''
  }, 2000)
}

const closeResetResult = () => {
  resetResultOpen.value = false
  resetToken.value = ''
  resetCopiedKey.value = ''
}


onMounted(() => {
  // 订阅格式开关（决定重置弹窗里给几个复制按钮）；失败不影响页面
  loadSubFormats()
})
</script>

<template>
  <div class="more uu-slide-up">
    <!-- 资料卡 -->
    <section class="mp uu-mat-regular">
      <div class="mp-head">
        <div class="mp-avatar">{{ avatarLetter }}</div>
        <div class="mp-id">
          <p class="mp-email">{{ user.email || '—' }}</p>
        </div>
        <span v-if="user.plan_name" class="mp-badge">{{ user.plan_name }}</span>
      </div>

      <!-- 原来的「套餐 / 到期 / 节点在线」三格已去掉：
           套餐在头像旁已有徽章、到期在控制台首页流量卡上有、节点在线首页也有 —— 这里重复。
           换成「重置订阅」入口：订阅链接泄露时用户自己要能处置（否则只能找管理员）。 -->
      <div class="mp-reset-row">
        <button type="button" class="mp-reset-btn uu-press" @click="openResetConfirm">
          <UserIcon name="refresh" :size="15" />
          <span>重置订阅</span>
        </button>
      </div>
    </section>

    <!-- 偏好设置 -->
    <ListGroup label="偏好设置">
      <ListRow
        :label="theme.isDark ? '切换浅色模式' : '切换深色模式'"
        :sub="theme.isDark ? '当前：深色' : '当前：浅色'"
        :icon-bg="theme.isDark ? 'var(--orange-t)' : 'var(--purple-t)'"
        :icon-color="theme.isDark ? 'var(--orange)' : 'var(--purple)'"
        last
        @press="theme.toggle()"
      >
        <template #icon>
          <UserIcon :name="theme.isDark ? 'sun' : 'moon'" :size="20" />
        </template>
        <template #right>
          <span class="mp-switch" :class="{ 'mp-switch--on': theme.isDark }">
            <span class="mp-switch-knob"></span>
          </span>
        </template>
      </ListRow>
    </ListGroup>

    <!-- 服务与支持 -->
    <ListGroup label="服务与支持">
      <ListRow
        label="我的订单"
        sub="查看历史订单与支付状态"
        @press="router.push({ name: 'user-orders' })"
      >
        <template #icon><UserIcon name="cart" :size="20" /></template>
      </ListRow>
      <!-- 买过代理才显示：商城关闭时侧栏没有入口，这里是唯一的入口 -->
      <ListRow
        v-if="hasProxy"
        label="我的代理"
        sub="账号密码 · IP 列表 · 到期时间"
        @press="router.push({ name: 'user-proxy-mine' })"
      >
        <template #icon><UserIcon name="globe" :size="20" /></template>
      </ListRow>
      <ListRow
        label="邀请好友"
        sub="我的邀请码 · 兑换码"
        @press="router.push({ name: 'user-invite' })"
      >
        <template #icon><UserIcon name="person" :size="20" /></template>
      </ListRow>
      <ListRow
        label="使用文档"
        sub="客户端配置 · 常见问题"
        @press="router.push({ name: 'user-tutorials' })"
      >
        <template #icon><UserIcon name="doc" :size="20" /></template>
      </ListRow>
      <ListRow
        v-if="siteConfig.config.feedback_link"
        label="反馈与联系"
        sub="问题反馈与联系我们"
        @press="openFeedback"
      >
        <template #icon><UserIcon name="mail" :size="20" /></template>
      </ListRow>
      <ListRow
        label="系统公告"
        sub="最新服务动态通知"
        last
        @press="openAnnouncement"
      >
        <template #icon><UserIcon name="bell" :size="20" /></template>
      </ListRow>
    </ListGroup>

    <!-- 退出登录 -->
    <ListGroup>
      <ListRow
        label="退出登录"
        danger
        last
        icon-bg="var(--red-t)"
        icon-color="var(--red)"
        @press="onLogout"
      >
        <template #icon><UserIcon name="logout" :size="20" /></template>
      </ListRow>
    </ListGroup>

    <!-- 重置订阅：二次确认 -->
    <BottomSheet v-model="resetOpen" title="重置订阅" max-width="420px">
      <div class="mp-sheet">
        <p class="mp-sheet-warn">
          <UserIcon name="warning" :size="18" />
          <span>重置后<strong>旧的订阅链接会立即失效</strong>，已导入旧链接的客户端会断开，需要把新链接重新导入。</span>
        </p>
        <ul class="mp-sheet-list">
          <li>账号、密码、套餐、已用流量、到期时间都不变；</li>
          <li>你当前已登录的页面<strong>不会被退出</strong>，可以继续使用；</li>
          <li>已用流量记录不会清零，只是换成新的订阅凭据。</li>
        </ul>
        <p class="mp-sheet-hint">只在订阅链接泄露、或需要作废已分享的链接时才用。24 小时内只能重置一次。</p>
        <p v-if="resetError" class="mp-sheet-err">{{ resetError }}</p>
      </div>
      <template #footer>
        <button type="button" class="mp-btn mp-btn--ghost uu-press" :disabled="resetBusy" @click="resetOpen = false">
          取消
        </button>
        <button type="button" class="mp-btn mp-btn--danger uu-press" :disabled="resetBusy" @click="doReset">
          {{ resetBusy ? '重置中…' : '确认重置' }}
        </button>
      </template>
    </BottomSheet>

    <!-- 重置订阅：结果（按格式给多个复制按钮 + 关闭） -->
    <BottomSheet v-model="resetResultOpen" title="重置成功" max-width="420px">
      <div class="mp-sheet">
        <p class="mp-sheet-ok">
          <UserIcon name="check" :size="18" />
          <span>旧链接已失效，下面是新订阅地址。选你的客户端复制：</span>
        </p>

        <div class="mp-sub-list">
          <div v-for="t in resetTargets" :key="t.key" class="mp-sub-item">
            <div class="mp-sub-meta">
              <p class="mp-sub-label">{{ t.label }}</p>
              <p class="mp-sub-hint">{{ t.sub }}</p>
            </div>
            <button
              type="button"
              class="mp-sub-copy uu-press"
              :class="{ 'mp-sub-copy--ok': resetCopiedKey === t.key }"
              @click="copySubOf(t.key)"
            >
              <UserIcon :name="resetCopiedKey === t.key ? 'check' : 'copy'" :size="15" />
              <span>{{ resetCopiedKey === t.key ? '已复制' : '复制' }}</span>
            </button>
          </div>
        </div>

        <p class="mp-sheet-hint">
          复制后到客户端里「从剪贴板导入」。当前页面不会退出登录；但下次登录请用新的订阅凭据。
        </p>
        <p v-if="resetError" class="mp-sheet-err">{{ resetError }}</p>
      </div>
      <template #footer>
        <button type="button" class="mp-btn mp-btn--primary uu-press" @click="closeResetResult">
          关闭
        </button>
      </template>
    </BottomSheet>
  </div>
</template>

<style scoped>
.more {
  display: flex;
  flex-direction: column;
  gap: 20px;
  padding-bottom: 8px;
}

/* ===== 资料卡 ===== */
.mp {
  border-radius: 24px;
  overflow: hidden;
}
.mp-head {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 20px;
  background: linear-gradient(135deg, rgba(10, 132, 255, 0.18) 0%, transparent 60%);
}
.mp-avatar {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 64px;
  height: 64px;
  border-radius: 16px;
  background: var(--blue);
  color: #fff;
  font-size: 26px;
  font-weight: 700;
  letter-spacing: -1px;
  flex-shrink: 0;
}
.mp-id {
  flex: 1;
  min-width: 0;
}
.mp-email {
  margin: 0;
  font-size: 17px;
  font-weight: 700;
  letter-spacing: -0.3px;
  color: var(--lbl1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.mp-badge {
  padding: 4px 10px;
  border-radius: 999px;
  background: var(--blue);
  color: #fff;
  font-size: 12px;
  font-weight: 700;
  white-space: nowrap;
  flex-shrink: 0;
}
/* 重置订阅入口：占整行、弱化样式（低频且危险的操作，不做成主按钮） */
.mp-reset-row {
  border-top: 1px solid var(--sep);
  padding: 10px 14px;
}
.mp-reset-btn {
  display: flex;
  width: 100%;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 9px 12px;
  border: 0;
  border-radius: 12px;
  background: transparent;
  color: var(--lbl3);
  font-size: 13px;
  cursor: pointer;
}
.mp-reset-btn:hover {
  background: var(--sep);
  color: var(--lbl2);
}

/* ===== 重置订阅弹层 ===== */
.mp-sheet {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.mp-sheet-warn {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  margin: 0;
  padding: 12px;
  border-radius: 12px;
  background: var(--red-t, rgba(255, 69, 58, 0.12));
  color: var(--lbl1);
  font-size: 13px;
  line-height: 1.5;
}
.mp-sheet-ok {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  margin: 0;
  font-size: 13px;
  line-height: 1.5;
  color: var(--lbl2);
}
.mp-sheet-list {
  margin: 0;
  padding-left: 18px;
  font-size: 13px;
  line-height: 1.7;
  color: var(--lbl2);
}
.mp-sheet-hint {
  margin: 0;
  font-size: 12px;
  line-height: 1.6;
  color: var(--lbl3);
}
.mp-sheet-err {
  margin: 0;
  font-size: 13px;
  color: var(--red, #ff453a);
}
/* 每个订阅格式一行：左侧名称 + 右侧复制按钮 */
.mp-sub-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.mp-sub-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 10px 12px;
  border-radius: 12px;
  background: var(--sep);
}
.mp-sub-meta {
  min-width: 0;
}
.mp-sub-label {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
  color: var(--lbl1);
}
.mp-sub-hint {
  margin: 2px 0 0;
  font-size: 11px;
  color: var(--lbl3);
}
.mp-sub-copy {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
  padding: 6px 12px;
  border: 0;
  border-radius: 9px;
  background: var(--blue, #0a84ff);
  color: #fff;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}
.mp-sub-copy--ok {
  background: var(--green, #30d158);
}
/* 弹层底部按钮 */
.mp-btn {
  flex: 1;
  padding: 11px 16px;
  border: 0;
  border-radius: 12px;
  font-size: 14px;
  font-weight: 600;
  cursor: pointer;
}
.mp-btn:disabled {
  opacity: 0.6;
}
.mp-btn--ghost {
  background: var(--sep);
  color: var(--lbl2);
}
.mp-btn--danger {
  background: var(--red, #ff453a);
  color: #fff;
}
.mp-btn--primary {
  background: var(--blue, #0a84ff);
  color: #fff;
}

/* ===== 主题开关 ===== */
.mp-switch {
  display: flex;
  align-items: center;
  width: 48px;
  height: 28px;
  padding: 2px 3px;
  border-radius: 999px;
  background: var(--blue);
  justify-content: flex-start;
  pointer-events: none;
}
.mp-switch--on {
  justify-content: flex-end;
}
.mp-switch-knob {
  width: 23px;
  height: 23px;
  border-radius: 999px;
  background: #fff;
  box-shadow: 0 2px 4px rgba(0, 0, 0, 0.3);
}

@media (max-width: 480px) {
  .mp-head {
    flex-wrap: wrap;
  }
}
</style>
