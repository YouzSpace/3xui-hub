<script setup>
/**
 * 用户端 · 代理商城。
 *
 * 和「套餐商店」是两个独立产品线：
 *  - 套餐商店卖的是节点订阅（周期 + 流量）
 *  - 这里卖的是代理出口（住宅 IP / 共享 socks），不影响节点订阅
 *
 * 购买流程与套餐一致：选规格 → 选支付方式 → 跳收银台 → 轮询订单状态。
 */
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'

const router = useRouter()
const toast = useToast()

const products = ref([])
const paymentMethods = ref([])
const loading = ref(true)

const buyOpen = ref(false)
const selectedSku = ref(null)
const selectedPayment = ref(null)
const buying = ref(false)
const polling = ref(false)

const anySku = computed(() => products.value.some((p) => p.skus?.length))

const load = async () => {
  loading.value = true
  try {
    products.value = (await userApi.proxyProducts()) || []
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const openBuy = async (sku) => {
  selectedSku.value = sku
  if (!paymentMethods.value.length) {
    try {
      paymentMethods.value = (await userApi.paymentMethods()) || []
    } catch (e) {
      paymentMethods.value = []
    }
  }
  selectedPayment.value = paymentMethods.value[0]?.id ?? null
  buyOpen.value = true
}

const doBuy = async () => {
  if (!selectedSku.value) return
  if (paymentMethods.value.length && !selectedPayment.value) {
    toast.error('请选择支付方式')
    return
  }
  buying.value = true
  try {
    const result = await userApi.createProxyOrder(selectedSku.value.id, selectedPayment.value)
    if (result.status === 'paid') {
      toast.success('开通成功')
      buyOpen.value = false
      router.push('/dashboard/proxy/mine')
      return
    }
    if (result.pay_url) {
      buyOpen.value = false
      window.open(result.pay_url, '_blank')
      toast.success('订单已创建，请在收银台完成支付')
      startPolling(result.order_no)
    } else {
      toast.error('获取支付链接失败，请稍后再试')
    }
  } catch (e) {
    toast.error(e.message || '下单失败')
  } finally {
    buying.value = false
  }
}

const startPolling = (orderNo) => {
  polling.value = true
  let count = 0
  const timer = setInterval(async () => {
    count++
    if (count > 60) {
      clearInterval(timer)
      polling.value = false
      return
    }
    try {
      const status = await userApi.orderStatus(orderNo)
      if (status?.status === 'paid') {
        clearInterval(timer)
        polling.value = false
        toast.success('支付成功，代理已开通')
        router.push('/dashboard/proxy/mine')
      }
    } catch (e) {
      /* 轮询失败静默，等下一轮 */
    }
  }, 3000)
}

const skuMeta = (sku) =>
  [
    sku.traffic_gb > 0 ? `${sku.traffic_gb} GB` : '不限流量',
    `${sku.duration_days} 天`,
    sku.max_thread_count > 0 ? `${sku.max_thread_count} 并发` : null,
    sku.region || null,
  ]
    .filter(Boolean)
    .join(' · ')

const kindLabel = (kind) => (kind === 'socks' ? '共享 socks' : '独享账号')

onMounted(load)
</script>

<template>
  <div class="px">
    <div class="px-head">
      <div>
        <h1 class="px-title">代理商城</h1>
        <p class="px-sub">住宅 IP 与共享 socks，独立于节点套餐，不影响你的订阅。</p>
      </div>
      <Button variant="ghost" size="sm" @click="router.push('/dashboard/proxy/mine')">
        <Icon name="server" :size="15" /> 我的代理
      </Button>
    </div>

    <p v-if="polling" class="px-poll">支付完成后会自动跳转，请稍候…</p>

    <Card v-for="p in products" :key="p.id" class="px-card">
      <div class="px-phead">
        <h2 class="px-pname">{{ p.name }}</h2>
        <span v-if="p.subtitle" class="px-psub">{{ p.subtitle }}</span>
      </div>
      <p v-if="p.description" class="px-pdesc">{{ p.description }}</p>

      <div class="px-skus">
        <button
          v-for="sku in p.skus"
          :key="sku.id"
          class="px-sku"
          type="button"
          @click="openBuy(sku)"
        >
          <span class="px-sku-top">
            <span class="px-sku-name">{{ sku.name }}</span>
            <Badge variant="default">{{ kindLabel(sku.kind) }}</Badge>
          </span>
          <span class="px-sku-meta">{{ skuMeta(sku) }}</span>
          <span class="px-sku-foot">
            <span class="px-price">¥{{ sku.price }}</span>
            <span class="px-stock">剩 {{ sku.stock_available }} 份</span>
          </span>
        </button>
      </div>
    </Card>

    <Card v-if="!anySku && !loading" class="px-card">
      <p class="px-empty">暂时没有可购买的代理，看看别的吧。</p>
    </Card>

    <Modal v-model="buyOpen" title="确认购买" width="460px">
      <div v-if="selectedSku" class="px-buy">
        <div class="px-buy-row">
          <span class="px-buy-label">规格</span>
          <span>{{ selectedSku.name }}</span>
        </div>
        <div class="px-buy-row">
          <span class="px-buy-label">内容</span>
          <span>{{ skuMeta(selectedSku) }}</span>
        </div>
        <div class="px-buy-row">
          <span class="px-buy-label">金额</span>
          <span class="px-price">¥{{ selectedSku.price }}</span>
        </div>

        <div v-if="paymentMethods.length" class="px-pays">
          <span class="px-buy-label">支付方式</span>
          <div class="px-pay-list">
            <button
              v-for="m in paymentMethods"
              :key="m.id"
              class="px-pay"
              :class="{ active: selectedPayment === m.id }"
              type="button"
              @click="selectedPayment = m.id"
            >
              {{ m.name }}
            </button>
          </div>
        </div>

        <p class="px-tip">
          购买后可在「我的代理」查看账号密码与 IP。流量额度由上游计费，本站只做转发与展示。
        </p>
      </div>

      <template #footer>
        <div class="px-foot">
          <Button variant="ghost" size="sm" @click="buyOpen = false">取消</Button>
          <Button variant="primary" size="sm" :loading="buying" @click="doBuy">去支付</Button>
        </div>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.px { max-width: 900px; }
.px-head { display: flex; align-items: flex-start; justify-content: space-between; gap: var(--space-3); margin-bottom: var(--space-4); }
.px-title { margin: 0 0 4px; font-size: var(--text-xl); font-weight: var(--font-bold); }
.px-sub { margin: 0; color: var(--text-muted); font-size: var(--text-sm); }
.px-poll { margin: 0 0 var(--space-3); color: var(--text-muted); font-size: var(--text-sm); }
.px-card { margin-bottom: var(--space-4); }
.px-phead { display: flex; align-items: baseline; gap: var(--space-2); flex-wrap: wrap; }
.px-pname { margin: 0; font-size: var(--text-base); font-weight: var(--font-bold); }
.px-psub { color: var(--text-muted); font-size: var(--text-sm); }
.px-pdesc { margin: 6px 0 0; color: var(--text-secondary); font-size: var(--text-sm); line-height: 1.6; }

.px-skus { display: grid; grid-template-columns: repeat(auto-fill, minmax(240px, 1fr)); gap: var(--space-3); margin-top: var(--space-3); }
.px-sku {
  display: flex; flex-direction: column; gap: 6px; text-align: left;
  padding: var(--space-3); border: 1px solid var(--border-subtle); border-radius: 12px;
  background: #fff; cursor: pointer; transition: border-color 0.15s, box-shadow 0.15s;
  font-family: inherit;
}
.px-sku:hover { border-color: var(--accent); box-shadow: 0 2px 10px rgba(0, 0, 0, 0.04); }
.px-sku-top { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.px-sku-name { font-weight: var(--font-medium); font-size: var(--text-sm); }
.px-sku-meta { color: var(--text-muted); font-size: var(--text-xs); }
.px-sku-foot { display: flex; align-items: baseline; justify-content: space-between; margin-top: 2px; }
.px-price { color: var(--accent); font-weight: var(--font-bold); font-size: var(--text-base); }
.px-stock { color: var(--text-muted); font-size: var(--text-xs); }
.px-empty { margin: 0; color: var(--text-muted); font-size: var(--text-sm); text-align: center; padding: var(--space-4) 0; }

.px-buy { display: flex; flex-direction: column; gap: var(--space-3); }
.px-buy-row { display: flex; align-items: center; justify-content: space-between; font-size: var(--text-sm); }
.px-buy-label { color: var(--text-muted); font-size: var(--text-sm); }
.px-pays { display: flex; flex-direction: column; gap: 8px; }
.px-pay-list { display: flex; flex-wrap: wrap; gap: 8px; }
.px-pay {
  padding: 6px 14px; border: 1px solid var(--border-subtle); border-radius: 999px;
  background: #fff; cursor: pointer; font-size: var(--text-sm); font-family: inherit;
}
.px-pay.active { border-color: var(--accent); color: var(--accent); background: var(--bg-hover); }
.px-tip { margin: 0; color: var(--text-muted); font-size: var(--text-xs); line-height: 1.6; }
.px-foot { display: flex; justify-content: flex-end; gap: var(--space-2); width: 100%; }
</style>
