<script setup>
/**
 * Admin · 返佣记录（只读流水）。
 *
 * 后端：internal/handler/admin/affiliate.go（GET /admin-api/affiliate/rebates）
 *  - 金额一律「分」，展示用 utils/money 转元；
 *  - rate 是发放当时落库的 decimal(5,4)（0.1000 = 10%），留痕用，不跟当前配置联动；
 *  - 每页默认 20 条、上限 100（后端口径），翻页由本页自己管。
 *
 * 本页只读：返佣的发放发生在支付链路（被邀请人购买套餐）。要停返佣，
 * 把 site_configs.affiliate_rebate_rate 改成 0，不在这个页面操作。
 *
 * 分页没用 usePagedList：那套约定是「data 是数组、分页信息挂在数组的 .pagination」，
 * 返佣接口回的是 { list, total, page, per_page }（与兑换码列表同形），
 * 所以这里手管 page / total，翻页控件仍复用 Pagination 组件。
 */
import { ref, computed, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import Skeleton from '@/components/ui/Skeleton.vue'
import Pagination from '@/components/ui/Pagination.vue'
import { adminApi } from '@/api/admin'
import { formatYuan } from '@/utils/money'

const PER_PAGE = 20

const list = ref([])
const total = ref(0)
const page = ref(1)
const loading = ref(false)
const loadError = ref('')

const pageCount = computed(() => Math.max(1, Math.ceil(total.value / PER_PAGE)))

// 0.1000 → '10%'；0.1250 → '12.5%'（最多两位小数，去掉末尾的 0）
const rateLabel = (rate) => {
  const n = Number(rate)
  if (!Number.isFinite(n)) return '—'
  return `${Number((n * 100).toFixed(2))}%`
}

const emailOf = (email) => email || '—'

const load = async () => {
  loading.value = true
  loadError.value = ''
  try {
    const res = await adminApi.listAffiliateRebates({ page: page.value, per_page: PER_PAGE })
    list.value = Array.isArray(res?.list) ? res.list : []
    total.value = Number(res?.total || 0)
  } catch (e) {
    list.value = []
    total.value = 0
    loadError.value = e.message || '加载失败'
  } finally {
    loading.value = false
  }
}

const goPage = (delta) => {
  const next = page.value + delta
  if (next < 1 || next > pageCount.value) return
  page.value = next
  load()
}

onMounted(load)
</script>

<template>
  <div class="afr">
    <div class="afr-head">
      <div>
        <h1 class="afr-title">返佣记录</h1>
        <p class="afr-sub">
          每笔订单发放给邀请人的返佣流水（只读）。金额按订单实付计算，比例取发放当时的配置。
        </p>
      </div>
      <Button variant="secondary" size="sm" :loading="loading" @click="load">
        <Icon name="refresh" :size="14" /> 刷新
      </Button>
    </div>

    <Card padding="false">
      <div class="admin-table-wrap">
        <table class="admin-table">
          <thead>
            <tr>
              <th>邀请人</th>
              <th>被邀请人</th>
              <th>订单号</th>
              <th>返佣金额</th>
              <th>比例</th>
              <th>时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in list" :key="r.id">
              <td class="afr-email" :title="r.inviter_email || ''">{{ emailOf(r.inviter_email) }}</td>
              <td class="afr-email" :title="r.invitee_email || ''">{{ emailOf(r.invitee_email) }}</td>
              <td class="afr-order" :title="r.order_no || ''">{{ r.order_no || '—' }}</td>
              <td class="afr-amount">{{ formatYuan(r.amount) }}</td>
              <td class="afr-dim">{{ rateLabel(r.rate) }}</td>
              <td class="afr-dim afr-time">{{ r.created_at || '—' }}</td>
            </tr>
            <tr v-if="loadError">
              <td colspan="6" class="afr-empty">
                加载失败：{{ loadError }}
                <Button variant="secondary" size="sm" @click="load">重试</Button>
              </td>
            </tr>
            <tr v-else-if="!list.length">
              <td colspan="6" class="afr-empty">
                <Skeleton v-if="loading" :rows="5" height="32px" />
                <template v-else>还没有返佣记录，好友下单后这里会出现流水</template>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Pagination
      :page="page"
      :last-page="pageCount"
      :total="total"
      :loading="loading"
      @prev="goPage(-1)"
      @next="goPage(1)"
    />
  </div>
</template>

<style scoped>
.afr {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.afr-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}
.afr-title {
  margin: 0 0 4px;
  font-size: 20px;
  font-weight: 700;
  color: var(--text-primary);
}
.afr-sub {
  margin: 0;
  max-width: 640px;
  font-size: 13px;
  line-height: 1.6;
  color: var(--text-secondary);
}

.afr-email {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.afr-order {
  max-width: 180px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--text-secondary);
}
.afr-amount {
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}
.afr-dim {
  color: var(--text-muted);
  font-size: 13px;
  white-space: nowrap;
}
.afr-time {
  font-variant-numeric: tabular-nums;
}
.afr-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}

@media (max-width: 720px) {
  .afr-head > :first-child {
    width: 100%;
  }
  .afr-email {
    max-width: 140px;
  }
  .afr-order {
    max-width: 120px;
  }
}
</style>
