<script setup>
/**
 * Shop — 套餐列表 + 订单管理
 */
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import Icon from '@/components/ui/Icon.vue'
import Modal from '@/components/ui/Modal.vue'
import UserIcon from '@/components/user/UserIcon.vue'
import { useAuthStore } from '@/stores/auth'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'

const auth = useAuthStore()
const toast = useToast()
const router = useRouter()

const plans = ref([])
const paymentMethods = ref([])
const loading = ref(true)
const selectedPlan = ref(null)
const selectedPayment = ref(null)
const creatingOrder = ref(false)
const pollingOrder = ref(null)
const buyModalOpen = ref(false)
// 移动端套餐布局：宫格（默认）/ 列表
const planView = ref('grid')
const VIEW_KEY = 'shop_plan_view'
try {
  const saved = localStorage.getItem(VIEW_KEY)
  if (saved === 'grid' || saved === 'list') planView.value = saved
} catch (e) { /* 隐私模式忽略 */ }
const setPlanView = (v) => {
  planView.value = v
  try { localStorage.setItem(VIEW_KEY, v) } catch (e) { /* 忽略 */ }
}

const user = computed(() => auth.user || {})

const fmtBytes = (b) => {
  if (!b || b < 0 || !Number.isFinite(b)) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB']
  const raw = Math.floor(Math.log(b) / Math.log(1024))
  const i = Math.max(0, Math.min(raw, u.length - 1))
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)} ${u[i]}`
}
const fmtPrice = (p) => { if (!p && p !== 0) return '—'; return p > 0 ? `¥${p}` : '免费' }

const loadPlans = async () => { try { plans.value = await userApi.plans() } catch (e) { plans.value = [] } }
const loadPaymentMethods = async () => { try { paymentMethods.value = await userApi.paymentMethods() } catch (e) { paymentMethods.value = [] } }

const openBuy = async (p) => {
  selectedPlan.value = p
  selectedPayment.value = null
  resetDiscount()
  if (!paymentMethods.value.length) await loadPaymentMethods()
  if (paymentMethods.value.length) selectedPayment.value = paymentMethods.value[0].id
  buyModalOpen.value = true
}

/* ===== 折扣码（邀请码 / 兑换码）：一个输入框，只能填一个码，不存在叠加 ===== */
const discountCode = ref('')
const discountChecking = ref(false)
const discountResult = ref(null)
let discountTimer = null

const resetDiscount = () => {
  discountCode.value = ''
  discountResult.value = null
  discountChecking.value = false
  if (discountTimer) { clearTimeout(discountTimer); discountTimer = null }
}

// 输入后防抖 400ms 调 /api/discount/check（仅校验，不占用次数）
const onDiscountInput = () => {
  if (discountTimer) clearTimeout(discountTimer)
  discountResult.value = null
  const code = discountCode.value.trim()
  if (!code || !selectedPlan.value || selectedPlan.value.price <= 0) {
    discountChecking.value = false
    return
  }
  discountChecking.value = true
  discountTimer = setTimeout(async () => {
    try {
      discountResult.value = await userApi.checkDiscount(code, selectedPlan.value.id)
    } catch (e) {
      discountResult.value = { usable: false, message: e.message || '优惠码校验失败' }
    } finally {
      discountChecking.value = false
    }
  }, 400)
}

const discountUsable = computed(() => !!discountResult.value?.usable)

// 价格主视觉 / 底部按钮金额都跟着折扣走
const priceInfo = computed(() => {
  const original = Number(selectedPlan.value?.price || 0)
  if (discountUsable.value && original > 0) {
    return { final: Number(discountResult.value.final_amount), original, discounted: true }
  }
  return { final: original, original, discounted: false }
})

const discountHint = computed(() => discountResult.value?.message || '')
const discountNote = computed(() => (discountUsable.value ? discountResult.value?.note : '') || '')

const createOrder = async () => {
  if (!selectedPlan.value) { toast.error('请选择套餐'); return }
  creatingOrder.value = true
  try {
    const result = await userApi.createOrder(selectedPlan.value.id, selectedPayment.value, discountCode.value.trim())
    if (result.status === 'paid') {
      toast.success('购买成功（免费套餐）')
      buyModalOpen.value = false
      selectedPlan.value = null
      resetDiscount()
      await auth.fetchMe()
      return
    }
    if (result.pay_url) {
      buyModalOpen.value = false
      window.open(result.pay_url, '_blank')
      toast.success('订单已创建，请完成支付')
      pollingOrder.value = result.order_no
      startPolling(result.order_no)
    }
  } catch (e) {
    toast.error(e.message || '创建订单失败')
  } finally {
    creatingOrder.value = false
  }
}

const startPolling = (orderNo) => {
  let count = 0
  const timer = setInterval(async () => {
    count++
    try {
      const status = await userApi.orderStatus(orderNo)
      if (status.status === 'paid') {
        clearInterval(timer)
        pollingOrder.value = null
        toast.success('支付成功！')
        await auth.fetchMe()
        return
      }
    } catch (e) {}
    if (count >= 60) { clearInterval(timer); pollingOrder.value = null }
  }, 5000)
}

onMounted(async () => {
  await Promise.all([loadPlans(), loadPaymentMethods()])
  loading.value = false
})
</script>

<template>
  <div class="shop">
    <!-- 操作栏 -->
    <div class="shop-actions">
      <h2 class="shop-title">套餐</h2>
      <div class="shop-actions-right">
        <!-- 布局切换：仅移动端显示 -->
        <div class="shop-view-switch" role="group" aria-label="布局切换">
          <button
            type="button"
            class="shop-view-btn"
            :class="{ 'shop-view-btn--on': planView === 'grid' }"
            aria-label="宫格"
            @click="setPlanView('grid')"
          >
            <span class="shop-view-glyph shop-view-glyph--grid"></span>
          </button>
          <button
            type="button"
            class="shop-view-btn"
            :class="{ 'shop-view-btn--on': planView === 'list' }"
            aria-label="列表"
            @click="setPlanView('list')"
          >
            <span class="shop-view-glyph shop-view-glyph--list"></span>
          </button>
        </div>
        <button type="button" class="shop-orders-btn uu-press" @click="router.push({ name: 'user-orders' })">
          <UserIcon name="cart" :size="16" />
          <span>订单</span>
        </button>
      </div>
    </div>

    <!-- 套餐列表 -->
    <div v-if="loading" class="shop-plans">
      <div v-for="i in 3" :key="i" class="shop-skeleton">
        <div class="shop-skeleton-inner">
          <div class="shop-skeleton-bar shop-skeleton-bar--sm"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--xl"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--sm"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--line"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--line"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--line"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--btn"></div>
        </div>
      </div>
    </div>
    <div v-else class="shop-plans" :class="`shop-plans--${planView}`">
      <div v-for="(p, idx) in plans" :key="p.id" class="shop-plan-card" :class="{ 'shop-plan-card--highlight': idx === 1 }" @click="openBuy(p)">
        <div class="shop-plan-inner">
          <h3 class="shop-plan-name">{{ p.name }}</h3>
          <div class="shop-plan-price">
            <span class="shop-plan-currency">¥</span>
            <span class="shop-plan-amount">{{ p.price || 0 }}</span>
            <span v-if="p.type === 'period'" class="shop-plan-unit">{{ p.months === 1 ? '/月' : p.months === 3 ? '/季' : p.months === 12 ? '/年' : '/' + p.months + '个月' }}</span>
          </div>
          <p v-if="p.type === 'period' && p.months > 1" class="shop-plan-equiv">{{ (p.price / p.months).toFixed(1) }}元/月</p>
          <ul class="shop-plan-features">
            <template v-if="!p.include_local && p.third_party_sub_ids?.length">
              <li>{{ p.months }} 个月有效期</li>
              <li>高速线路可用</li>
              <li>不限速</li>
            </template>
            <template v-else-if="p.type === 'period'">
              <li>{{ p.months }} 个月有效期</li>
              <li>每月 {{ fmtBytes(p.monthly_traffic) }}</li>
              <li>流量按月重置</li>
              <li>全节点可用</li>
              <li>不限速</li>
            </template>
            <template v-else>
              <li>总量 {{ fmtBytes(p.total_traffic) }}</li>
              <li>永久有效</li>
              <li>用完为止</li>
              <li>全节点可用</li>
              <li>不限速</li>
            </template>
          </ul>
          <button class="shop-plan-btn" :class="idx === 1 ? 'shop-plan-btn--dark' : 'shop-plan-btn--light'" @click.stop="openBuy(p)">
            选择 {{ p.name }}
          </button>
        </div>
      </div>
      <div v-if="!plans.length && !loading" class="shop-empty">
        <div class="shop-empty-icon"><Icon name="package" :size="32" /></div>
        <p class="shop-empty-title">暂无套餐</p>
        <p class="shop-empty-desc">管理员尚未发布套餐，请稍后再来</p>
      </div>
    </div>

    <!-- 购买弹窗 -->
    <Modal v-model="buyModalOpen" :title="selectedPlan ? `购买 ${selectedPlan.name}` : '购买套餐'" width="460px">
      <!-- uu-root：Modal 被 Teleport 到 body，需要在此重新挂上用户端 token 作用域 -->
      <div v-if="selectedPlan" class="buy uu-root">
        <!-- 价格主视觉 -->
        <div class="buy-hero">
          <div class="buy-hero-main">
            <span class="buy-hero-price">{{ fmtPrice(priceInfo.final) }}</span>
            <span v-if="priceInfo.discounted" class="buy-hero-old">¥{{ priceInfo.original }}</span>
            <span class="buy-hero-unit">{{ selectedPlan.type === 'period' ? `/${selectedPlan.months}个月` : '一次性' }}</span>
          </div>
          <span class="buy-hero-tag" :class="{ 'buy-hero-tag--on': priceInfo.discounted }">
            {{ priceInfo.discounted ? `已优惠 ¥${(priceInfo.original - priceInfo.final).toFixed(2)}` : (selectedPlan.type === 'period' ? '周期套餐' : '总量套餐') }}
          </span>
        </div>

        <!-- 规格 -->
        <div class="buy-specs">
          <template v-if="!selectedPlan.include_local && selectedPlan.third_party_sub_ids?.length">
            <div class="buy-spec">
              <span class="buy-spec-k">线路</span>
              <span class="buy-spec-v">高速线路可用</span>
              <span class="buy-spec-hint">不限速</span>
            </div>
            <div class="buy-spec">
              <span class="buy-spec-k">有效期</span>
              <span class="buy-spec-v">{{ selectedPlan.months }} 个月</span>
              <span class="buy-spec-hint">到期后需续费</span>
            </div>
          </template>
          <template v-else>
            <div class="buy-spec">
              <span class="buy-spec-k">流量</span>
              <span class="buy-spec-v">{{ selectedPlan.type === 'period' ? fmtBytes(selectedPlan.monthly_traffic) : fmtBytes(selectedPlan.total_traffic) }}</span>
              <span class="buy-spec-hint">{{ selectedPlan.type === 'period' ? '每月重置' : '用完为止' }}</span>
            </div>
            <div class="buy-spec">
              <span class="buy-spec-k">有效期</span>
              <span class="buy-spec-v">{{ selectedPlan.type === 'period' ? `${selectedPlan.months} 个月` : '永久' }}</span>
              <span class="buy-spec-hint">{{ selectedPlan.type === 'period' ? '到期后需续费' : '不限时间' }}</span>
            </div>
          </template>
        </div>

        <p v-if="selectedPlan.type === 'period'" class="buy-note">
          重复购买同一套餐时，有效期将从本次购买重新计算，剩余天数不累计。
        </p>

        <!-- 邀请码 / 兑换码：一个输入框，只能填一个码 -->
        <div v-if="selectedPlan.price > 0" class="buy-disc">
          <p class="buy-pay-title">邀请码 / 兑换码</p>
          <div
            class="buy-disc-box"
            :class="{
              'buy-disc-box--ok': discountUsable,
              'buy-disc-box--err': discountResult && !discountUsable,
            }"
          >
            <input
              v-model="discountCode"
              class="buy-disc-input"
              type="text"
              autocomplete="off"
              spellcheck="false"
              placeholder="填写邀请码或兑换码"
              @input="onDiscountInput"
            />
            <span v-if="discountChecking" class="buy-disc-spin"><UserIcon name="refresh" :size="16" spin /></span>
            <span v-else-if="discountUsable" class="buy-disc-ok"><UserIcon name="checkCircle" :size="18" /></span>
          </div>
          <p v-if="discountHint" class="buy-disc-msg" :class="discountUsable ? 'buy-disc-msg--ok' : 'buy-disc-msg--err'">
            {{ discountHint }}
          </p>
          <p v-if="discountNote" class="buy-disc-note">{{ discountNote }}</p>
          <p v-if="discountUsable" class="buy-disc-price">
            折扣 {{ (discountResult.discount * 10).toFixed(1) }} 折 · 折后 <b>¥{{ priceInfo.final.toFixed(2) }}</b>
          </p>
        </div>

        <!-- 支付方式 -->
        <div v-if="selectedPlan.price > 0" class="buy-pay">
          <p class="buy-pay-title">支付方式</p>
          <div v-if="paymentMethods.length" class="buy-pay-list">
            <label
              v-for="m in paymentMethods"
              :key="m.id"
              class="buy-pay-item"
              :class="{ 'buy-pay-item--on': selectedPayment === m.id }"
            >
              <input type="radio" :value="m.id" v-model="selectedPayment" />
              <span class="buy-pay-name">{{ m.name }}</span>
              <span class="buy-pay-check">
                <Icon name="check" :size="13" />
              </span>
            </label>
          </div>
          <p v-else class="buy-pay-empty">暂无可用支付方式</p>
        </div>
      </div>

      <template #footer>
        <div class="buy-foot uu-root">
          <button type="button" class="buy-btn" @click="buyModalOpen = false">取消</button>
          <button
            type="button"
            class="buy-btn buy-btn--primary"
            :disabled="creatingOrder"
            @click="createOrder"
          >
            {{ creatingOrder ? '处理中...' : (selectedPlan && selectedPlan.price > 0 ? `支付 ${fmtPrice(priceInfo.final)}` : '免费领取') }}
          </button>
        </div>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.shop { padding-top: 8px; display: flex; flex-direction: column; gap: 20px; }

.shop-actions { display: flex; align-items: center; justify-content: space-between; }
.shop-title { margin: 0; font-size: 18px; font-weight: 800; color: #1d1d1f; }
.shop-actions-right { display: flex; align-items: center; gap: 8px; }

/* 布局切换（仅移动端显示，桌面隐藏） */
.shop-view-switch {
  display: none;
  align-items: center;
  gap: 2px;
  height: 32px;
  padding: 2px;
  border-radius: 999px;
  background: var(--fill3);
}
.shop-view-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 30px;
  height: 28px;
  border: none;
  border-radius: 999px;
  background: transparent;
  color: var(--lbl4);
  cursor: pointer;
  transition: background 0.25s, color 0.25s;
  -webkit-tap-highlight-color: transparent;
}
.shop-view-btn--on { background: #fff; color: var(--lbl1); box-shadow: 0 1px 3px rgba(0,0,0,0.1); }
.shop-view-glyph { display: block; width: 14px; height: 14px; }
.shop-view-glyph--grid {
  background-image:
    linear-gradient(currentColor 0 0), linear-gradient(currentColor 0 0),
    linear-gradient(currentColor 0 0), linear-gradient(currentColor 0 0);
  background-size: 6px 6px;
  background-position: 0 0, 8px 0, 0 8px, 8px 8px;
  background-repeat: no-repeat;
  border-radius: 1px;
}
.shop-view-glyph--list {
  background-image:
    linear-gradient(currentColor 0 0), linear-gradient(currentColor 0 0),
    linear-gradient(currentColor 0 0);
  background-size: 14px 3px;
  background-position: 0 0, 0 5.5px, 0 11px;
  background-repeat: no-repeat;
}

.shop-orders-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 32px;
  padding: 0 14px;
  border: none;
  border-radius: 999px;
  background: var(--blue-t);
  color: var(--blue);
  font-size: 13px;
  font-weight: 600;
  font-family: inherit;
}

/* 骨架屏 */
.shop-skeleton { display: flex; border-radius: 24px; padding: 5px; background: #fff; border: 1px solid rgba(0,0,0,0.06); }
.shop-skeleton-inner { border-radius: 19px; padding: 36px 28px 28px; display: flex; flex-direction: column; gap: 16px; width: 100%; background: #fafafa; }
.shop-skeleton-bar { border-radius: 6px; background: linear-gradient(90deg, #f0f0f0 25%, #e8e8e8 50%, #f0f0f0 75%); background-size: 200% 100%; animation: shimmer 1.5s infinite; }
.shop-skeleton-bar--sm { width: 40%; height: 14px; }
.shop-skeleton-bar--xl { width: 60%; height: 48px; }
.shop-skeleton-bar--line { width: 80%; height: 12px; }
.shop-skeleton-bar--btn { width: 100%; height: 48px; margin-top: auto; border-radius: 9999px; }
@keyframes shimmer { 0% { background-position: 200% 0; } 100% { background-position: -200% 0; } }

/* 套餐卡片 */
.shop-plans { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 20px; align-items: stretch; }
.shop-plan-card { display: flex; cursor: pointer; border-radius: 24px; padding: 5px; transition: all 0.6s cubic-bezier(0.16, 1, 0.3, 1); background: #fff; border: 1px solid rgba(0,0,0,0.06); }
.shop-plan-card:hover { box-shadow: 0 12px 60px -12px rgba(0,0,0,0.14); transform: translateY(-4px); }
.shop-plan-card--highlight { background: #1d1d1f; border-color: #333; box-shadow: 0 12px 60px -12px rgba(0,0,0,0.3); }
.shop-plan-card--highlight:hover { box-shadow: 0 16px 70px -12px rgba(0,0,0,0.4); }
.shop-plan-inner { border-radius: 19px; padding: 36px 28px 28px; display: flex; flex-direction: column; gap: 16px; width: 100%; background: #fafafa; box-shadow: inset 0 1px 1px rgba(255,255,255,0.8); }
.shop-plan-card--highlight .shop-plan-inner { background: transparent; }
.shop-plan-name { font-size: 14px; font-weight: 700; margin: 0; letter-spacing: 0.05em; text-transform: uppercase; color: #999; }
.shop-plan-card--highlight .shop-plan-name { color: rgba(255,255,255,0.6); }
.shop-plan-price { display: flex; align-items: baseline; gap: 4px; margin: 4px 0 8px; }
.shop-plan-currency { font-size: 18px; font-weight: 700; color: #999; }
.shop-plan-card--highlight .shop-plan-currency { color: rgba(255,255,255,0.6); }
.shop-plan-amount { font-size: 3.5rem; font-weight: 800; letter-spacing: -0.04em; line-height: 1; font-variant-numeric: tabular-nums; color: #1d1d1f; }
.shop-plan-card--highlight .shop-plan-amount { color: #fff; }
.shop-plan-unit { font-size: 14px; font-weight: 500; color: #999; }
.shop-plan-card--highlight .shop-plan-unit { color: rgba(255,255,255,0.5); }
.shop-plan-equiv { font-size: 12px; color: #999; margin: 2px 0 0; }
.shop-plan-card--highlight .shop-plan-equiv { color: rgba(255,255,255,0.6); }
.shop-plan-features { list-style: none; padding: 0; margin: 0; display: flex; flex-direction: column; gap: 10px; flex: 1; padding-top: 16px; border-top: 1px solid rgba(0,0,0,0.06); }
.shop-plan-card--highlight .shop-plan-features { border-color: rgba(255,255,255,0.1); }
.shop-plan-features li { font-size: 13px; font-weight: 500; padding-left: 16px; position: relative; color: #1d1d1f; }
.shop-plan-features li::before { content: ''; position: absolute; left: 0; top: 6px; width: 5px; height: 5px; border-radius: 50%; background: #1d1d1f; }
.shop-plan-card--highlight .shop-plan-features li { color: rgba(255,255,255,0.8); }
.shop-plan-card--highlight .shop-plan-features li::before { background: rgba(255,255,255,0.5); }
.shop-plan-btn { width: 100%; padding: 14px 24px; border-radius: 9999px; font-size: 14px; font-weight: 600; border: none; cursor: pointer; transition: all 0.4s cubic-bezier(0.16, 1, 0.3, 1); font-family: inherit; margin-top: auto; }
.shop-plan-btn:active { transform: scale(0.98); }
.shop-plan-btn--light { background: #f0f0f0; color: #1d1d1f; border: 1px solid rgba(0,0,0,0.08); }
.shop-plan-btn--light:hover { background: #e5e5e5; }
.shop-plan-btn--dark { background: #fff; color: #1d1d1f; }
.shop-plan-btn--dark:hover { box-shadow: 0 4px 20px rgba(255,255,255,0.2); }

/* 购买弹窗 */
.shop-empty { text-align: center; padding: 48px 24px; }
.shop-empty-icon { display: inline-flex; align-items: center; justify-content: center; width: 64px; height: 64px; border-radius: 16px; background: #f0f0f0; color: #999; margin-bottom: 16px; }
.shop-empty-title { font-size: 15px; font-weight: 600; color: #1d1d1f; margin: 0 0 6px; }
.shop-empty-desc { font-size: 13px; color: #999; margin: 0; }
html.dark .shop-empty-icon { background: rgba(255,255,255,0.06); color: #666; }
html.dark .shop-empty-title { color: #e5e5e5; }
html.dark .shop-empty-desc { color: #666; }
/* ===== 购买弹窗（沿用用户端 token，浅/暗色自适应） ===== */
.buy { display: flex; flex-direction: column; gap: 16px; }

/* 价格主视觉 */
.buy-hero {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 16px 18px;
  border-radius: 16px;
  background: var(--fill3);
  border: 1px solid var(--sep);
}
.buy-hero-main { display: flex; align-items: baseline; gap: 6px; min-width: 0; }
.buy-hero-price {
  font-size: 30px;
  font-weight: 800;
  letter-spacing: -0.03em;
  line-height: 1.1;
  color: var(--lbl1);
  font-variant-numeric: tabular-nums;
}
.buy-hero-old {
  font-size: 14px;
  font-weight: 500;
  color: var(--lbl3);
  text-decoration: line-through;
  font-variant-numeric: tabular-nums;
}
.buy-hero-unit { font-size: 13px; font-weight: 500; color: var(--lbl3); }
.buy-hero-tag {
  flex-shrink: 0;
  padding: 4px 10px;
  border-radius: 999px;
  background: var(--blue-t);
  color: var(--blue);
  font-size: 11px;
  font-weight: 600;
}
.buy-hero-tag--on { background: var(--green-t); color: var(--green); }

/* 邀请码 / 兑换码 */
.buy-disc { display: flex; flex-direction: column; gap: 8px; }
.buy-disc-box {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 0 14px;
  height: 44px;
  border-radius: 14px;
  border: 1.5px solid var(--sep);
  background: var(--fill3);
  transition: border-color 0.2s, background 0.2s;
}
.buy-disc-box--ok { border-color: var(--green); background: var(--green-t); }
.buy-disc-box--err { border-color: var(--red); background: var(--red-t); }
.buy-disc-input {
  flex: 1;
  min-width: 0;
  height: 100%;
  border: none;
  outline: none;
  background: transparent;
  color: var(--lbl1);
  font-family: inherit;
  font-size: 14px;
  font-weight: 600;
  letter-spacing: 0.04em;
  text-transform: uppercase;
}
.buy-disc-input::placeholder {
  color: var(--lbl4);
  font-weight: 400;
  letter-spacing: 0;
  text-transform: none;
}
.buy-disc-ok { display: inline-flex; color: var(--green); flex-shrink: 0; }
.buy-disc-spin { display: inline-flex; color: var(--lbl3); flex-shrink: 0; }
.buy-disc-msg { margin: 0; font-size: 12px; line-height: 1.4; }
.buy-disc-msg--ok { color: var(--green); }
.buy-disc-msg--err { color: var(--red); }
.buy-disc-note {
  margin: 0;
  padding: 8px 12px;
  border-radius: 10px;
  background: var(--fill3);
  border-left: 3px solid var(--green);
  font-size: 12px;
  line-height: 1.5;
  color: var(--lbl2);
}
.buy-disc-price {
  margin: 0;
  font-size: 12px;
  color: var(--lbl3);
  font-variant-numeric: tabular-nums;
}
.buy-disc-price b { color: var(--green); font-size: 13px; }

/* 规格：两栏等高 */
.buy-specs { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
.buy-spec {
  display: flex;
  flex-direction: column;
  gap: 3px;
  padding: 12px 14px;
  border-radius: 14px;
  background: var(--fill3);
  border: 1px solid var(--sep);
  min-width: 0;
}
.buy-spec-k { font-size: 11px; color: var(--lbl3); }
.buy-spec-v {
  font-size: 16px;
  font-weight: 700;
  color: var(--lbl1);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.buy-spec-hint { font-size: 11px; color: var(--lbl4); }

/* 重复购买说明（周期套餐） */
.buy-note { margin: 10px 2px 0; font-size: 12px; line-height: 1.5; color: var(--lbl3); }

/* 支付方式 */
.buy-pay { display: flex; flex-direction: column; gap: 8px; }
.buy-pay-title { margin: 0; font-size: 12px; font-weight: 600; color: var(--lbl3); }
.buy-pay-list { display: flex; flex-direction: column; gap: 8px; }
.buy-pay-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 14px;
  border-radius: 14px;
  border: 1.5px solid var(--sep);
  background: var(--fill3);
  cursor: pointer;
  transition: border-color 0.2s, background 0.2s;
  -webkit-tap-highlight-color: transparent;
}
.buy-pay-item input { position: absolute; opacity: 0; width: 0; height: 0; }
.buy-pay-name { flex: 1; font-size: 14px; font-weight: 600; color: var(--lbl1); }
.buy-pay-check {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 20px;
  height: 20px;
  border-radius: 50%;
  border: 1.5px solid var(--lbl4);
  color: transparent;
  flex-shrink: 0;
  transition: background 0.2s, border-color 0.2s, color 0.2s;
}
.buy-pay-item--on { border-color: var(--blue); background: var(--blue-t); }
.buy-pay-item--on .buy-pay-check {
  background: var(--blue);
  border-color: var(--blue);
  color: #fff;
}
.buy-pay-empty { margin: 0; font-size: 13px; color: var(--lbl3); }

/* 底部按钮 */
.buy-foot { display: flex; gap: 10px; width: 100%; }
.buy-btn {
  flex: 1;
  padding: 12px 20px;
  border-radius: 999px;
  font-size: 15px;
  font-weight: 600;
  font-family: inherit;
  cursor: pointer;
  transition: opacity 0.2s, background 0.2s;
  border: 1px solid var(--sep);
  background: transparent;
  color: var(--lbl1);
}
.buy-btn:hover { background: var(--fill3); }
.buy-btn--primary {
  border-color: transparent;
  background: var(--blue);
  color: #fff;
  font-weight: 700;
}
.buy-btn--primary:hover { opacity: 0.88; background: var(--blue); }
.buy-btn:disabled { opacity: 0.5; cursor: not-allowed; }

/* 响应式 */
@media (max-width: 480px) {
  .shop-plan-detail { flex-direction: column; gap: 4px; }
  .shop-plans { gap: 14px; }
  .shop-plan-inner { padding: 24px 18px 20px; gap: 10px; }
  .shop-plan-name { font-size: 12px; }
  .shop-plan-amount { font-size: 2.5rem; }
  .shop-plan-currency { font-size: 14px; }
  .shop-plan-unit { font-size: 12px; }
  .shop-plan-equiv { font-size: 11px; }
  .shop-plan-features { gap: 6px; padding-top: 12px; }
  .shop-plan-features li { font-size: 12px; }
  .shop-plan-btn { padding: 12px 20px; font-size: 13px; }
}

/* ===== 移动端布局切换（≤768px 生效，桌面完全不受影响）===== */
@media (max-width: 768px) {
  .shop-view-switch { display: inline-flex; }

  /* 宫格（默认）：2 列，信息展示完整（5 行特性全保留） */
  .shop-plans--grid { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px; }
  .shop-plans--grid .shop-plan-card { border-radius: 18px; padding: 4px; }
  .shop-plans--grid .shop-plan-card:hover { transform: none; box-shadow: none; }
  .shop-plans--grid .shop-plan-card:active { transform: scale(0.98); }
  .shop-plans--grid .shop-plan-inner { border-radius: 14px; padding: 16px 12px 14px; gap: 10px; }
  .shop-plans--grid .shop-plan-name { font-size: 11px; letter-spacing: 0.03em; }
  .shop-plans--grid .shop-plan-price { flex-wrap: wrap; gap: 2px; margin: 2px 0 0; }
  .shop-plans--grid .shop-plan-currency { font-size: 13px; }
  .shop-plans--grid .shop-plan-amount { font-size: 2rem; letter-spacing: -0.03em; }
  .shop-plans--grid .shop-plan-unit { font-size: 12px; }
  .shop-plans--grid .shop-plan-equiv { font-size: 11px; }
  /* 2 列下内容宽 109-144px，「每月 204.80 MB」可单行放下，特性行完整保留 */
  .shop-plans--grid .shop-plan-features { gap: 6px; padding-top: 12px; }
  .shop-plans--grid .shop-plan-features li { font-size: 11px; line-height: 1.35; padding-left: 12px; }
  .shop-plans--grid .shop-plan-features li::before { width: 4px; height: 4px; top: 6px; }
  .shop-plans--grid .shop-plan-btn { padding: 10px 10px; font-size: 12px; border-radius: 10px; }

  /* 列表：与桌面同款单列大卡 */
  .shop-plans--list { grid-template-columns: minmax(0, 1fr); gap: 14px; }
}

/* 移动端弹窗：内边距与字号收紧，保证一屏看全 */
@media (max-width: 480px) {
  .buy { gap: 12px; }
  .buy-hero { padding: 14px 16px; }
  .buy-hero-price { font-size: 26px; }
  .buy-specs { gap: 8px; }
  .buy-spec { padding: 10px 12px; }
  .buy-spec-v { font-size: 15px; }
  .buy-disc { gap: 6px; }
  .buy-disc-box { height: 42px; }
  .buy-pay-item { padding: 11px 14px; }
  .buy-btn { padding: 12px 16px; font-size: 15px; }
}

/* 暗色模式：布局切换 */
html.dark .shop-view-switch { background: rgba(255,255,255,0.08); }
html.dark .shop-view-btn--on { background: rgba(255,255,255,0.16); color: #fff; box-shadow: none; }
html.dark .shop-view-glyph--grid,
html.dark .shop-view-glyph--list { color: inherit; }

/* 暗色模式 */
html.dark .shop-title { color: #fff; }
html.dark .shop-plan-card { background: rgba(20,20,20,0.8); border-color: rgba(255,255,255,0.06); }
html.dark .shop-plan-card:hover { border-color: rgba(255,255,255,0.1); }
html.dark .shop-plan-card--highlight { background: #fff; border-color: #fff; }
html.dark .shop-plan-inner { background: rgba(255,255,255,0.04); }
html.dark .shop-plan-card--highlight .shop-plan-inner { background: transparent; }
html.dark .shop-plan-name { color: #888; }
html.dark .shop-plan-card--highlight .shop-plan-name { color: rgba(0,0,0,0.5); }
html.dark .shop-plan-currency { color: #888; }
html.dark .shop-plan-card--highlight .shop-plan-currency { color: rgba(0,0,0,0.5); }
html.dark .shop-plan-amount { color: #fff; }
html.dark .shop-plan-card--highlight .shop-plan-amount { color: #1d1d1f; }
html.dark .shop-plan-unit { color: #888; }
html.dark .shop-plan-card--highlight .shop-plan-unit { color: rgba(0,0,0,0.4); }
html.dark .shop-plan-equiv { color: #888; }
html.dark .shop-plan-card--highlight .shop-plan-equiv { color: rgba(0,0,0,0.5); }
html.dark .shop-plan-features { border-color: rgba(255,255,255,0.06); }
html.dark .shop-plan-card--highlight .shop-plan-features { border-color: rgba(0,0,0,0.08); }
html.dark .shop-plan-features li { color: #e5e5e5; }
html.dark .shop-plan-features li::before { background: #e5e5e5; }
html.dark .shop-plan-card--highlight .shop-plan-features li { color: rgba(0,0,0,0.7); }
html.dark .shop-plan-card--highlight .shop-plan-features li::before { background: rgba(0,0,0,0.4); }
html.dark .shop-plan-btn--light { background: rgba(255,255,255,0.08); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
html.dark .shop-plan-btn--light:hover { background: rgba(255,255,255,0.12); }
html.dark .shop-plan-btn--dark { background: #1d1d1f; color: #fff; }
</style>
