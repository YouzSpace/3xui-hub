<script setup>
/**
 * Orders — 我的订单全屏页。
 * 数据来自 userApi.myOrders()（GET /api/payment/orders）。
 */
import { ref, onMounted } from 'vue'
import { userApi } from '@/api/user'

const orders = ref([])
const loading = ref(true)

const STATUS = {
  paid: { label: '已支付', color: 'var(--green)', bg: 'var(--green-t)' },
  pending: { label: '待支付', color: 'var(--orange)', bg: 'var(--orange-t)' },
  failed: { label: '失败', color: 'var(--red)', bg: 'var(--red-t)' },
  expired: { label: '已过期', color: 'var(--lbl3)', bg: 'var(--fill3)' },
}
const statusOf = (s) => STATUS[s] || { label: s || '未知', color: 'var(--lbl3)', bg: 'var(--fill3)' }

const fmtAmount = (a) => (a > 0 ? `¥${a}` : '免费')

const fmtTime = (o) => {
  const t = o.paid_at || o.created_at
  if (!t) return '—'
  const d = new Date(t)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

onMounted(async () => {
  try {
    orders.value = (await userApi.myOrders()) || []
  } catch (e) {
    orders.value = []
  }
  loading.value = false
})
</script>

<template>
  <div class="od uu-slide-up">
    <section class="od-card uu-mat-regular">
      <div v-if="orders.length" class="od-list">
        <div v-for="o in orders" :key="o.order_no" class="od-row">
          <div class="od-main">
            <p class="od-plan">{{ o.plan_name || '套餐' }}</p>
            <p class="od-time">{{ fmtTime(o) }}</p>
          </div>
          <div class="od-side">
            <p class="od-amount">{{ fmtAmount(o.amount) }}</p>
            <span
              class="od-badge"
              :style="{ color: statusOf(o.status).color, background: statusOf(o.status).bg }"
            >
              {{ statusOf(o.status).label }}
            </span>
          </div>
        </div>
      </div>
      <p v-else-if="!loading" class="od-empty">暂无订单</p>
    </section>
  </div>
</template>

<style scoped>
.od {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.od-card {
  border-radius: 24px;
  padding: 8px 16px;
}

.od-list {
  display: flex;
  flex-direction: column;
}
.od-row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 14px 0;
  border-bottom: 1px solid var(--sep);
}
.od-row:last-child {
  border-bottom: none;
}

.od-main {
  flex: 1;
  min-width: 0;
}
.od-plan {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: var(--lbl1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.od-time {
  margin: 2px 0 0;
  font-size: 12px;
  color: var(--lbl3);
}

.od-side {
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 4px;
  flex-shrink: 0;
}
.od-amount {
  margin: 0;
  font-size: 15px;
  font-weight: 700;
  color: var(--lbl1);
  font-variant-numeric: tabular-nums;
}
.od-badge {
  padding: 2px 10px;
  border-radius: 999px;
  font-size: 11px;
  font-weight: 600;
  white-space: nowrap;
}

.od-empty {
  margin: 0;
  padding: 40px 0;
  text-align: center;
  font-size: 14px;
  color: var(--lbl3);
}
</style>
