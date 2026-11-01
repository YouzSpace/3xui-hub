<script setup>
/**
 * Admin · 代理运营。
 *
 * 一个页面回答三个问题：
 *  1. 谁买了代理，还剩多少流量、什么时候到期（住宅账号 / 共享 socks 分两个 Tab）
 *  2. 到期了没有、要不要停用/延期（客服补偿）
 *  3. 有没有「钱付了但没发出权益」的订单（待补开，重试即可）
 */
import { ref, computed, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import Modal from '@/components/ui/Modal.vue'
import Select from '@/components/ui/Select.vue'
import Pagination from '@/components/ui/Pagination.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const loading = ref(false)
const stats = ref({})
const socksOptionsRaw = ref([])

// 三个列表各自分页：一个接口塞三张分页表，翻账号页时待补开也会跳页
const accounts = ref([])
const accPage = ref(1)
const accLast = ref(1)
const accTotal = ref(0)
const accStatus = ref('')
const accSearch = ref('')

const socksGroups = ref([])
const socksPage = ref(1)
const socksLast = ref(1)
const socksTotal = ref(0)

const pendingOrders = ref([])
const pendPage = ref(1)
const pendLast = ref(1)
const pendTotal = ref(0)

const tab = ref('accounts')
const busyId = ref(null)

// 凭据弹窗（重置密码后一次性展示）
const credOpen = ref(false)
const cred = ref({ username: '', password: '' })

// IP 展开
const ipState = ref({})

// 换线路弹窗
const moveOpen = ref(false)
const moveTarget = ref(null)
const moveSocksId = ref('')

const GB = 1024 ** 3
const fmtGB = (bytes) => {
  if (bytes === null || bytes === undefined) return '—'
  return `${(bytes / GB).toFixed(2)} GB`
}

const daysLeft = (iso) => {
  if (!iso) return null
  return Math.ceil((new Date(iso).getTime() - Date.now()) / 86400000)
}

const fmtDate = (iso) => (iso ? new Date(iso).toLocaleDateString('zh-CN') : '—')

const accStatusOptions = [
  { label: '全部状态', value: '' },
  { label: '生效中', value: 'active' },
  { label: '已停用', value: 'disabled' },
]

/** 统计 + socks 线路下拉（轻量，操作后也刷它，让顶部数字跟着变） */
const loadOverview = async () => {
  try {
    const d = await adminApi.proxyOpsOverview()
    stats.value = d?.stats || {}
    socksOptionsRaw.value = d?.socks_options || []
  } catch (e) {
    toast.error(e.message || '加载失败')
  }
}

const loadAccounts = async (page = accPage.value) => {
  loading.value = true
  try {
    const params = { page }
    if (accStatus.value) params.status = accStatus.value
    if (accSearch.value.trim()) params.search = accSearch.value.trim()
    const res = await adminApi.proxyOpsAccounts(params)
    accounts.value = res || []
    accPage.value = res?.pagination?.current_page || page
    accLast.value = res?.pagination?.last_page || 1
    accTotal.value = res?.pagination?.total || (res || []).length
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const loadSocks = async (page = socksPage.value) => {
  loading.value = true
  try {
    const res = await adminApi.proxyOpsSocksGroups({ page })
    socksGroups.value = res || []
    socksPage.value = res?.pagination?.current_page || page
    socksLast.value = res?.pagination?.last_page || 1
    socksTotal.value = res?.pagination?.total || (res || []).length
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const loadPending = async (page = pendPage.value) => {
  loading.value = true
  try {
    const res = await adminApi.proxyOpsPending({ page })
    pendingOrders.value = res || []
    pendPage.value = res?.pagination?.current_page || page
    pendLast.value = res?.pagination?.last_page || 1
    pendTotal.value = res?.pagination?.total || (res || []).length
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

/** 当前 Tab 的列表重载（操作完调它） */
const reloadCurrent = async () => {
  if (tab.value === 'accounts') await loadAccounts()
  else if (tab.value === 'socks') await loadSocks()
  else await loadPending()
}

const load = async () => {
  await Promise.all([loadOverview(), reloadCurrent()])
}

const switchTab = (key) => {
  tab.value = key
  reloadCurrent()
}

const applyAccFilter = () => {
  accPage.value = 1
  loadAccounts(1)
}

const socksOptions = computed(() => [
  { label: '请选择目标线路', value: '' },
  ...(socksOptionsRaw.value || []).map((s) => ({ label: s.label, value: s.id })),
])

const statusLabel = (s) => ({ active: '生效中', disabled: '已停用', expired: '已到期' }[s] || s)
const statusVariant = (s) => ({ active: 'success', disabled: 'warning', expired: 'warning' }[s] || 'default')

const run = async (id, fn, okMsg) => {
  busyId.value = id
  try {
    await fn()
    if (okMsg) toast.success(okMsg)
    await Promise.all([loadOverview(), reloadCurrent()])
  } catch (e) {
    toast.error(e.message || '操作失败')
  } finally {
    busyId.value = null
  }
}

/* ── 住宅账号操作 ─────────────────────── */

const pull = (a) => run(a.id, () => adminApi.proxyOpsPull(a.id), '已回采用量')

const disable = (a) => {
  if (!confirm(`停用「${a.user?.email || a.label}」的代理账号？\n会改随机密码并把额度压到 0.01GB，用户立刻连不上。`)) return
  run(a.id, () => adminApi.proxyOpsDisable(a.id), '已停用')
}

const resetPassword = async (a) => {
  if (!confirm(`重置「${a.user?.email || a.label}」的密码？\n旧密码立刻失效，新密码只显示这一次。`)) return
  busyId.value = a.id
  try {
    const r = await adminApi.proxyOpsResetPassword(a.id)
    cred.value = { username: r?.username || '', password: r?.password || '' }
    credOpen.value = true
    await Promise.all([loadOverview(), reloadCurrent()])
  } catch (e) {
    toast.error(e.message || '重置失败')
  } finally {
    busyId.value = null
  }
}

const extend = async (entitlementId, id, days = 30) => {
  const input = prompt('延长多少天？（客服补偿用，不产生订单）', String(days))
  if (input === null) return
  const n = parseInt(input, 10)
  if (!n || n < 1) {
    toast.error('天数必须是正整数')
    return
  }
  await run(id, () => adminApi.proxyOpsExtend(entitlementId, n), `已延长 ${n} 天`)
}

const removeAccount = (a) => {
  if (!confirm(`删除账号「${a.user?.email || a.label}」？\n会同时删除 Webshare 上游子用户，不可恢复。`)) return
  run(a.id, () => adminApi.proxyOpsDeleteAccount(a.id), '已删除')
}

const loadIps = async (a) => {
  if (ipState.value[a.id]?.list?.length) {
    ipState.value = { ...ipState.value, [a.id]: { list: [], loading: false, error: '' } }
    return
  }
  ipState.value = { ...ipState.value, [a.id]: { list: [], loading: true, error: '' } }
  try {
    const list = (await adminApi.proxyOpsAccountProxies(a.id)) || []
    ipState.value = { ...ipState.value, [a.id]: { list, loading: false, error: '' } }
  } catch (e) {
    ipState.value = { ...ipState.value, [a.id]: { list: [], loading: false, error: e.message || '获取失败' } }
  }
}

const copyCred = async () => {
  const text = `服务器：gateway.webshare.io\n端口：80\n用户名：${cred.value.username}\n密码：${cred.value.password}`
  try {
    await navigator.clipboard.writeText(text)
    toast.success('凭据已复制')
  } catch (e) {
    toast.error('复制失败，请手动选择')
  }
}

/* ── 共享 socks ──────────────────────── */

const openMove = (u) => {
  moveTarget.value = u
  moveSocksId.value = ''
  moveOpen.value = true
}

const submitMove = async () => {
  if (!moveSocksId.value) {
    toast.error('请选择目标线路')
    return
  }
  const id = moveTarget.value?.entitlement_id
  moveOpen.value = false
  await run(id, () => adminApi.proxyOpsMoveSocks(id, moveSocksId.value), '已换线路')
}

/* ── 待补开 ──────────────────────────── */

const retry = (o) => run(o.id, () => adminApi.proxyOpsRetryOrder(o.id), '已开通')

onMounted(() => {
  loadOverview()
  loadAccounts(1)
})
</script>

<template>
  <div class="po">
    <div class="po-head">
      <div>
        <h1 class="po-title">代理运营</h1>
        <p class="po-sub">谁买了代理、还剩多少、什么时候到期、该不该停 —— 都在这一页。</p>
      </div>
      <Button variant="ghost" size="sm" :loading="loading" @click="load"><Icon name="refresh" :size="15" /> 刷新</Button>
    </div>

    <!-- 统计 -->
    <div class="po-stats">
      <button class="po-stat" :class="{ active: tab === 'accounts' }" @click="switchTab('accounts')">
        <span class="po-stat-n">{{ stats.active ?? 0 }}</span>
        <span class="po-stat-l">生效中</span>
      </button>
      <button class="po-stat" @click="switchTab('accounts')">
        <span class="po-stat-n po-warn">{{ stats.expiring ?? 0 }}</span>
        <span class="po-stat-l">7 天内到期</span>
      </button>
      <button class="po-stat" @click="switchTab('accounts')">
        <span class="po-stat-n po-dim">{{ stats.expired ?? 0 }}</span>
        <span class="po-stat-l">已过期</span>
      </button>
      <button class="po-stat" :class="{ active: tab === 'pending' }" @click="switchTab('pending')">
        <span class="po-stat-n" :class="{ 'po-danger': (stats.pending_orders ?? 0) > 0 }">{{ stats.pending_orders ?? 0 }}</span>
        <span class="po-stat-l">待补开</span>
      </button>
    </div>

    <div class="po-tabs">
      <button class="po-tab" :class="{ active: tab === 'accounts' }" @click="switchTab('accounts')">
        住宅账号 <span class="po-tab-n">{{ accounts.length }}</span>
      </button>
      <button class="po-tab" :class="{ active: tab === 'socks' }" @click="switchTab('socks')">
        共享 socks <span class="po-tab-n">{{ socksGroups.length }}</span>
      </button>
      <button class="po-tab" :class="{ active: tab === 'pending' }" @click="switchTab('pending')">
        待补开 <span class="po-tab-n">{{ pendingOrders.length }}</span>
      </button>
    </div>

    <!-- 住宅账号 -->
    <Card v-if="tab === 'accounts'" padding="false" class="po-card">
      <div class="po-filter">
        <Select v-model="accStatus" :options="accStatusOptions" class="po-filter-sel" @change="applyAccFilter" />
        <input
          v-model.trim="accSearch"
          class="po-search"
          type="text"
          placeholder="搜 邮箱 / 用户 ID / 账号标识"
          maxlength="120"
          @keyup.enter="applyAccFilter"
        />
        <Button variant="ghost" size="sm" @click="applyAccFilter">搜索</Button>
        <span class="po-dim">共 {{ accTotal }} 个账号</span>
      </div>
      <div class="po-table-wrap">
        <table class="po-table">
          <thead>
            <tr>
              <th>用户</th>
              <th>规格</th>
              <th>已用 / 额度</th>
              <th>到期</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="a in accounts" :key="a.id">
              <tr>
                <td>
                  <div class="po-user">{{ a.user?.email || '—' }}</div>
                  <div class="po-dim">#{{ a.user?.id ?? '—' }} · {{ a.label }}</div>
                </td>
                <td class="po-dim">{{ a.sku_name || '—' }}</td>
                <td class="mono">
                  {{ fmtGB(a.used_bytes) }} / {{ a.limit_bytes ? fmtGB(a.limit_bytes) : fmtGB(a.proxy_limit * GB) }}
                </td>
                <td>
                  {{ fmtDate(a.expires_at) }}
                  <div v-if="daysLeft(a.expires_at) !== null" class="po-dim">
                    {{ daysLeft(a.expires_at) > 0 ? `还剩 ${daysLeft(a.expires_at)} 天` : '已到期' }}
                  </div>
                </td>
                <td><Badge :variant="statusVariant(a.status)">{{ statusLabel(a.status) }}</Badge></td>
                <td>
                  <div class="po-actions">
                    <Button size="sm" variant="ghost" :loading="busyId === a.id" @click="pull(a)">回采</Button>
                    <Button size="sm" variant="ghost" :disabled="busyId === a.id" @click="loadIps(a)">
                      {{ ipState[a.id]?.list?.length ? '收起 IP' : 'IP' }}
                    </Button>
                    <Button size="sm" variant="ghost" :disabled="busyId === a.id" @click="resetPassword(a)">重置密码</Button>
                    <Button
                      v-if="a.entitlement_id"
                      size="sm"
                      variant="ghost"
                      :disabled="busyId === a.id"
                      @click="extend(a.entitlement_id, a.id, 30)"
                    >延期</Button>
                    <Button v-if="a.status === 'active'" size="sm" variant="ghost" :disabled="busyId === a.id" @click="disable(a)">停用</Button>
                    <Button size="sm" variant="ghost" :disabled="busyId === a.id" @click="removeAccount(a)">删除</Button>
                  </div>
                </td>
              </tr>
              <tr v-if="ipState[a.id]">
                <td colspan="6" class="po-ips">
                  <span v-if="ipState[a.id].loading" class="po-dim">加载中…</span>
                  <span v-else-if="ipState[a.id].error" class="po-dim">{{ ipState[a.id].error }}</span>
                  <div v-else-if="ipState[a.id].list.length" class="po-ip-list">
                    <span v-for="(ip, i) in ipState[a.id].list" :key="i" class="po-ip mono">
                      {{ ip.address }}:{{ ip.port }}
                      <span class="po-dim">{{ ip.country }}{{ ip.city ? ' · ' + ip.city : '' }}</span>
                    </span>
                  </div>
                  <span v-else class="po-dim">没有拿到 IP，可能是上游账号/额度问题。</span>
                </td>
              </tr>
            </template>
            <tr v-if="!accounts.length">
              <td colspan="6" class="po-empty">
                还没有卖出住宅代理。卖出后（用户付款并成功开号）会自动出现在这里。
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <Pagination
        :page="accPage"
        :last-page="accLast"
        :total="accTotal"
        :loading="loading"
        @prev="loadAccounts(accPage - 1)"
        @next="loadAccounts(accPage + 1)"
      />
    </Card>

    <!-- 共享 socks -->
    <template v-if="tab === 'socks'">
      <Card v-for="g in socksGroups" :key="g.socks_id" class="po-card">
        <div class="po-sock-head">
          <span class="po-sock-name mono">{{ g.host }}:{{ g.port }}</span>
          <span class="po-dim">{{ g.country || '—' }}</span>
          <Badge :variant="g.socks_active ? 'success' : 'warning'">{{ g.socks_active ? '线路启用' : '线路已停用' }}</Badge>
          <span class="po-dim">{{ g.user_count }} 人在用 / 共 {{ g.total_count }} 条权益</span>
          <span v-if="g.next_expiry" class="po-dim">最近到期 {{ fmtDate(g.next_expiry) }}</span>
        </div>

        <div class="po-table-wrap">
          <table class="po-table">
            <thead>
              <tr>
                <th>用户</th>
                <th>规格</th>
                <th>到期</th>
                <th>状态</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="u in g.users" :key="u.entitlement_id">
                <td>
                  <div class="po-user">{{ u.email || '—' }}</div>
                  <div class="po-dim">#{{ u.user_id }}</div>
                </td>
                <td class="po-dim">{{ u.sku_name || '—' }}</td>
                <td>
                  {{ fmtDate(u.expires_at) }}
                  <div v-if="daysLeft(u.expires_at) !== null" class="po-dim">
                    {{ daysLeft(u.expires_at) > 0 ? `还剩 ${daysLeft(u.expires_at)} 天` : '已到期' }}
                  </div>
                </td>
                <td><Badge :variant="statusVariant(u.status)">{{ statusLabel(u.status) }}</Badge></td>
                <td>
                  <div class="po-actions">
                    <Button size="sm" variant="ghost" :disabled="busyId === u.entitlement_id" @click="extend(u.entitlement_id, u.entitlement_id, 30)">延期</Button>
                    <Button size="sm" variant="ghost" :disabled="busyId === u.entitlement_id" @click="openMove(u)">换线路</Button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </Card>

      <Pagination
        v-if="socksTotal > socksGroups.length || socksPage > 1"
        :page="socksPage"
        :last-page="socksLast"
        :total="socksTotal"
        :loading="loading"
        @prev="loadSocks(socksPage - 1)"
        @next="loadSocks(socksPage + 1)"
      />

      <Card v-if="!socksGroups.length" class="po-card">
        <p class="po-empty">还没有卖出共享 socks。</p>
      </Card>
    </template>

    <!-- 待补开 -->
    <Card v-if="tab === 'pending'" padding="false" class="po-card">
      <p class="po-tip">钱已经付了、但上游开号失败没发出权益的订单。点「重试开通」再跑一次开通流程。</p>
      <div class="po-table-wrap">
        <table class="po-table">
          <thead>
            <tr>
              <th>订单号</th>
              <th>用户</th>
              <th>规格</th>
              <th>金额</th>
              <th>支付时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="o in pendingOrders" :key="o.id">
              <td class="mono">{{ o.order_no }}</td>
              <td>
                <div class="po-user">{{ o.user?.email || '—' }}</div>
                <div class="po-dim">#{{ o.user?.id ?? '—' }}</div>
              </td>
              <td class="po-dim">
                {{ o.sku_name || '—' }}
                <Badge v-if="o.renewal" variant="info" size="sm">续费</Badge>
              </td>
              <td class="mono">¥{{ o.amount }}</td>
              <td class="po-dim">{{ fmtDate(o.paid_at) }}</td>
              <td>
                <Button size="sm" variant="primary" :loading="busyId === o.id" @click="retry(o)">重试开通</Button>
              </td>
            </tr>
            <tr v-if="!pendingOrders.length">
              <td colspan="6" class="po-empty">没有待补开的订单，一切正常。</td>
            </tr>
          </tbody>
        </table>
      </div>
      <Pagination
        :page="pendPage"
        :last-page="pendLast"
        :total="pendTotal"
        :loading="loading"
        @prev="loadPending(pendPage - 1)"
        @next="loadPending(pendPage + 1)"
      />
    </Card>

    <!-- 新凭据 -->
    <Modal v-model="credOpen" title="新凭据（只显示这一次）" width="420px">
      <div class="po-cred">
        <div class="po-cred-row"><span class="po-dim">用户名</span><span class="mono">{{ cred.username }}</span></div>
        <div class="po-cred-row"><span class="po-dim">密码</span><span class="mono">{{ cred.password }}</span></div>
        <p class="po-tip">把这两项发给用户，同时告诉他：服务器 gateway.webshare.io，端口 80，用任一 IP。</p>
      </div>
      <template #footer>
        <div class="po-foot">
          <Button variant="ghost" size="sm" @click="credOpen = false">关闭</Button>
          <CopyButton :get-text="() => `服务器：gateway.webshare.io\n端口：80\n用户名：${cred.username}\n密码：${cred.password}`" label="复制凭据" />
        </div>
      </template>
    </Modal>

    <!-- 换线路 -->
    <Modal v-model="moveOpen" title="换到另一条线路" width="420px">
      <div class="po-move">
        <p class="po-dim">用户：{{ moveTarget?.email }}（#{{ moveTarget?.user_id }}）</p>
        <Select v-model="moveSocksId" :options="socksOptions" />
        <p class="po-tip">换线路后用户端「我的代理」立刻显示新线路的地址与账号。</p>
      </div>
      <template #footer>
        <div class="po-foot">
          <Button variant="ghost" size="sm" @click="moveOpen = false">取消</Button>
          <Button variant="primary" size="sm" @click="submitMove">确认切换</Button>
        </div>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.po { max-width: 1180px; }
.po-head { display: flex; align-items: flex-start; justify-content: space-between; gap: var(--space-3); margin-bottom: var(--space-3); }
.po-title { margin: 0 0 4px; font-size: var(--text-xl); font-weight: var(--font-bold); }
.po-sub { margin: 0; color: var(--text-muted); font-size: var(--text-sm); }

.po-stats { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: var(--space-3); margin-bottom: var(--space-3); }
.po-stat {
  display: flex; flex-direction: column; gap: 2px; align-items: flex-start;
  padding: var(--space-3) var(--space-4); border: 1px solid var(--border-subtle);
  border-radius: 12px; background: #fff; cursor: pointer; font-family: inherit; text-align: left;
}
.po-stat.active { border-color: var(--accent); }
.po-stat-n { font-size: var(--text-xl); font-weight: var(--font-bold); }
.po-stat-l { font-size: var(--text-xs); color: var(--text-muted); }
.po-warn { color: #d97706; }
.po-dim { color: var(--text-muted); font-size: var(--text-xs); }
.po-danger { color: #dc2626; }

.po-tabs { display: flex; gap: 4px; margin-bottom: var(--space-3); border-bottom: 1px solid var(--border-subtle); }
.po-tab {
  padding: 8px 14px; border: none; background: transparent; cursor: pointer;
  font-size: var(--text-sm); color: var(--text-muted); font-family: inherit;
  border-bottom: 2px solid transparent; margin-bottom: -1px;
}
.po-tab.active { color: var(--text-primary); border-bottom-color: var(--accent); font-weight: var(--font-medium); }
.po-tab-n { color: var(--text-muted); font-size: var(--text-xs); }

.po-card { margin-bottom: var(--space-4); }
.po-tip { margin: var(--space-3) var(--space-4); color: var(--text-muted); font-size: var(--text-xs); line-height: 1.6; }
.po-filter {
  display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap;
  padding: var(--space-3) var(--space-4); border-bottom: 1px solid var(--border-subtle);
}
.po-filter-sel { width: 130px; }
.po-search {
  height: 32px; padding: 0 12px; border: 1px solid var(--border-subtle);
  border-radius: 8px; font-size: var(--text-sm); font-family: inherit; outline: none; width: 220px;
}
.po-search:focus { border-color: var(--accent); }
.po-table-wrap { overflow-x: auto; }
.po-table { width: 100%; border-collapse: collapse; font-size: var(--text-sm); }
.po-table th, .po-table td {
  padding: var(--space-3) var(--space-4); text-align: left; vertical-align: top;
  border-bottom: 1px solid var(--border-subtle); white-space: nowrap;
}
.po-table th { color: var(--text-muted); font-weight: var(--font-medium); font-size: var(--text-xs); }
.po-table tbody tr:last-child td { border-bottom: none; }
.po-user { font-weight: var(--font-medium); }
.po-actions { display: flex; flex-wrap: wrap; gap: 4px; }
.po-empty { text-align: center; color: var(--text-muted); font-size: var(--text-sm); padding: var(--space-6); }

.po-sock-head { display: flex; align-items: center; gap: var(--space-3); flex-wrap: wrap; margin-bottom: var(--space-2); }
.po-sock-name { font-weight: var(--font-medium); }

.po-ips { background: var(--bg-hover); }
.po-ip-list { display: flex; flex-wrap: wrap; gap: var(--space-3); }
.po-ip { font-size: var(--text-xs); }

.po-cred { display: flex; flex-direction: column; gap: var(--space-2); }
.po-cred-row { display: flex; align-items: center; gap: var(--space-3); font-size: var(--text-sm); }
.po-cred-row .po-dim { width: 56px; flex: 0 0 56px; }
.po-foot { display: flex; justify-content: flex-end; gap: var(--space-2); width: 100%; }
.po-move { display: flex; flex-direction: column; gap: var(--space-3); }
</style>
