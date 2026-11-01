<script setup>
/**
 * Invite — 邀请好友（二级页面）。
 * 我的邀请码（大字 + 复制 + 下次刷新倒计时）/ 规则说明 / 我的兑换码列表。
 * 数据来自 userApi.myInvite()（GET /api/invite/me）。
 */
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { userApi } from '@/api/user'
import UserIcon from '@/components/user/UserIcon.vue'
import ListGroup from '@/components/user/ListGroup.vue'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const loading = ref(true)
const invite = ref(null)
const redeemCodes = ref([])
const copied = ref(false)

// 倒计时每秒走一格
const now = ref(Date.now())
let ticker = null

const countdown = computed(() => {
  const at = invite.value?.next_refresh_at
  if (!at) return ''
  const diff = new Date(at).getTime() - now.value
  if (diff <= 0) return '即将刷新'
  const d = Math.floor(diff / 86400000)
  const h = Math.floor((diff % 86400000) / 3600000)
  const m = Math.floor((diff % 3600000) / 60000)
  const s = Math.floor((diff % 60000) / 1000)
  if (d > 0) return `${d} 天 ${h} 小时后刷新`
  if (h > 0) return `${h} 小时 ${m} 分后刷新`
  return `${m} 分 ${s} 秒后刷新`
})

const discountLabel = computed(() => {
  const d = Number(invite.value?.discount)
  if (!d) return ''
  return `${(d * 10).toFixed(1)} 折`
})

const refreshDate = computed(() => {
  const at = invite.value?.next_refresh_at
  if (!at) return '—'
  const d = new Date(at)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric' })
})

const codeDiscount = (d) => `${(Number(d) * 10).toFixed(1)} 折`

const statusLabel = (s) => ({
  active: '未使用',
  used_up: '已使用',
  expired: '已过期',
  refreshed: '已失效',
}[s] || s)

