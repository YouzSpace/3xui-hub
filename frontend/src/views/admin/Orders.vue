<script setup>
/**
 * Admin 订单管理：查看所有支付订单 + 搜索。
 * 列表分页：每页 50 条，页码走 ?page=（刷新/切页回来不丢页）。
 */
import { ref, computed, watch, onMounted, onActivated, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Icon from '@/components/ui/Icon.vue'
import Pagination from '@/components/ui/Pagination.vue'
import Skeleton from '@/components/ui/Skeleton.vue'
import { adminApi } from '@/api/admin'
import { usePagedList } from '@/composables/usePagedList'

const route = useRoute()
const router = useRouter()
const orders = ref([])
// 初始值取自 URL：直连 / 刷新 ?q=xxx 时搜索词直接进框并随首屏 load 生效（不能只靠下面
// 的 watch —— 它没有 immediate，首次进入不触发）
const searchQuery = ref(String(route.query.q ?? ''))
// 订单类型筛选：全部 / 节点套餐 / 代理商城（代理是独立产品线，混合列表对账不方便）
const orderType = ref(String(route.query.type ?? ''))
const typeOptions = [
  { label: '全部订单', value: '' },
  { label: '节点套餐', value: 'plan' },
  { label: '代理商城', value: 'proxy' },
]

const { page, loading, loadError, pagination, hasPrev, hasNext, prev, next, fetchPage } = usePagedList()

const fmtPrice = (p) => {
  if (!p && p !== 0) return '—'
  return p > 0 ? `¥${p}` : '免费'
}

const statusText = (s) => ({
  pending: '待支付',
  paid: '已支付',
  failed: '失败',
  expired: '已过期',
}[s] || s)

const statusVariant = (s) => ({
  pending: 'warning',
  paid: 'success',
  failed: 'danger',
  expired: 'info',
}[s] || 'info')


const load = async () => {
  const q = searchQuery.value.trim()
  const extra = {}
  if (q) extra.search = q
  if (orderType.value) extra.order_type = orderType.value
  const res = await fetchPage((params, config) => adminApi.listOrders(params, config), Object.keys(extra).length ? extra : null)
  if (res) orders.value = res
}

// 搜索走后端全表查询：前端只有当前页 50 条，对象在别的页就永远搜不到。
let searchTimer = null
watch(searchQuery, () => {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    const q = searchQuery.value.trim()
    if (!page.value || page.value === 1) {
      const next = { ...route.query }
      if (q) next.q = q
      else delete next.q
      router.replace({ query: next })
      load()
    } else {
      router.push({ query: { q: q || undefined, page: undefined } })
    }
  }, 300)
})

watch(page, load)

// URL 里的 ?q= 回填搜索框（刷新 / 分享链接回来时词还在）
watch(() => route.query.q, (v) => {
  const q = v || ''
  if (q !== searchQuery.value) searchQuery.value = q
})

watch(orderType, () => { page.value = 1; load() })

onUnmounted(() => clearTimeout(searchTimer))
onMounted(load)
onActivated(load)
</script>

<template>
  <div class="ord">
    <div class="ord-head">
      <h1 class="ord-title">订单</h1>
      <div class="ord-head-right">
        <Select v-model="orderType" :options="typeOptions" class="ord-type" />
        <Input v-model="searchQuery" placeholder="搜索 订单号/用户/套餐..." class="ord-search" />
        <Button variant="secondary" size="sm" :loading="loading" @click="load">
          <Icon name="refresh" :size="14" /> 刷新
        </Button>
      </div>
    </div>

    <Card padding="false">
      <div class="ord-table-wrap">
        <table class="ord-table">
          <thead>
            <tr>
              <th>订单号</th>
              <th>用户</th>
              <th>套餐</th>
              <th>金额</th>
              <th>状态</th>
              <th>支付方式</th>
              <th>交易号</th>
              <th>时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="o in orders" :key="o.order_no">
              <td class="mono ord-no">{{ o.order_no }}</td>
              <td>{{ o.user_email || `#${o.user_id}` }}</td>
              <td>{{ o.plan_name }}</td>
              <td class="mono ord-amount">{{ fmtPrice(o.amount) }}</td>
              <td><Badge :variant="statusVariant(o.status)" dot>{{ statusText(o.status) }}</Badge></td>
              <td>{{ o.payment_name || '—' }}</td>
              <td class="mono">{{ o.trade_no || '—' }}</td>
              <td>{{ new Date(o.created_at).toLocaleString() }}</td>
            </tr>
            <tr v-if="loadError">
              <td colspan="8" class="ord-empty">
                加载失败：{{ loadError }}
                <Button variant="secondary" size="sm" @click="load">重试</Button>
              </td>
            </tr>
            <tr v-else-if="!orders.length">
              <td colspan="8" class="ord-empty">
                <Skeleton v-if="loading" :rows="5" height="32px" />
                <template v-else>{{ searchQuery ? '无匹配结果' : '暂无订单' }}</template>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Pagination
      :page="pagination.current_page"
      :last-page="pagination.last_page"
      :total="pagination.total"
      :loading="loading"
      @prev="prev"
      @next="next"
    />
  </div>
</template>

<style scoped>
.ord-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-4);
  gap: var(--space-3);
}
.ord-head-right {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.ord-search {
  width: 220px;
}
.ord-type {
  width: 130px;
}
.ord-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.ord-table-wrap {
  overflow-x: auto;
}
.ord-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.ord-table th,
.ord-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
.ord-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.ord-table tbody tr:hover {
  background: var(--bg-hover);
}
.ord-no {
  font-size: var(--text-xs);
}
.ord-amount {
  font-weight: var(--font-medium);
  color: var(--accent);
}
.ord-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
.mono {
  font-family: var(--font-mono);
}
</style>
