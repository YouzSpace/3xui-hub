<script setup>
/**
 * 余额充值弹层（用户端）。
 *
 * 流程对齐 docs/balance/implementation-plan.md §3.1.4：
 *   选金额 → POST /api/payment/create {order_type:'balance', amount:分}
 *   → 拿 pay_url 去第三方收银台 → 回调到账（本弹层轮询 /api/payment/status 等结果）。
 *
 * 三个口径：
 *  1. 金额一律**分**提交，输入框按「元」收，提交前用 yuanToCents 换算（整数，不走浮点）。
 *  2. 充值区间 1 元 ~ 10 万元（后端 rechargeMinCents/rechargeMaxCents），前端先拦一次，
 *     不把明显非法的单发给后端。
 *  3. 订单创建成功不等于到账：钱在第三方回调里进余额，所以这里只轮询状态，
 *     不自己改本地余额（余额由父组件重新拉 /api/balance）。
 */
import { computed, ref, watch, onUnmounted } from 'vue'
import BottomSheet from '@/components/user/BottomSheet.vue'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'
import { centsToYuan, yuanToCents, formatYuan } from '@/utils/money'

const props = defineProps({
  modelValue: { type: Boolean, default: false },
})
const emit = defineEmits(['update:modelValue', 'paid', 'redeemed'])

// 弹层内两个 tab：在线支付（默认）/ 兑换码。兑换码只在点进这个 tab 后才露出。
const tab = ref('pay')
// 兑换码输入
const code = ref('')
const redeeming = ref(false)
const normalizeCode = (v) => String(v || '').trim().toUpperCase()
const canRedeem = computed(() => !redeeming.value && normalizeCode(code.value).length > 0)

const redeem = async () => {
  const value = normalizeCode(code.value)
  if (!value) {
    toast.error('请输入兑换码')
    return
  }
  redeeming.value = true
  try {
    const res = await userApi.redeem(value)
    code.value = ''
    toast.success(`兑换成功，到账 ${formatYuan(res?.added || 0)}`)
    emit('redeemed')
    open.value = false
  } catch (e) {
    toast.error(e.message || '兑换失败')
  } finally {
    redeeming.value = false
  }
}

// 预设档位（分）：10 / 30 / 50 / 100 / 200 / 500 元
const PRESETS = [1000, 3000, 5000, 10000, 20000, 50000]
// 与后端 rechargeMinCents / rechargeMaxCents 对齐（分）
const MIN_CENTS = 100
const MAX_CENTS = 10000000
// 轮询：4s 一次，最多 75 次（≈5 分钟）；超时后停止，用户可重新点「我已支付」
const POLL_INTERVAL = 4000
const POLL_MAX = 75

const toast = useToast()

const open = computed({
  get: () => props.modelValue,
  set: (v) => emit('update:modelValue', v),
})

const presetCents = ref(PRESETS[2])
const customYuan = ref('')
const methods = ref([])
const methodId = ref(null)
const submitting = ref(false)
const waiting = ref(false)
const payUrl = ref('')

let pollTimer = null
let pollCount = 0

// 自定义金额优先：填了就以它为准（预设高亮同时取消）
const chosenCents = computed(() => {
  if (customYuan.value.trim() !== '') return yuanToCents(customYuan.value)
  return presetCents.value
})

const pickPreset = (cents) => {
  presetCents.value = cents
  customYuan.value = ''
}

const loadMethods = async () => {
  if (methods.value.length) return
  try {
    methods.value = (await userApi.paymentMethods()) || []
    if (methods.value.length) methodId.value = methods.value[0].id
  } catch (e) {
    methods.value = []
  }
}

const stopPolling = () => {
  if (pollTimer) {
    clearTimeout(pollTimer)
    pollTimer = null
  }
  pollCount = 0
}

