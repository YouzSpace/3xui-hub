<script setup>
/**
 * 余额卡片（用户端「我的」页）。
 *
 * 数据源：GET /api/balance（{balance, logs}，金额单位分），API 口径见
 * docs/balance/implementation-plan.md §3.1.1 / §3.1.2。
 *
 * 两块内容对应规划 §模块5 的用户端要求：
 *  ① 余额（分 → 元展示）+ 充值入口（弹层内：在线支付 / 兑换码）
 *  ② 余额流水（分页「加载更多」，每条带变动后余额）
 *
 * 金额显示一律走 utils/money：后端只认整数分，前端不自己拼小数。
 */
import { computed, onMounted, ref } from 'vue'
import UserIcon from '@/components/user/UserIcon.vue'
import BalanceRechargeSheet from '@/components/user/BalanceRechargeSheet.vue'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'
import { formatSignedYuan, formatYuan } from '@/utils/money'

const PER_PAGE = 20

const TYPE_LABELS = {
  recharge: '充值',
  consume: '消费',
  redeem: '兑换',
  admin: '管理员调整',
  rebate: '返佣',
}

const toast = useToast()

const loading = ref(true)
const balance = ref(0)
const logs = ref([])
const page = ref(1)
// 后端不返回 user 流水总数：用「最近一批是否装满」判断还有没有下一页
const hasMore = ref(false)
const loadingMore = ref(false)
const rechargeOpen = ref(false)

const typeLabel = (t) => TYPE_LABELS[t] || t

// 后端 user 流水是 ISO（2026-11-07T04:29:24+00:00），按浏览器本地时区显示
const fmtTime = (v) => {
  if (!v) return ''
  const d = new Date(String(v).replace(' ', 'T'))
  if (Number.isNaN(d.getTime())) return String(v)
  const pad = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

const applyPage = (res, append) => {
  balance.value = Number(res?.balance || 0)
  const rows = Array.isArray(res?.logs) ? res.logs : []
  logs.value = append ? [...logs.value, ...rows] : rows
  hasMore.value = rows.length >= PER_PAGE
}

const load = async () => {
  loading.value = true
  try {
    const res = await userApi.balance({ page: 1, per_page: PER_PAGE })
    page.value = 1
    applyPage(res, false)
  } catch (e) {
    // 余额接口失败不阻断仪表盘其它卡片：只把这块标成 0 并提示一次
    toast.error(e.message || '余额加载失败')
  } finally {
    loading.value = false
  }
}

const loadMore = async () => {
  if (loadingMore.value || !hasMore.value) return
  loadingMore.value = true
  try {
    const next = page.value + 1
    const res = await userApi.balance({ page: next, per_page: PER_PAGE })
    page.value = next
    applyPage(res, true)
  } catch (e) {
    toast.error(e.message || '加载更多失败')
  } finally {
    loadingMore.value = false
  }
}

// 充值是第三方支付：订单创建后钱不会立刻到账，等弹层轮询到 paid 再刷新余额
const onRecharged = () => {
  load()
}

const amountClass = (amount) => (Number(amount) < 0 ? 'is-out' : 'is-in')

onMounted(load)
</script>

<template>
  <section class="bp uu-mat-regular">
    <div class="bp-head">
      <div class="bp-head-main">
        <p class="bp-label">账户余额</p>
        <p class="bp-amount">{{ loading ? '—' : formatYuan(balance) }}</p>
      </div>
      <button type="button" class="bp-recharge uu-press" @click="rechargeOpen = true">
        充值
      </button>
    </div>

    <!-- 流水 -->
    <div class="bp-logs">
      <div class="bp-logs-head">
        <p class="bp-logs-title">余额明细</p>
        <button
          v-if="hasMore"
          type="button"
          class="bp-more uu-press"
          :disabled="loadingMore"
          @click="loadMore"
        >
          <UserIcon name="chevronDown" :size="14" />
          {{ loadingMore ? '加载中…' : '更多' }}
        </button>
      </div>

      <p v-if="loading" class="bp-empty">加载中…</p>
      <p v-else-if="!logs.length" class="bp-empty">还没有余额变动</p>
      <ul v-else class="bp-list">
        <li v-for="l in logs" :key="l.id" class="bp-log">
          <div class="bp-log-main">
            <span class="bp-log-type">{{ typeLabel(l.type) }}</span>
            <span v-if="l.note" class="bp-log-note" :title="l.note">{{ l.note }}</span>
          </div>
          <div class="bp-log-side">
            <span class="bp-log-amt" :class="amountClass(l.amount)">
              {{ formatSignedYuan(l.amount) }}
            </span>
            <span class="bp-log-time">{{ fmtTime(l.created_at) }}</span>
          </div>
        </li>
      </ul>
    </div>

    <BalanceRechargeSheet v-model="rechargeOpen" @paid="onRecharged" @redeemed="onRecharged" />
  </section>
</template>

<style scoped>
.bp {
  border-radius: 24px;
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.bp-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
}
.bp-head-main {
  min-width: 0;
}
.bp-label {
  margin: 0 0 4px;
  font-size: 13px;
  color: var(--lbl3);
}
.bp-amount {
  margin: 0;
  font-size: 30px;
  font-weight: 800;
  letter-spacing: -0.5px;
  color: var(--lbl1);
}
.bp-recharge {
  flex: none;
  height: 34px;
  padding: 0 18px;
  border: none;
  border-radius: 999px;
  background: var(--blue);
  color: #fff;
  font-size: 14px;
  font-weight: 600;
  font-family: inherit;
}

.bp-logs {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.bp-logs-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.bp-logs-title {
  margin: 0;
  font-size: 13px;
  font-weight: 600;
  color: var(--lbl3);
}
.bp-more {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  height: 26px;
  padding: 0 10px;
  border: none;
  border-radius: 999px;
  background: var(--fill4);
  color: var(--lbl2);
  font-size: 12px;
  font-family: inherit;
}
.bp-empty {
  margin: 0;
  padding: 10px 0;
  font-size: 13px;
  color: var(--lbl3);
}
.bp-list {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
}
.bp-log {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 11px 0;
  border-top: 1px solid var(--border-glass);
}
.bp-list .bp-log:first-child {
  border-top: none;
}
.bp-log-main {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.bp-log-type {
  font-size: 14px;
  font-weight: 600;
  color: var(--lbl1);
}
.bp-log-note {
  font-size: 12px;
  color: var(--lbl3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.bp-log-side {
  flex: none;
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 3px;
}
.bp-log-amt {
  font-size: 15px;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
}
.bp-log-amt.is-in {
  color: var(--green);
}
.bp-log-amt.is-out {
  color: var(--red);
}
.bp-log-time {
  font-size: 12px;
  color: var(--lbl3);
}

@media (max-width: 360px) {
  .bp-amount {
    font-size: 26px;
  }
}
</style>