/**
 * 复制到剪贴板（三级降级）。
 * navigator.clipboard 只在安全上下文（https / localhost）且未被策略禁用时可用：
 * App WebView、iframe 内嵌、http 页面里它直接不存在或抛 NotAllowedError，
 * 所以失败时退到 textarea + document.execCommand('copy')。
 * 返回是否真的复制成功。
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

const copyCode = async () => {
  const code = invite.value?.code
  if (!code) return
  if (!(await copyText(code))) {
    toast.error('复制失败，请手动选择复制')
    return
  }
  copied.value = true
  toast.success('邀请码已复制')
  setTimeout(() => { copied.value = false }, 1500)
}

const load = async () => {
  try {
    const data = await userApi.myInvite()
    invite.value = data?.invite || null
    redeemCodes.value = data?.redeem_codes || []
  } catch (e) {
    invite.value = null
    redeemCodes.value = []
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  ticker = setInterval(() => { now.value = Date.now() }, 1000)
})
onUnmounted(() => { if (ticker) clearInterval(ticker) })
</script>

<template>
  <div class="iv uu-slide-up">
    <!-- 我的邀请码 -->
    <section v-if="loading" class="iv-hero iv-hero--skeleton">
      <div class="iv-skel iv-skel--sm"></div>
      <div class="iv-skel iv-skel--lg"></div>
      <div class="iv-skel iv-skel--sm"></div>
    </section>

    <section v-else-if="invite" class="iv-hero">
      <div class="iv-hero-top">
        <p class="iv-hero-label">我的邀请码</p>
        <span class="iv-hero-badge">{{ discountLabel }}</span>
      </div>

      <button type="button" class="iv-code uu-press" @click="copyCode">
        <span class="iv-code-text">{{ invite.code }}</span>
        <span class="iv-code-copy" :class="{ 'iv-code-copy--done': copied }">
          <UserIcon :name="copied ? 'check' : 'copy'" :size="16" />
          <span>{{ copied ? '已复制' : '复制' }}</span>
        </span>
      </button>

      <p v-if="!invite.enabled" class="iv-hero-off">邀请机制当前已关闭，邀请码暂不可用</p>

      <div class="iv-hero-foot">
        <div class="iv-hero-stat">
          <p class="iv-hero-stat-k">下次刷新</p>
          <p class="iv-hero-stat-v">{{ refreshDate }}</p>
        </div>
        <div class="iv-hero-stat">
          <p class="iv-hero-stat-k">剩余</p>
          <p class="iv-hero-stat-v">{{ countdown || '—' }}</p>
        </div>
        <div class="iv-hero-stat">
          <p class="iv-hero-stat-k">已邀请成功</p>
          <p class="iv-hero-stat-v">{{ invite.invited_count }} 人</p>
        </div>
      </div>
    </section>

    <section v-else class="iv-hero iv-hero--empty">
      <p class="iv-empty">邀请码加载失败，请稍后重试</p>
    </section>

    <!-- 规则说明 -->
    <ListGroup label="规则说明">
      <ul class="iv-rules">
        <li>邀请好友下单，好友享 {{ discountLabel || '折扣' }} 优惠</li>
        <li>每位用户一生只能使用一次邀请码，用过即永久生效</li>
        <li>不能使用自己的邀请码</li>
        <li>邀请码定期刷新，刷新后旧码立即失效，只认最新码</li>
        <li>好友支付成功后，你会自动获得一张兑换码</li>
      </ul>
    </ListGroup>

    <!-- 我的兑换码 -->
    <ListGroup label="我的兑换码">
      <template v-if="redeemCodes.length">
        <div v-for="c in redeemCodes" :key="c.code" class="iv-gift">
          <div class="iv-gift-head">
            <span class="iv-gift-code" :class="{ 'iv-gift-code--dead': c.status !== 'active' }">{{ c.code }}</span>
            <span class="iv-gift-status" :class="`iv-gift-status--${c.status}`">{{ statusLabel(c.status) }}</span>
          </div>
          <div class="iv-gift-meta">
            <span class="iv-gift-discount">{{ codeDiscount(c.discount) }}</span>
            <span class="iv-gift-exp">{{ c.expires_at ? `有效期至 ${new Date(c.expires_at).toLocaleDateString('zh-CN')}` : '永久有效' }}</span>
          </div>
          <p v-if="c.note" class="iv-gift-note">{{ c.note }}</p>
        </div>
      </template>
      <p v-else class="iv-empty iv-empty--inset">还没有兑换码，邀请好友下单即可获得</p>
    </ListGroup>
  </div>
</template>

<style scoped>
.iv { display: flex; flex-direction: column; gap: 20px; }

/* ===== 邀请码卡片 ===== */
.iv-hero {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 20px;
  border-radius: 24px;
  background: var(--mat-regular);
  overflow: hidden;
}
.iv-hero-top { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.iv-hero-label { margin: 0; font-size: 13px; font-weight: 600; color: var(--lbl3); letter-spacing: 0.04em; }
.iv-hero-badge {
  padding: 4px 10px;
  border-radius: 999px;
  background: var(--green-t);
  color: var(--green);
  font-size: 11px;
  font-weight: 700;
  white-space: nowrap;
}

.iv-code {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  width: 100%;
  padding: 14px 16px;
  border: 1px dashed var(--sep);
  border-radius: 16px;
  background: var(--blue-t);
  font-family: inherit;
  text-align: left;
}
.iv-code-text {
  font-family: 'SF Mono', 'Fira Code', ui-monospace, monospace;
  font-size: 22px;
  font-weight: 800;
  letter-spacing: 0.06em;
  color: var(--blue);
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.iv-code-copy {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
  padding: 6px 12px;
  border-radius: 999px;
  background: var(--blue);
  color: #fff;
  font-size: 12px;
  font-weight: 600;
  transition: background 0.2s;
}
.iv-code-copy--done { background: var(--green); }

.iv-hero-off {
  margin: 0;
  padding: 8px 12px;
  border-radius: 10px;
  background: var(--orange-t);
  color: var(--orange);
  font-size: 12px;
  line-height: 1.4;
}

.iv-hero-foot {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 8px;
  padding-top: 14px;
  border-top: 1px solid var(--sep);
}
.iv-hero-stat { min-width: 0; }
.iv-hero-stat-k { margin: 0; font-size: 11px; color: var(--lbl3); letter-spacing: 0.03em; }
.iv-hero-stat-v {
  margin: 3px 0 0;
  font-size: 13px;
  font-weight: 600;
  color: var(--lbl1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 骨架 */
.iv-hero--skeleton { gap: 12px; }
.iv-skel {
  border-radius: 8px;
  background: linear-gradient(90deg, var(--fill3) 25%, rgba(120, 120, 128, 0.2) 50%, var(--fill3) 75%);
  background-size: 200% 100%;
  animation: iv-shimmer 1.5s infinite;
}
.iv-skel--sm { height: 14px; width: 40%; }
.iv-skel--lg { height: 52px; width: 100%; }
@keyframes iv-shimmer { 0% { background-position: 200% 0; } 100% { background-position: -200% 0; } }

/* ===== 规则说明 ===== */
.iv-rules { list-style: none; margin: 0; padding: 14px 16px; display: flex; flex-direction: column; gap: 10px; }
.iv-rules li {
  position: relative;
  padding-left: 16px;
  font-size: 13px;
  line-height: 1.5;
  color: var(--lbl2);
}
.iv-rules li::before {
  content: '';
  position: absolute;
  left: 0;
  top: 7px;
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: var(--blue);
}

/* ===== 兑换码 ===== */
.iv-gift {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px 16px;
  border-bottom: 1px solid var(--sep);
}
.iv-gift:last-child { border-bottom: none; }
.iv-gift-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
.iv-gift-code {
  font-family: 'SF Mono', 'Fira Code', ui-monospace, monospace;
  font-size: 16px;
  font-weight: 700;
  letter-spacing: 0.05em;
  color: var(--lbl1);
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.iv-gift-code--dead { color: var(--lbl3); text-decoration: line-through; }
.iv-gift-status {
  flex-shrink: 0;
  padding: 3px 9px;
  border-radius: 999px;
  font-size: 11px;
  font-weight: 600;
  white-space: nowrap;
  background: var(--fill3);
  color: var(--lbl3);
}
.iv-gift-status--active { background: var(--green-t); color: var(--green); }
.iv-gift-status--expired { background: var(--orange-t); color: var(--orange); }
.iv-gift-meta { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.iv-gift-discount { font-size: 12px; font-weight: 700; color: var(--blue); }
.iv-gift-exp { font-size: 12px; color: var(--lbl3); }
.iv-gift-note {
  margin: 0;
  padding: 8px 12px;
  border-radius: 10px;
  background: var(--fill3);
  border-left: 3px solid var(--green);
  font-size: 12px;
  line-height: 1.5;
  color: var(--lbl2);
}

.iv-empty { margin: 0; padding: 24px 16px; text-align: center; font-size: 13px; color: var(--lbl3); }
.iv-empty--inset { padding: 20px 16px; }
.iv-hero--empty { padding: 32px 20px; }

@media (max-width: 480px) {
  .iv-code-text { font-size: 18px; }
  .iv-code { padding: 12px 14px; }
  .iv-hero { padding: 16px; }
  .iv-hero-stat-v { font-size: 12px; }
}
</style>