const pollStatus = async (orderNo) => {
  pollTimer = null
  pollCount += 1
  try {
    const res = await userApi.orderStatus(orderNo)
    if (res?.status === 'paid') {
      waiting.value = false
      stopPolling()
      toast.success('充值成功，余额已到账')
      emit('paid')
      open.value = false
      return
    }
  } catch (e) {
    /* 单次查询失败继续等：回调到账可能比前端查询晚几秒 */
  }
  if (pollCount >= POLL_MAX) {
    waiting.value = false
    toast.info('还没有收到支付结果，到账后余额会自动更新')
    return
  }
  pollTimer = setTimeout(() => pollStatus(orderNo), POLL_INTERVAL)
}

const submit = async () => {
  const cents = chosenCents.value
  if (cents === null) {
    toast.error('金额格式不对，最多两位小数')
    return
  }
  if (cents < MIN_CENTS || cents > MAX_CENTS) {
    toast.error(`单次充值需在 ${MIN_CENTS / 100} 元到 ${MAX_CENTS / 100} 元之间`)
    return
  }
  if (waiting.value) return

  submitting.value = true
  try {
    const res = await userApi.createRechargeOrder(cents, methodId.value)
    if (!res?.pay_url) {
      toast.error('获取支付链接失败，请稍后再试')
      return
    }
    payUrl.value = res.pay_url
    window.open(res.pay_url, '_blank')
    toast.success('订单已创建，请在支付页完成付款')
    waiting.value = true
    pollCount = 0
    pollStatus(res.order_no)
  } catch (e) {
    toast.error(e.message || '创建充值订单失败')
  } finally {
    submitting.value = false
  }
}

const reopenPay = () => {
  if (payUrl.value) window.open(payUrl.value, '_blank')
}

const submitLabel = computed(() => {
  if (waiting.value) return '等待支付…'
  if (submitting.value) return '处理中…'
  return `支付 ¥${centsToYuan(chosenCents.value ?? 0)}`
})

// 打开时拉支付方式；关闭时停轮询（避免弹层关掉还在后台刷接口）
watch(
  () => props.modelValue,
  (v) => {
    if (v) {
      loadMethods()
    } else {
      stopPolling()
      waiting.value = false
    }
  },
)

onUnmounted(stopPolling)
</script>

<template>
  <BottomSheet v-model="open" title="充值余额" max-width="460px">
    <div class="brs-tabs" role="tablist">
      <button type="button" class="brs-tab" :class="{ 'brs-tab--on': tab === 'pay' }" role="tab" :aria-selected="tab === 'pay'" @click="tab = 'pay'">在线支付</button>
      <button type="button" class="brs-tab" :class="{ 'brs-tab--on': tab === 'redeem' }" role="tab" :aria-selected="tab === 'redeem'" @click="tab = 'redeem'">兑换码</button>
    </div>

    <div v-if="tab === 'pay'" class="brs">
      <p class="brs-label">充值金额</p>
      <div class="brs-grid">
        <button
          v-for="p in PRESETS"
          :key="p"
          type="button"
          class="brs-amt uu-press"
          :class="{ 'brs-amt--on': customYuan.trim() === '' && presetCents === p }"
          @click="pickPreset(p)"
        >
          ¥{{ centsToYuan(p) }}
        </button>
      </div>

      <label class="brs-custom">
        <span class="brs-custom-label">其他金额</span>
        <input
          v-model="customYuan"
          class="brs-custom-input"
          type="text"
          inputmode="decimal"
          autocomplete="off"
          placeholder="1 ~ 100000"
        />
        <span class="brs-custom-unit">元</span>
      </label>
      <p class="brs-hint">
        充值金额 {{
          centsToYuan(MIN_CENTS)
        }} 元起；支付成功后余额自动到账，可用来买套餐。
      </p>

      <template v-if="methods.length">
        <p class="brs-label">支付方式</p>
        <div class="brs-methods">
          <button
            v-for="m in methods"
            :key="m.id"
            type="button"
            class="brs-method uu-press"
            :class="{ 'brs-method--on': methodId === m.id }"
            @click="methodId = m.id"
          >
            {{ m.name }}
          </button>
        </div>
      </template>
    </div>

    <!-- 兑换码 tab -->
    <div v-else class="brs">
      <p class="brs-hint">输入余额兑换码，兑换后余额自动到账，可用于购买套餐。</p>
      <div class="brs-redeem">
        <input
          v-model="code"
          class="brs-redeem-input"
          type="text"
          maxlength="32"
          autocomplete="off"
          spellcheck="false"
          placeholder="输入余额兑换码"
          @keyup.enter="redeem"
        />
      </div>
    </div>

    <template #footer>
      <template v-if="tab === 'pay'">
        <button type="button" class="brs-submit uu-press" :disabled="submitting || waiting" @click="submit">
          {{ submitLabel }}
        </button>
        <button v-if="waiting" type="button" class="brs-reopen uu-press" @click="reopenPay">
          重新打开支付页
        </button>
      </template>
      <button v-else type="button" class="brs-submit uu-press" :disabled="!canRedeem" @click="redeem">
        {{ redeeming ? '兑换中…' : '兑换' }}
      </button>
    </template>
  </BottomSheet>
