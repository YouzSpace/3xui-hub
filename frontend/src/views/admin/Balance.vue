<script setup>
/**
 * Admin · 余额管理（用户余额查询 + 人工调账）。
 *
 * 从「用户」页详情弹窗里拆出来的独立页：用户列表 + 每个用户的余额 + 调账入口。
 * 页面本身不新增后端能力，复用现有接口：
 *  - 用户列表 GET /admin-api/users（分页 + 搜索，与用户页同一口径）
 *  - 单个余额 GET /admin-api/users/:id/balance（余额 + 最近流水，金额单位分）
 *  - 人工调账 POST /admin-api/users/:id/balance（amount 为分，正=加 / 负=扣）
 *
 * 两点口径：
 *  1. 用户列表接口不返回 users.balance，所以余额要按行单独查；一页 50 个，
 *     用 6 路并发拉，避免一次打 50 个请求，也避免串行等 50 轮。
 *  2. 调账金额输入按「元」收，提交前用 yuanToCents 换成整数分（后端只认分）。
 */
import { ref, computed, watch, onMounted, onActivated, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import Skeleton from '@/components/ui/Skeleton.vue'
import Pagination from '@/components/ui/Pagination.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { usePagedList } from '@/composables/usePagedList'
import { formatSignedYuan, formatYuan, yuanToCents } from '@/utils/money'

const BALANCE_LOG_TYPES = { recharge: '充值', consume: '消费', redeem: '兑换', admin: '管理员调整', rebate: '返佣' }
// 每页余额并发查询数：太高会瞬间打满后端，太低串行等待明显
const BALANCE_CONCURRENCY = 6
const RECENT_LOG_LIMIT = 5

const route = useRoute()
const router = useRouter()
const toast = useToast()

const users = ref([])
// 初始值取自 URL：直连 / 刷新 ?q=xxx 时搜索词直接进框并随首屏 load 生效
const searchQuery = ref(String(route.query.q ?? ''))
// 余额列排序：'' = 默认（按 id 倒序），asc / desc。点表头三次一循环。
// 排序在后端 SQL 层做（ORDER BY users.balance），整表排序后再分页，不是只排当前页。
const balanceOrder = ref(String(route.query.balance_order ?? ''))

const { page, loading, loadError, pagination, prev, next, fetchPage } = usePagedList()

/* ===== 列表 + 每行余额 ===== */
// id → { loading, error, balance }；列表换页 / 重新搜索时整块重建
const balances = ref({})
// 每次拉余额自增：换页后旧请求的响应不许写回（否则老页的余额会盖到新页）
let balanceGen = 0

const balanceOf = (id) => balances.value[id] || null

const fetchBalances = async (rows) => {
  // 后端按余额排序时已把每行 balance 一并回传，不必再逐行发 50 个请求
  if (balanceOrder.value && rows.length && rows.every((u) => u.balance !== undefined && u.balance !== null)) {
    const seed = {}
    for (const u of rows) seed[u.id] = { loading: false, error: '', balance: Number(u.balance || 0) }
    balances.value = seed
    return
  }
  const gen = ++balanceGen
  const seed = {}
  for (const u of rows) seed[u.id] = { loading: true, error: '', balance: null }
  balances.value = seed

  const queue = rows.map((u) => u.id)
  const runWorker = async () => {
    while (queue.length) {
      const id = queue.shift()
      try {
        const res = await adminApi.getUserBalance(id, { per_page: 1 })
        if (gen !== balanceGen) return
        balances.value[id] = { loading: false, error: '', balance: Number(res?.balance || 0) }
      } catch (e) {
        if (gen !== balanceGen) return
        balances.value[id] = { loading: false, error: e.message || '查询失败', balance: null }
      }
    }
  }
  const workers = []
  for (let i = 0; i < Math.min(BALANCE_CONCURRENCY, queue.length); i++) workers.push(runWorker())
  await Promise.all(workers)
}

const load = async () => {
  const q = searchQuery.value.trim()
  const extra = {}
  if (q) extra.search = q
  if (balanceOrder.value) extra.balance_order = balanceOrder.value
  const res = await fetchPage((params, config) => adminApi.listUsers(params, config), extra)
  if (!res) return
  users.value = Array.isArray(res) ? res : []
  await fetchBalances(users.value)
}

/* ===== 余额列排序（点表头三次一循环：默认 → 升序 → 降序 → 默认）===== */
const cycleBalanceOrder = () => {
  balanceOrder.value = balanceOrder.value === '' ? 'asc' : balanceOrder.value === 'asc' ? 'desc' : ''
  // 排序变了必须回到第 1 页，否则会停在「新排序的第 N 页」，看不到最值
  const nextQuery = { ...route.query }
  if (balanceOrder.value) nextQuery.balance_order = balanceOrder.value
  else delete nextQuery.balance_order
  delete nextQuery.page
  router.replace({ query: nextQuery })
  load()
}

const balanceOrderIcon = computed(() => (balanceOrder.value === 'asc' ? 'chevron-up' : 'chevron-down'))

// 只刷新当前页余额，不动列表（余额变动频繁时用）
const refreshingBalances = ref(false)
const refreshBalances = async () => {
  if (refreshingBalances.value || !users.value.length) return
  refreshingBalances.value = true
  try {
    await fetchBalances(users.value)
  } finally {
    refreshingBalances.value = false
  }
}

// 搜索走后端全表查询：词变就重查，并把页码按需拨回第 1 页（与用户页同一套逻辑）
let searchTimer = null
watch(searchQuery, () => {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    const q = searchQuery.value.trim()
    if (!page.value || page.value === 1) {
      const nextQuery = { ...route.query }
      if (q) nextQuery.q = q
      else delete nextQuery.q
      router.replace({ query: nextQuery })
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

onUnmounted(() => clearTimeout(searchTimer))

/* ===== 调账 ===== */
const adjustOpen = ref(false)
const adjustUser = ref(null)
const adjustLoading = ref(false)
const adjustBalance = ref(null) // { balance, logs, ... }
const adjustForm = ref({ direction: 'add', amount: '', note: '' })
const adjustSaving = ref(false)

const adjustTypeLabel = (t) => BALANCE_LOG_TYPES[t] || t
// 调账后余额不能为负：以弹窗里刚查到的余额为准
const adjustMaxSubCents = computed(() => Number(adjustBalance.value?.balance || 0))

const openAdjust = async (u) => {
  adjustUser.value = u
  adjustBalance.value = null
  adjustForm.value = { direction: 'add', amount: '', note: '' }
  adjustOpen.value = true
  adjustLoading.value = true
  try {
    adjustBalance.value = await adminApi.getUserBalance(u.id, { per_page: RECENT_LOG_LIMIT })
  } catch (e) {
    toast.error(e.message || '余额查询失败')
  } finally {
    adjustLoading.value = false
  }
}

const closeAdjust = () => {
  if (adjustSaving.value) return
  adjustOpen.value = false
}

const submitAdjust = async () => {
  if (!adjustUser.value?.id) return
  const cents = yuanToCents(adjustForm.value.amount)
  if (cents === null) {
    toast.error('金额格式不对，最多两位小数')
    return
  }
  if (cents <= 0) {
    toast.error('金额必须大于 0')
    return
  }
  const amount = adjustForm.value.direction === 'sub' ? -cents : cents
  if (amount < 0 && Number(adjustBalance.value?.balance || 0) + amount < 0) {
    toast.error('扣减后余额不能为负数')
    return
  }
  adjustSaving.value = true
  try {
    const res = await adminApi.adjustUserBalance(adjustUser.value.id, {
      amount,
      note: adjustForm.value.note.trim() || undefined,
    })
    toast.success(amount > 0 ? `已加 ${formatYuan(cents)}` : `已扣 ${formatYuan(cents)}`)
    // 弹窗内余额 / 流水就地更新；列表里那一行也同步，不用整页重拉
    adjustBalance.value = {
      ...(adjustBalance.value || {}),
      balance: Number(res?.balance ?? Number(adjustBalance.value?.balance || 0) + amount),
    }
    adjustForm.value = { direction: 'add', amount: '', note: '' }
    balances.value[adjustUser.value.id] = {
      loading: false,
      error: '',
      balance: Number(adjustBalance.value.balance || 0),
    }
    try {
      adjustBalance.value = await adminApi.getUserBalance(adjustUser.value.id, { per_page: RECENT_LOG_LIMIT })
    } catch (e) {
      // 流水刷新失败不影响调账结果：余额已经用接口返回值同步了
    }
  } catch (e) {
    toast.error(e.message || '调整余额失败')
  } finally {
    adjustSaving.value = false
  }
}

onMounted(load)
// 列表页在 AdminLayout 里被 keep-alive 缓存：切走再切回来要看到最新余额。
// 首次挂载时 activated 也会触发（此时 onMounted 已经拉过），所以第一跳忽略，
// 否则一进页面就是两轮「列表 + 50 个余额查询」。
let activatedOnce = false
onActivated(() => {
  if (!activatedOnce) {
    activatedOnce = true
    return
  }
  load()
})
</script>

<template>
  <div class="ab">
    <div class="ab-head">
      <div>
        <h1 class="ab-title">余额管理</h1>
        <p class="ab-sub">
          查看用户余额、按需人工调账；每次调账都写一条余额流水（含操作人与变动后余额）。
          兑换码在「兑换码」页生成。
        </p>
      </div>
      <div class="ab-tools">
        <Input v-model="searchQuery" placeholder="搜索 ID / 邮箱 / Token…" class="ab-search" />
        <Button variant="secondary" size="sm" :loading="refreshingBalances" :disabled="!users.length" @click="refreshBalances">
          <Icon name="refresh" :size="14" /> 刷新余额
        </Button>
      </div>
    </div>

    <Card padding="false">
      <div class="admin-table-wrap">
        <table class="admin-table">
          <thead>
            <tr>
              <th>ID</th>
              <th>邮箱</th>
              <th class="ab-sortable" :title="`点击切换余额排序（当前：${balanceOrder === 'asc' ? '升序' : balanceOrder === 'desc' ? '降序' : '默认'}）`" @click="cycleBalanceOrder">
                <span class="ab-sort-th">余额<Icon :name="balanceOrderIcon" :size="14" class="ab-sort-icon" :class="{ 'is-active': balanceOrder }" /></span>
              </th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="u in users" :key="u.id">
              <td class="mono ab-id">#{{ u.id }}</td>
              <td class="ab-email" :title="u.email || ''">{{ u.email || '—' }}</td>
              <td class="ab-balance-cell">
                <Skeleton v-if="balanceOf(u.id)?.loading" :rows="1" height="16px" />
                <span v-else-if="balanceOf(u.id)?.error" class="ab-dim" :title="balanceOf(u.id).error">查询失败</span>
                <span v-else class="ab-balance-num" :class="{ 'is-zero': !Number(balanceOf(u.id)?.balance || 0) }">
                  {{ formatYuan(balanceOf(u.id)?.balance || 0) }}
                </span>
              </td>
              <td>
                <Button variant="ghost" size="sm" title="人工调账（正=加 / 负=扣，必留流水）" @click="openAdjust(u)">
                  <Icon name="edit" :size="14" /> 调账
                </Button>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="4" class="ab-empty">
                加载失败：{{ loadError }}
                <Button variant="secondary" size="sm" @click="load">重试</Button>
              </td>
            </tr>
            <tr v-else-if="!users.length">
              <td colspan="4" class="ab-empty">
                <Skeleton v-if="loading" :rows="5" height="32px" />
                <template v-else>暂无用户</template>
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

    <!-- 调账（人工：正=加 / 负=扣，每次必留流水） -->
    <Modal v-model="adjustOpen" :title="adjustUser ? `调整余额 · #${adjustUser.id}` : '调整余额'" width="460px">
      <div class="ab-form">
        <div class="ab-user">
          <span class="ab-user-email">{{ adjustUser?.email || '—' }}</span>
          <span class="ab-user-balance">
            当前余额
            <b>{{ adjustLoading ? '…' : formatYuan(adjustBalance?.balance || 0) }}</b>
          </span>
        </div>

        <div class="ab-field-row">
          <label class="ab-field">
            <span>方向</span>
            <Select
              v-model="adjustForm.direction"
              :options="[
                { label: '增加', value: 'add' },
                { label: '扣减', value: 'sub' },
              ]"
            />
          </label>
          <label class="ab-field">
            <span>金额（元）</span>
            <Input v-model="adjustForm.amount" placeholder="例如 10.50" />
          </label>
        </div>
        <label class="ab-field">
          <span>备注（可选，写进流水）</span>
          <Input v-model="adjustForm.note" placeholder="例如 活动补偿 / 退款" />
        </label>
        <p class="ab-hint">
          扣减不能超过当前余额（{{ formatYuan(adjustMaxSubCents) }}）；提交后余额与流水会立即刷新。
        </p>

        <div class="ab-logs">
          <p class="ab-logs-title">最近流水</p>
          <p v-if="adjustLoading" class="ab-hint">加载中…</p>
          <ul v-else-if="adjustBalance?.logs?.length" class="ab-log-list">
            <li v-for="l in adjustBalance.logs" :key="l.id" class="ab-log">
              <span class="ab-log-amt" :class="Number(l.amount) < 0 ? 'is-out' : 'is-in'">
                {{ formatSignedYuan(l.amount) }}
              </span>
              <span class="ab-log-type">{{ adjustTypeLabel(l.type) }}</span>
              <span class="ab-dim">{{ formatYuan(l.balance_after) }}</span>
              <span v-if="l.note" class="ab-log-note" :title="l.note">{{ l.note }}</span>
            </li>
          </ul>
          <p v-else class="ab-hint">还没有余额变动</p>
        </div>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" :disabled="adjustSaving" @click="closeAdjust">关闭</Button>
        <Button variant="primary" size="sm" :loading="adjustSaving" @click="submitAdjust">保存调账</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.ab {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.ab-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}
.ab-title {
  margin: 0 0 4px;
  font-size: 20px;
  font-weight: 700;
  color: var(--text-primary);
}
.ab-sub {
  margin: 0;
  max-width: 640px;
  font-size: 13px;
  line-height: 1.6;
  color: var(--text-secondary);
}
.ab-tools {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.ab-search {
  width: 220px;
}

.ab-id {
  white-space: nowrap;
  color: var(--text-muted);
}
.ab-email {
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ab-dim {
  color: var(--text-muted);
  font-size: 13px;
}
.ab-balance-cell {
  white-space: nowrap;
}
/* 余额列排序：列头可点，箭头提示当前方向。
 * inline-flex 横排文字+箭头，否则内联 SVG 的基线会把箭头挤到下一行、把表头撑高。
 * 只给这个可排序列定宽，其余列保持自动布局不变。 */
.ab-sortable {
  cursor: pointer;
  user-select: none;
  white-space: nowrap;
  width: 130px;
}
.ab-sort-th {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  white-space: nowrap;
}
.ab-sortable:hover {
  color: var(--text-primary);
}
.ab-sort-icon {
  flex: none;
  opacity: 0.35;
}
.ab-sort-icon.is-active {
  opacity: 1;
  color: var(--accent);
}
.ab-balance-num {
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  color: var(--text-primary);
}
.ab-balance-num.is-zero {
  font-weight: 500;
  color: var(--text-muted);
}
.ab-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}

/* ===== 调账弹窗 ===== */
.ab-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.ab-user {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-2);
  flex-wrap: wrap;
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-md);
  background: var(--bg-elevated);
  font-size: var(--text-sm);
}
.ab-user-email {
  color: var(--text-primary);
  word-break: break-all;
}
.ab-user-balance {
  color: var(--text-secondary);
  white-space: nowrap;
}
.ab-user-balance b {
  margin-left: 6px;
  color: var(--text-primary);
  font-variant-numeric: tabular-nums;
}
.ab-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.ab-field-row {
  display: flex;
  gap: var(--space-3);
}
.ab-field-row .ab-field {
  flex: 1;
  min-width: 0;
}
.ab-hint {
  margin: 0;
  font-size: var(--text-xs);
  line-height: 1.6;
  color: var(--text-muted);
}
.ab-logs {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding-top: var(--space-2);
  border-top: 1px solid var(--border-subtle);
}
.ab-logs-title {
  margin: 0;
  font-size: var(--text-xs);
  font-weight: 600;
  color: var(--text-secondary);
}
.ab-log-list {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.ab-log {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  flex-wrap: wrap;
  font-size: var(--text-xs);
}
.ab-log-amt {
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}
.ab-log-amt.is-in {
  color: var(--success);
}
.ab-log-amt.is-out {
  color: var(--danger);
}
.ab-log-type {
  color: var(--text-secondary);
}
.ab-log-note {
  color: var(--text-muted);
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

@media (max-width: 720px) {
  .ab-tools {
    width: 100%;
  }
  .ab-search {
    flex: 1;
    width: auto;
    min-width: 0;
  }
  .ab-email {
    max-width: 160px;
  }
}

@media (max-width: 420px) {
  .ab-field-row {
    flex-direction: column;
    gap: var(--space-2);
  }
}

@media (max-width: 767px) {
  /* 全局规则给所有 *-table-wrap 都加了「← 左右滑动查看全部列 →」提示；
     这里四列在 375px 内能放下，别提示用户去滑一张不溢出的表（同 dsh-ut-table-wrap 的处理）。 */
  .admin-table-wrap::before {
    content: none;
  }
}

/* 375px：ID / 邮箱 / 余额 / 操作四列刚好塞进屏幕 —— 收一点横向留白，
   表格就不用左右拖（超长邮箱在 .ab-email 里已是省略号） */
@media (max-width: 480px) {
  .ab :deep(.admin-table th),
  .ab :deep(.admin-table td) {
    padding-left: 6px;
    padding-right: 6px;
  }
  /* 375px 下四列合计要 ≤ 内容区宽：邮箱列再收一点，完整地址走 title 提示 */
  .ab-email {
    max-width: 110px;
  }
}
</style>
