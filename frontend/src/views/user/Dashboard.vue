<script setup>
/**
 * 用户 Dashboard — 精致优雅风格
 */
import { ref, computed, onMounted } from 'vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { useAuthStore } from '@/stores/auth'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'

const auth = useAuthStore()
const toast = useToast()

const nodes = ref([])
const plans = ref([])
const paymentMethods = ref([])
const orders = ref([])
const plansModalOpen = ref(false)
const ordersModalOpen = ref(false)
const selectedPlan = ref(null)
const selectedPayment = ref(null)
const creatingOrder = ref(false)
const pollingOrder = ref(null)

const user = computed(() => auth.user || {})
const paidOrders = computed(() => orders.value.filter(o => o.status === 'paid').slice(0, 10))

const fmtBytes = (b) => {
  if (!b) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(b) / Math.log(1024))
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)} ${u[i]}`
}
const fmtPrice = (p) => { if (!p && p !== 0) return '—'; return p > 0 ? `¥${p}` : '免费' }

const trafficPercent = computed(() => {
  const limit = user.value.traffic_limit || 0
  if (!limit) return 0
  return Math.min(100, Math.round((user.value.traffic_used / limit) * 100))
})
const monthlyPercent = computed(() => {
  const limit = user.value.monthly_traffic_limit || 0
  if (!limit) return 0
  return Math.min(100, Math.round((user.value.monthly_traffic_used / limit) * 100))
})
const subUrl = computed(() => { if (!user.value.token) return ''; return `${location.origin}/api/sub/${user.value.token}` })
const expiredLabel = computed(() => { if (!user.value.expired_at) return '永不过期'; return new Date(user.value.expired_at).toLocaleString() })
const planLabel = computed(() => { if (!user.value.plan_name) return '无套餐'; return user.value.plan_name })

const loadNodes = async () => { try { nodes.value = await userApi.nodes() } catch (e) { nodes.value = [] } }
const loadPlans = async () => { try { plans.value = await userApi.plans() } catch (e) { plans.value = [] } }
const loadPaymentMethods = async () => { try { paymentMethods.value = await userApi.paymentMethods() } catch (e) { paymentMethods.value = [] } }
const loadOrders = async () => { try { orders.value = await userApi.myOrders() } catch (e) { orders.value = [] } }

const openPlans = async () => { plansModalOpen.value = true; selectedPlan.value = null; selectedPayment.value = null; await loadPlans(); if (!paymentMethods.value.length) await loadPaymentMethods(); await loadOrders() }
const openOrders = async () => { ordersModalOpen.value = true; await loadOrders() }

const syncing = ref(false)
const syncTraffic = async () => {
  if (syncing.value) return
  syncing.value = true
  try { await userApi.syncTraffic(); await auth.fetchMe(); toast.success('流量已同步') }
  catch (e) { toast.error(e.message || '同步失败') }
  finally { syncing.value = false }
}

const selectPlan = (p) => { selectedPlan.value = p; if (paymentMethods.value.length && !selectedPayment.value) selectedPayment.value = paymentMethods.value[0].id }

const createOrder = async () => {
  if (!selectedPlan.value) { toast.error('请选择套餐'); return }
  creatingOrder.value = true
  try {
    const result = await userApi.createOrder(selectedPlan.value.id, selectedPayment.value)
    if (result.status === 'paid') { toast.success('购买成功（免费套餐）'); plansModalOpen.value = false; await auth.fetchMe(); await loadOrders(); return }
    if (result.pay_url) { window.open(result.pay_url, '_blank'); toast.success('订单已创建，请完成支付'); pollingOrder.value = result.order_no; startPolling(result.order_no) }
  } catch (e) { toast.error(e.message || '创建订单失败') }
  finally { creatingOrder.value = false }
}

const startPolling = (orderNo) => {
  let count = 0
  const timer = setInterval(async () => {
    count++
    try { const status = await userApi.orderStatus(orderNo); if (status.status === 'paid') { clearInterval(timer); pollingOrder.value = null; toast.success('支付成功！'); await auth.fetchMe(); await loadOrders(); return } } catch (e) {}
    if (count >= 60) { clearInterval(timer); pollingOrder.value = null }
  }, 5000)
}

const copySub = async () => { if (!subUrl.value) return; try { await navigator.clipboard.writeText(subUrl.value); toast.success('订阅地址已复制') } catch (e) { toast.error('复制失败') } }

onMounted(() => { loadNodes(); loadOrders() })
</script>

<template>
  <div class="dash">
    <div class="dash-welcome">
      <div class="dash-welcome-text">
        <h1 class="dash-welcome-title">你好，{{ user.email?.split('@')[0] || '用户' }}</h1>
        <p class="dash-welcome-sub">欢迎回来，这是你的控制面板</p>
      </div>
      <button class="dash-sync-btn" :disabled="syncing" @click="syncTraffic"><Icon name="refresh" :size="16" />{{ syncing ? '同步中...' : '同步流量' }}</button>
    </div>

    <div class="dash-stats">
      <div class="dash-stat-card">
        <div class="dash-stat-icon"><Icon name="traffic" :size="20" /></div>
        <div class="dash-stat-info">
          <span class="dash-stat-label">{{ user.plan_type === 'period' ? '当月已用' : '已用流量' }}</span>
          <span class="dash-stat-value">{{ fmtBytes(user.plan_type === 'period' ? user.monthly_traffic_used : user.traffic_used) }}<template v-if="user.plan_type === 'period' ? user.monthly_traffic_limit : user.traffic_limit"><span class="dash-stat-limit"> / {{ fmtBytes(user.plan_type === 'period' ? user.monthly_traffic_limit : user.traffic_limit) }}</span></template></span>
        </div>
        <div class="dash-stat-bar"><div class="dash-stat-bar-fill" :style="{ width: (user.plan_type === 'period' ? monthlyPercent : trafficPercent) + '%' }"></div></div>
      </div>
      <div class="dash-stat-card"><div class="dash-stat-icon"><Icon name="shield" :size="20" /></div><div class="dash-stat-info"><span class="dash-stat-label">套餐</span><span class="dash-stat-value">{{ planLabel }}</span></div></div>
      <div class="dash-stat-card"><div class="dash-stat-icon"><Icon name="order" :size="20" /></div><div class="dash-stat-info"><span class="dash-stat-label">到期时间</span><span class="dash-stat-value">{{ expiredLabel }}</span></div></div>
      <div class="dash-stat-card"><div class="dash-stat-icon"><Icon name="check" :size="20" /></div><div class="dash-stat-info"><span class="dash-stat-label">状态</span><span class="dash-stat-value" :class="user.enabled ? 'dash-status-ok' : 'dash-status-err'">{{ user.enabled ? '正常' : '已禁用' }}</span></div></div>
    </div>

    <div v-if="user.plan_type === 'period' && user.traffic_limit" class="dash-card">
      <div class="dash-card-head"><h3 class="dash-card-title">周期总流量</h3><span class="dash-card-value">{{ fmtBytes(user.traffic_used) }} / {{ fmtBytes(user.traffic_limit) }}</span></div>
      <div class="dash-bar-wrap"><div class="dash-bar"><div class="dash-bar-fill" :style="{ width: trafficPercent + '%' }"></div></div><span class="dash-bar-label">{{ trafficPercent }}%</span></div>
    </div>

    <div class="dash-card">
      <div class="dash-card-head"><h3 class="dash-card-title">订阅地址</h3></div>
      <div class="dash-sub-url"><code>{{ subUrl || '—' }}</code><button v-if="subUrl" class="dash-copy-btn" @click="copySub"><Icon name="copy" :size="14" /></button></div>
      <div class="dash-actions">
        <button class="dash-action-btn dash-action-btn--primary" @click="openPlans"><Icon name="plus" :size="16" /> 订购订阅</button>
        <button class="dash-action-btn" @click="openOrders"><Icon name="order" :size="16" /> 最近订单</button>
      </div>
    </div>

    <div class="dash-card">
      <div class="dash-card-head"><h3 class="dash-card-title">在线节点</h3><span class="dash-card-count">{{ nodes.length }}</span></div>
      <div v-if="nodes.length" class="dash-nodes">
        <div v-for="n in nodes" :key="n.id" class="dash-node"><span class="dash-node-dot"></span><span class="dash-node-name">{{ n.name }}</span><span class="dash-node-latency">{{ n.latency }}ms</span></div>
      </div>
      <p v-else class="dash-empty">暂无可用节点</p>
    </div>

    <Modal v-model="plansModalOpen" title="订购订阅" width="600px">
      <div class="dash-modal-plans">
        <h3 class="dash-modal-section-title">选择套餐</h3>
        <div v-if="plans.length" class="dash-plan-list">
          <div v-for="p in plans" :key="p.id" class="dash-plan-item" :class="{ 'dash-plan-item--selected': selectedPlan?.id === p.id }" @click="selectPlan(p)">
            <div class="dash-plan-head"><span class="dash-plan-name">{{ p.name }}</span><span class="dash-plan-price">{{ fmtPrice(p.price) }}</span></div>
            <div class="dash-plan-detail"><template v-if="p.type === 'period'"><span>{{ p.months }}个月</span><span>月限 {{ fmtBytes(p.monthly_traffic) }}</span></template><template v-else><span>总量 {{ fmtBytes(p.total_traffic) }}</span><span>永久有效</span></template></div>
          </div>
        </div>
        <p v-else class="dash-empty">暂无套餐</p>
      </div>
      <div v-if="selectedPlan && selectedPlan.price > 0 && paymentMethods.length" class="dash-modal-pay">
        <h3 class="dash-modal-section-title">支付方式</h3>
        <div class="dash-pay-list"><label v-for="m in paymentMethods" :key="m.id" class="dash-pay-item" :class="{ 'dash-pay-item--selected': selectedPayment === m.id }"><input type="radio" :value="m.id" v-model="selectedPayment" /><span>{{ m.name }}</span></label></div>
      </div>
      <template #footer>
        <div class="dash-modal-footer">
          <button class="dash-modal-btn" @click="plansModalOpen = false">关闭</button>
          <button v-if="selectedPlan" class="dash-modal-btn dash-modal-btn--primary" :disabled="creatingOrder" @click="createOrder">{{ creatingOrder ? '处理中...' : (selectedPlan.price > 0 ? `支付 ${fmtPrice(selectedPlan.price)}` : '免费领取') }}</button>
        </div>
      </template>
    </Modal>

    <Modal v-model="ordersModalOpen" title="最近订单" width="500px">
      <div v-if="paidOrders.length" class="dash-order-list">
        <div v-for="o in paidOrders" :key="o.order_no" class="dash-order-item"><span class="dash-order-plan">{{ o.plan_name }}</span><span class="dash-order-amount">{{ fmtPrice(o.amount) }}</span><span class="dash-order-time">{{ new Date(o.paid_at || o.created_at).toLocaleString() }}</span></div>
      </div>
      <p v-else class="dash-empty">暂无订单</p>
      <template #footer><button class="dash-modal-btn" @click="ordersModalOpen = false">关闭</button></template>
    </Modal>
  </div>
</template>

<style scoped>
.dash { max-width: 900px; margin: 0 auto; display: flex; flex-direction: column; gap: 20px; }
.dash-welcome { display: flex; align-items: center; justify-content: space-between; padding: 32px; background: linear-gradient(135deg, #1d1d1f 0%, #333 100%); border-radius: 24px; color: #fff; }
.dash-welcome-title { margin: 0 0 4px; font-size: 24px; font-weight: 800; letter-spacing: -0.02em; }
.dash-welcome-sub { margin: 0; font-size: 14px; color: rgba(255,255,255,0.6); }
.dash-sync-btn { display: inline-flex; align-items: center; gap: 6px; padding: 10px 20px; border-radius: 9999px; font-size: 13px; font-weight: 600; border: 1px solid rgba(255,255,255,0.2); background: rgba(255,255,255,0.1); color: #fff; cursor: pointer; transition: all 0.3s; font-family: inherit; backdrop-filter: blur(10px); }
.dash-sync-btn:hover { background: rgba(255,255,255,0.2); }
.dash-sync-btn:disabled { opacity: 0.5; cursor: not-allowed; }

.dash-stats { display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px; }
.dash-stat-card { background: #fff; border: 1px solid rgba(0,0,0,0.04); border-radius: 20px; padding: 24px 20px; box-shadow: 0 2px 20px -4px rgba(0,0,0,0.06); display: flex; flex-direction: column; gap: 12px; }
.dash-stat-icon { display: flex; align-items: center; justify-content: center; width: 36px; height: 36px; border-radius: 10px; background: #eff6ff; color: #2563eb; }
.dash-stat-info { display: flex; flex-direction: column; gap: 2px; }
.dash-stat-label { font-size: 12px; color: #86868b; font-weight: 500; }
.dash-stat-value { font-size: 18px; font-weight: 800; letter-spacing: -0.02em; color: #1d1d1f; }
.dash-stat-limit { font-size: 13px; font-weight: 500; color: #86868b; }
.dash-stat-bar { height: 4px; background: rgba(0,0,0,0.04); border-radius: 2px; overflow: hidden; }
.dash-stat-bar-fill { height: 100%; background: #2563eb; border-radius: 2px; transition: width 0.6s cubic-bezier(0.16, 1, 0.3, 1); }
.dash-status-ok { color: #16a34a; }
.dash-status-err { color: #dc2626; }

.dash-card { background: #fff; border: 1px solid rgba(0,0,0,0.04); border-radius: 20px; padding: 24px; box-shadow: 0 2px 20px -4px rgba(0,0,0,0.06); }
.dash-card-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 16px; }
.dash-card-title { margin: 0; font-size: 15px; font-weight: 700; color: #1d1d1f; }
.dash-card-value { font-size: 13px; font-weight: 600; color: #86868b; font-family: -apple-system, system-ui, monospace; }
.dash-card-count { font-size: 12px; font-weight: 700; color: #2563eb; background: #eff6ff; padding: 2px 8px; border-radius: 9999px; }

.dash-bar-wrap { display: flex; align-items: center; gap: 12px; }
.dash-bar { flex: 1; height: 6px; background: rgba(0,0,0,0.04); border-radius: 3px; overflow: hidden; }
.dash-bar-fill { height: 100%; background: #2563eb; border-radius: 3px; transition: width 0.6s cubic-bezier(0.16, 1, 0.3, 1); }
.dash-bar-label { font-size: 12px; font-weight: 700; color: #86868b; min-width: 32px; text-align: right; }

.dash-sub-url { display: flex; align-items: center; gap: 8px; background: #f5f5f5; border: 1px solid rgba(0,0,0,0.04); border-radius: 12px; padding: 12px 16px; margin-bottom: 16px; }
.dash-sub-url code { flex: 1; font-size: 12px; color: #1d1d1f; word-break: break-all; font-family: -apple-system, SF Mono, monospace; }
.dash-copy-btn { display: flex; align-items: center; justify-content: center; width: 32px; height: 32px; border-radius: 8px; border: 1px solid rgba(0,0,0,0.06); background: #fff; color: #86868b; cursor: pointer; transition: all 0.3s; flex-shrink: 0; }
.dash-copy-btn:hover { color: #1d1d1f; border-color: rgba(0,0,0,0.12); }

.dash-actions { display: flex; gap: 10px; }
.dash-action-btn { display: inline-flex; align-items: center; gap: 6px; padding: 10px 18px; border-radius: 9999px; font-size: 13px; font-weight: 600; border: 1px solid rgba(0,0,0,0.08); background: #fff; color: #1d1d1f; cursor: pointer; transition: all 0.3s cubic-bezier(0.16, 1, 0.3, 1); font-family: inherit; }
.dash-action-btn:hover { background: #f5f5f5; }
.dash-action-btn--primary { background: #1d1d1f; color: #fff; border-color: #1d1d1f; }
.dash-action-btn--primary:hover { background: #333; }

.dash-nodes { display: flex; flex-direction: column; gap: 8px; }
.dash-node { display: flex; align-items: center; gap: 12px; padding: 12px 16px; background: #f5f5f5; border-radius: 12px; }
.dash-node-dot { width: 8px; height: 8px; border-radius: 50%; background: #16a34a; flex-shrink: 0; }
.dash-node-name { flex: 1; font-size: 14px; font-weight: 600; color: #1d1d1f; }
.dash-node-latency { font-size: 12px; font-weight: 600; color: #86868b; font-family: -apple-system, system-ui, monospace; }
.dash-empty { text-align: center; color: #86868b; font-size: 13px; padding: 24px; }

.dash-modal-section-title { margin: 0 0 12px; font-size: 13px; font-weight: 700; color: #86868b; text-transform: uppercase; letter-spacing: 0.05em; }
.dash-modal-plans { margin-bottom: 24px; }
.dash-plan-list { display: flex; flex-direction: column; gap: 8px; }
.dash-plan-item { padding: 16px 20px; background: #f5f5f5; border: 2px solid transparent; border-radius: 14px; cursor: pointer; transition: all 0.3s; }
.dash-plan-item:hover { border-color: rgba(0,0,0,0.08); }
.dash-plan-item--selected { border-color: #1d1d1f; background: #f0f0f0; }
.dash-plan-head { display: flex; align-items: center; gap: 8px; margin-bottom: 6px; }
.dash-plan-name { font-weight: 700; flex: 1; font-size: 14px; }
.dash-plan-price { font-weight: 800; font-size: 16px; }
.dash-plan-detail { display: flex; gap: 12px; font-size: 12px; color: #86868b; }

.dash-modal-pay { margin-bottom: 24px; }
.dash-pay-list { display: flex; gap: 8px; flex-wrap: wrap; }
.dash-pay-item { display: flex; align-items: center; gap: 8px; padding: 10px 16px; background: #f5f5f5; border: 2px solid transparent; border-radius: 12px; cursor: pointer; font-size: 13px; transition: all 0.3s; }
.dash-pay-item--selected { border-color: #1d1d1f; }

.dash-modal-footer { display: flex; justify-content: space-between; width: 100%; gap: 12px; }
.dash-modal-btn { padding: 12px 24px; border-radius: 9999px; font-size: 14px; font-weight: 600; border: 1px solid rgba(0,0,0,0.08); background: #fff; color: #1d1d1f; cursor: pointer; transition: all 0.3s; font-family: inherit; }
.dash-modal-btn:hover { background: #f5f5f5; }
.dash-modal-btn--primary { background: #1d1d1f; color: #fff; border-color: #1d1d1f; }
.dash-modal-btn--primary:hover { background: #333; }
.dash-modal-btn:disabled { opacity: 0.5; cursor: not-allowed; }

.dash-order-list { display: flex; flex-direction: column; gap: 8px; }
.dash-order-item { display: flex; align-items: center; gap: 12px; padding: 12px 16px; background: #f5f5f5; border-radius: 12px; font-size: 13px; }
.dash-order-plan { flex: 1; font-weight: 600; }
.dash-order-amount { font-weight: 700; }
.dash-order-time { color: #86868b; font-size: 12px; }

@media (max-width: 768px) { .dash-stats { grid-template-columns: repeat(2, 1fr); } .dash-welcome { flex-direction: column; gap: 16px; text-align: center; padding: 24px; } .dash-actions { flex-direction: column; } }
@media (max-width: 480px) { .dash-stats { grid-template-columns: 1fr; } }

/* 暗色模式 */
html.dark .dash-stat-card { background: rgba(20,20,20,0.8); border-color: rgba(255,255,255,0.06); box-shadow: 0 2px 20px -4px rgba(0,0,0,0.3); }
html.dark .dash-stat-icon { background: rgba(37,99,235,0.15); color: #60a5fa; }
html.dark .dash-stat-label { color: #86868b; }
html.dark .dash-stat-value { color: #fff; }
html.dark .dash-stat-limit { color: #86868b; }
html.dark .dash-stat-bar { background: rgba(255,255,255,0.06); }
html.dark .dash-stat-bar-fill { background: #60a5fa; }
html.dark .dash-status-ok { color: #4ade80; }
html.dark .dash-status-err { color: #f87171; }
html.dark .dash-card { background: rgba(20,20,20,0.8); border-color: rgba(255,255,255,0.06); box-shadow: 0 2px 20px -4px rgba(0,0,0,0.3); }
html.dark .dash-card-title { color: #fff; }
html.dark .dash-card-value { color: #86868b; }
html.dark .dash-card-count { background: rgba(37,99,235,0.15); color: #60a5fa; }
html.dark .dash-bar { background: rgba(255,255,255,0.06); }
html.dark .dash-bar-fill { background: #60a5fa; }
html.dark .dash-bar-label { color: #86868b; }
html.dark .dash-sub-url { background: rgba(255,255,255,0.04); border-color: rgba(255,255,255,0.06); }
html.dark .dash-sub-url code { color: #e5e5e5; }
html.dark .dash-copy-btn { background: rgba(255,255,255,0.05); border-color: rgba(255,255,255,0.08); color: #86868b; }
html.dark .dash-copy-btn:hover { color: #e5e5e5; border-color: rgba(255,255,255,0.15); }
html.dark .dash-action-btn { background: rgba(255,255,255,0.05); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
html.dark .dash-action-btn:hover { background: rgba(255,255,255,0.08); }
html.dark .dash-action-btn--primary { background: #fff; color: #0a0a0a; border-color: #fff; }
html.dark .dash-action-btn--primary:hover { background: #e5e5e5; }
html.dark .dash-node { background: rgba(255,255,255,0.04); }
html.dark .dash-node-dot { background: #4ade80; }
html.dark .dash-node-name { color: #e5e5e5; }
html.dark .dash-node-latency { color: #86868b; }
html.dark .dash-plan-item { background: rgba(255,255,255,0.04); border-color: transparent; }
html.dark .dash-plan-item:hover { border-color: rgba(255,255,255,0.1); }
html.dark .dash-plan-item--selected { border-color: #fff; background: rgba(255,255,255,0.08); }
html.dark .dash-plan-name { color: #fff; }
html.dark .dash-plan-price { color: #fff; }
html.dark .dash-plan-detail { color: #86868b; }
html.dark .dash-pay-item { background: rgba(255,255,255,0.04); border-color: transparent; }
html.dark .dash-pay-item--selected { border-color: #fff; }
html.dark .dash-modal-btn { background: rgba(255,255,255,0.05); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
html.dark .dash-modal-btn:hover { background: rgba(255,255,255,0.08); }
html.dark .dash-modal-btn--primary { background: #fff; color: #0a0a0a; border-color: #fff; }
html.dark .dash-modal-btn--primary:hover { background: #e5e5e5; }
html.dark .dash-order-item { background: rgba(255,255,255,0.04); }
html.dark .dash-order-plan { color: #e5e5e5; }
html.dark .dash-order-amount { color: #fff; }
html.dark .dash-modal-section-title { color: #86868b; }
</style>