</template>

<style scoped>
.brs-tabs {
  display: flex;
  gap: 4px;
  margin-bottom: 12px;
  border-bottom: 1px solid var(--border-glass);
}
.brs-tab {
  padding: 8px 16px;
  border: none;
  background: transparent;
  cursor: pointer;
  font-family: inherit;
  font-size: 14px;
  color: var(--lbl3);
  border-bottom: 2px solid transparent;
  margin-bottom: -1px;
}
.brs-tab--on {
  color: var(--blue);
  border-bottom-color: var(--blue);
  font-weight: 600;
}
.brs-redeem-input {
  width: 100%;
  height: 46px;
  padding: 0 14px;
  border: 1px solid var(--border-glass);
  border-radius: 14px;
  background: var(--fill4);
  color: var(--lbl1);
  font-size: 15px;
  font-family: inherit;
  letter-spacing: 0.5px;
  outline: none;
}
.brs-redeem-input::placeholder {
  color: var(--lbl4);
}

.brs {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.brs-label {
  margin: 0;
  font-size: 13px;
  font-weight: 600;
  color: var(--lbl3);
}
.brs-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
}
.brs-amt {
  height: 46px;
  border: 1px solid var(--border-glass);
  border-radius: 14px;
  background: var(--fill4);
  color: var(--lbl1);
  font-size: 16px;
  font-weight: 700;
  font-family: inherit;
}
.brs-amt--on {
  border-color: var(--blue);
  background: var(--blue-t);
  color: var(--blue);
}

.brs-custom {
  display: flex;
  align-items: center;
  gap: 10px;
  height: 46px;
  padding: 0 14px;
  border: 1px solid var(--border-glass);
  border-radius: 14px;
  background: var(--fill4);
}
.brs-custom-label {
  flex: none;
  font-size: 13px;
  color: var(--lbl3);
}
.brs-custom-input {
  flex: 1;
  min-width: 0;
  border: none;
  outline: none;
  background: transparent;
  color: var(--lbl1);
  font-size: 16px;
  font-weight: 600;
  font-family: inherit;
}
.brs-custom-unit {
  flex: none;
  font-size: 13px;
  color: var(--lbl3);
}
.brs-hint {
  margin: 0;
  font-size: 12px;
  line-height: 1.5;
  color: var(--lbl3);
}

.brs-methods {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
}
.brs-method {
  min-height: 40px;
  padding: 0 16px;
  border: 1px solid var(--border-glass);
  border-radius: 12px;
  background: var(--fill4);
  color: var(--lbl1);
  font-size: 14px;
  font-family: inherit;
}
.brs-method--on {
  border-color: var(--blue);
  background: var(--blue-t);
  color: var(--blue);
  font-weight: 600;
}

.brs-submit {
  height: 50px;
  border: none;
  border-radius: 14px;
  background: var(--blue);
  color: #fff;
  font-size: 16px;
  font-weight: 700;
  font-family: inherit;
}
.brs-submit:disabled {
  opacity: 0.55;
}
.brs-reopen {
  height: 44px;
  border: 1px solid var(--border-glass);
  border-radius: 12px;
  background: transparent;
  color: var(--lbl2);
  font-size: 14px;
  font-family: inherit;
}

/* 窄屏：金额格变 2 列，按钮更高，避免挤成一条 */
@media (max-width: 380px) {
  .brs-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
</style>
