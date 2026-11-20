<script setup>
/**
 * Admin 用户管理：表格 + 创建/编辑 Modal + 删除 + 重置当月流量 + 续费。
 * 套餐联动：选套餐自动填充流量和时间，不手动填。
 * 无套餐 = 没有流量、不可用。
 * 列表分页：每页 50 条，页码走 ?page=（刷新/切页回来不丢页）。
 */
import { ref, computed, watch, onMounted, onActivated, onDeactivated, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import Skeleton from '@/components/ui/Skeleton.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import Pagination from '@/components/ui/Pagination.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { usePagedList } from '@/composables/usePagedList'

const route = useRoute()
const router = useRouter()
const toast = useToast()
const users = ref([])
const plans = ref([])
// 初始值取自 URL：直连 / 刷新 ?q=xxx 时搜索词直接进框并随首屏 load 生效（不能只靠下面
// 的 watch —— 它没有 immediate，首次进入不触发）
const searchQuery = ref(String(route.query.q ?? ''))

const { page, loading, loadError, pagination, hasPrev, hasNext, prev, next, fetchPage } = usePagedList()


const modalOpen = ref(false)
const editing = ref(null)
const form = ref({ email: '', plan_id: null, enabled: true })
// 保存提交中：按钮转圈并防重入（节点多时 updateUser 要同步等各节点，不能被反复点击）
const saving = ref(false)
const saved = ref(false)
const GB = 1024 * 1024 * 1024

// 用户详情（查看/复制 token 与订阅地址）
const detailOpen = ref(false)
const detail = ref(null)
const detailLoading = ref(false)

const viewDetail = async (u) => {
  detailOpen.value = true
  detailLoading.value = true
  detail.value = null
  try {
    detail.value = await adminApi.getUser(u.id)
  } catch (e) {
    detail.value = u
    toast.error('拉取详情失败，显示列表数据')
  } finally {
    detailLoading.value = false
  }
}

const detailSubUrl = computed(() => {
  if (!detail.value?.token) return ''
  return `${location.origin}/api/sub/${detail.value.token}`
})

// 管理员重置用户密码（改密会作废该用户全部登录态）
const pwdOpen = ref(false)
const pwdSaving = ref(false)
const pwdForm = ref({ password: '', confirm: '' })

const openPasswordReset = () => {
  pwdForm.value = { password: '', confirm: '' }
  pwdOpen.value = true
}

const savePassword = async () => {
  if (pwdForm.value.password.length < 6) { toast.error('密码至少 6 位'); return }
  if (pwdForm.value.password !== pwdForm.value.confirm) { toast.error('两次密码不一致'); return }
  pwdSaving.value = true
  try {
    await adminApi.resetUserPassword(detail.value.id, pwdForm.value.password)
    toast.success('密码已重置，该用户旧登录已失效')
    pwdOpen.value = false
  } catch (e) {
    toast.error(e.message || '重置失败')
  } finally { pwdSaving.value = false }
}

const copyText = async (text, label) => {
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`${label}已复制`)
  } catch (e) {
    toast.error('复制失败')
  }
}

const fmtBytes = (b) => {
  if (!b || b < 0 || !Number.isFinite(b)) return '0'
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB']
  const raw = Math.floor(Math.log(b || 1) / Math.log(1024))
  const i = Math.max(0, Math.min(raw, u.length - 1))
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)} ${u[i]}`
}

// 选中的套餐对象
const selectedPlan = computed(() => plans.value.find(p => p.id === form.value.plan_id) || null)
const planOptions = computed(() => [
  { label: '不绑定套餐（无流量）', value: null },
  ...plans.value.map((p) => ({
    label: `${p.name}（${p.type === 'period' ? `周期${p.months}个月` : '总量'}）`,
    value: p.id,
  })),
])

// 套餐选择器要一次看到全部套餐（分页会漏掉后面的选项），显式 all=1；不参与翻页取消
const loadPlans = async () => {
  try {
    plans.value = await adminApi.listPlans({ all: 1 })
  } catch (e) {
    toast.error('套餐选择器加载失败：' + (e.message || '未知错误'))
  }
}

const load = async () => {
  const q = searchQuery.value.trim()
  // 绑定了节点筛选就只查那台机器上的用户（'' = 全部）
  const extra = {}
  if (q) extra.search = q
  if (nodeFilter.value !== '' && nodeFilter.value != null) extra.node_id = nodeFilter.value
  const res = await fetchPage(
    (params, config) => adminApi.listUsers(params, config),
    Object.keys(extra).length ? extra : null,
  )
  if (res) users.value = res
}

// ── 绑定节点筛选 / 改绑 ──────────────────────────────────────
// 节点候选一次拉全（几十台，不分页）：筛选下拉与改绑弹窗共用同一份数据。
const nodeOptions = ref([])
const nodeFilter = ref('')

const nodeOptionLabel = (n) => (n.region_name ? `${n.name}（${n.region_name}）` : n.name)

// 首项「全部节点」= 不筛选（node_id 不带给后端）
const nodeFilterOptions = computed(() => [
  { label: '全部节点', value: '' },
  ...nodeOptions.value.map((n) => ({ label: nodeOptionLabel(n), value: n.id })),
])

const loadNodeOptions = async () => {
  try {
    const list = await adminApi.listUserNodeOptions()
    nodeOptions.value = Array.isArray(list) ? list : []
  } catch (e) {
    nodeOptions.value = []
    toast.error('节点列表加载失败：' + (e.message || '未知错误'))
  }
}

/**
 * 「绑定节点」列：后端给 node_names（字符串数组）+ node_ids；都缺失时显示「—」。
 */
const boundNodesText = (u) => {
  const raw = u?.node_names
  if (raw == null || raw === '') return '—'
  const arr = Array.isArray(raw) ? raw : String(raw).split(',')
  const names = arr.map((v) => (typeof v === 'string' ? v : v?.name)).filter((v) => v != null && v !== '')
  return names.length ? names.join('、') : '—'
}

// 用户已绑定的节点 id 列表（改绑弹窗用它排掉「目标节点」里已有的项）
const boundNodeIds = (u) => (Array.isArray(u?.node_ids) ? u.node_ids : [])

// 改绑弹窗
const rebindOpen = ref(false)
const rebindTarget = ref(null)
const rebindTo = ref(null)
const rebindSaving = ref(false)

// from_node_id 的取法：只绑了 1 台 → 把它换掉；0 台 → 直接加；多台 → 传空（只加不减），
// 免得点一次改绑就把用户在其他机器上的绑定悄悄删掉。
const rebindFromId = computed(() => {
  const ids = boundNodeIds(rebindTarget.value)
  return ids.length === 1 ? ids[0] : null
})
const rebindTargetOptions = computed(() => {
  const bound = boundNodeIds(rebindTarget.value)
  return nodeOptions.value
    .filter((n) => !bound.includes(n.id))
    .map((n) => ({ label: nodeOptionLabel(n), value: n.id }))
})

const openRebind = (u) => {
  rebindTarget.value = u
  rebindTo.value = null
  rebindSaving.value = false
  rebindOpen.value = true
}

const confirmRebind = async () => {
  const u = rebindTarget.value
  if (!u || rebindSaving.value) return
  if (!rebindTo.value) {
    toast.error('请选择目标节点')
    return
  }
  rebindSaving.value = true
  try {
    await adminApi.changeUserNodes(u.id, {
      from_node_id: rebindFromId.value,
      to_node_id: rebindTo.value,
    })
    toast.success('已改绑')
    rebindOpen.value = false
    await load()
  } catch (e) {
    toast.error(e.message || '改绑失败')
  } finally {
    rebindSaving.value = false
  }
}

// 换筛选 = 换结果集，页码必须回第 1 页（否则会停在一个空页上）
watch(nodeFilter, () => {
  if (page.value !== 1) {
    router.push({ query: { ...route.query, page: undefined } })
  } else {
    load()
  }
})

// 搜索走后端全表查询：词变就重查，并把页码按需拨回第 1 页。
// 不用前端过滤是因为前端手里只有当前页 50 条，对象在别的页就永远搜不到。
let searchTimer = null
watch(searchQuery, () => {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    const q = searchQuery.value.trim()
    // 还在第 1 页：只改 query 不写历史记录，但数据必须重拉
    if (!page.value || page.value === 1) {
      const next = { ...route.query }
      if (q) next.q = q
      else delete next.q
      router.replace({ query: next })
      load()
    } else {
      // 在第 N 页：跳回第 1 页，watch(page) 会触发 load
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

const openCreate = () => {
  editing.value = null
  form.value = { email: '', plan_id: null, enabled: true }
  modalOpen.value = true
}

const openEdit = async (u) => {
  editing.value = u
  form.value = {
    email: u.email || '',
    plan_id: u.plan_id || null,
    enabled: u.enabled,
  }
  modalOpen.value = true
}

const save = async () => {
  if (saving.value || saved.value) return
  saving.value = true
  try {
    // 用户协议已不再是可选项：后端渲染/订阅不再按 users.protocol 过滤，所有用户都拿到
    // 全部协议线路。新建时由后端补默认值（vless），编辑时 payload 不带该字段即不改。
    const payload = {
      email: form.value.email,
      plan_id: form.value.plan_id || null,
      enabled: form.value.enabled,
    }
    if (editing.value) {
      await adminApi.updateUser(editing.value.id, payload)
      toast.success('已更新')
    } else {
      const u = await adminApi.createUser(payload)
      toast.success(`已创建（Token: ${u.token}）`)
    }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    saved.value = true
    setTimeout(() => {
      saved.value = false
      modalOpen.value = false
    }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const remove = async (u) => {
  if (!confirm(`删除用户 ${u.email || u.id}？`)) return
  try {
    await adminApi.deleteUser(u.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

const resetTraffic = async (u) => {
  if (!confirm(`重置 ${u.email || u.id} 的当月流量？`)) return
  try {
    await adminApi.resetUserTraffic(u.id)
    toast.success('当月流量已重置')
    await load()
  } catch (e) {
    toast.error(e.message || '重置失败')
  }
}

const renewUser = async (u) => {
  if (!confirm(`续费 ${u.email || u.id}？将重置总流量并重新启用。`)) return
  try {
    await adminApi.renewUser(u.id)
    toast.success('续费成功')
    await load()
  } catch (e) {
    toast.error(e.message || '续费失败')
  }
}

// 重置订阅：换 token + uuid，旧订阅链接与旧节点凭据立即失效；账号/套餐/流量不动。
// 二次确认弹窗（说明后果）→ 成功后在同一弹窗里展示新链接，便于当场发回给用户。
const subResetOpen = ref(false)
const subResetTarget = ref(null)
const subResetSaving = ref(false)
const subResetResult = ref(null)

const openSubReset = (u) => {
  subResetTarget.value = u
  subResetResult.value = null
  subResetSaving.value = false
  subResetOpen.value = true
}

// 新订阅地址 = 当前站点域名 + 后端回传的 sub_path
const subResetUrl = computed(() => {
  if (!subResetResult.value?.sub_path) return ''
  return `${location.origin}${subResetResult.value.sub_path}`
})

// 弹窗里指代目标用户：优先邮箱，没有就写 #id
const subResetLabel = computed(() => {
  const u = subResetTarget.value
  if (!u) return ''
  return u.email || `用户 #${u.id}`
})

const confirmSubReset = async () => {
  const u = subResetTarget.value
  if (!u || subResetSaving.value) return
  subResetSaving.value = true
  try {
    const res = await adminApi.resetUserSubscription(u.id)
    subResetResult.value = res || null
    // 详情弹窗如果正开着同一个用户，里面的 token/订阅地址也同步换新，避免展示旧链接
    if (detail.value && detail.value.id === u.id && res?.token) {
      detail.value = { ...detail.value, token: res.token, uuid: res.uuid || detail.value.uuid }
    }
    toast.success('订阅已重置，旧链接已失效')
    await load()
  } catch (e) {
    toast.error(e.message || '重置订阅失败')
  } finally {
    subResetSaving.value = false
  }
}

const syncingAll = ref(false)
let syncPollTimer = null

const stopSyncWatch = () => {
  if (syncPollTimer) {
    window.clearTimeout(syncPollTimer)
    syncPollTimer = null
  }
}

// 同步完成：弹对应提示 + 刷新用户表格显示最新流量，恢复按钮可点
const finishSyncWatch = (task) => {
  stopSyncWatch()
  const status = task?.status
  if (status === 'succeeded') toast.success('流量同步完成')
  else if (status === 'partial') toast.info('流量同步部分完成')
  else if (status === 'failed') toast.error('流量同步失败')
  else toast.info('同步任务处理中，稍后可手动刷新查看')
  syncingAll.value = false
  load()
}

// 盯守指定 task_id 到终态（提交时拿到；已存在任务则盯那个）。任务进行中按钮保持禁用。
const startSyncWatch = (taskId, alreadyRunning) => {
  stopSyncWatch()
  if (alreadyRunning) toast.info('已有同步任务正在执行')
  else toast.success('同步任务已提交')

  const POLL = 5000
  const MAX = 300000
  let elapsed = 0
  const check = async () => {
    if (!taskId) {
      finishSyncWatch(null)
      return
    }
    let task = null
    try {
      const list = await adminApi.listAsyncTasks()
      task = (Array.isArray(list) ? list : []).find(t => t.id === taskId) || null
    } catch {
      task = null
    }
    if (task && ['succeeded', 'partial', 'failed'].includes(task.status)) {
      finishSyncWatch(task)
      return
    }
    elapsed += POLL
    if (elapsed >= MAX) {
      finishSyncWatch(task)
      return
    }
    syncPollTimer = window.setTimeout(check, POLL)
  }
  // 首次延迟一拍再查（提交后任务通常还在 running）
  syncPollTimer = window.setTimeout(check, POLL)
}

const syncAllTraffic = async () => {
  if (syncingAll.value) return
  syncingAll.value = true
  try {
    const res = await adminApi.syncTraffic()
    // 进入盯守：任务进行中按钮禁用，完成后自动弹提示并刷新表格
    startSyncWatch(res?.task_id, !!res?.already_running)
  } catch (e) {
    toast.error(e.message || '同步失败')
    syncingAll.value = false
  }
}

/** 流量列显示：按套餐类型区分 */
const trafficDisplay = (u) => {
  if (u.plan_type === 'period') {
    return `${fmtBytes(u.monthly_traffic_used)} / ${fmtBytes(u.monthly_traffic_limit)} | 总 ${fmtBytes(u.traffic_used)} / ${fmtBytes(u.traffic_limit)}`
  }
  if (u.plan_type === 'total') {
    return `${fmtBytes(u.traffic_used)} / ${fmtBytes(u.traffic_limit)}`
  }
  return '无流量'
}

// ── 静默轮询 ──────────────────────────────────────────────
// 目的：agent 每 5s 就把流量推上来落库，但本页只在「进入/切回」时拉一次，
// 盯着看数字永远是旧的。这里每 15s 旁路刷一次当前页，**不置 loading、不 abort 在飞请求**，
// 避免表格闪骨架或打断用户正在进行的翻页。
//
// 与主加载（load → usePagedList.fetchPage）的区别：直接调接口覆盖 users.value，
// 不动 pagination/cursor 状态 —— 页码没变，分页元信息也不需要考虑重算。
const SILENT_POLL_MS = 15000
let silentTimer = null
let silentInFlight = false

const silentRefresh = async () => {
  // 有弹窗/弹层、正在搜索输入、正在同步、主加载中、上一轮还没回来、页面在后台 → 跳过本轮。
  // 跳过不是错误：下一轮自然接着跑。
  if (
    silentInFlight ||
    document.hidden ||
    modalOpen.value ||
    detailOpen.value ||
    saving.value ||
    syncingAll.value ||
    loading.value ||
    searchTimer
  ) return

  silentInFlight = true
  try {
    const q = searchQuery.value.trim()
    const pageAt = page.value
    const res = await adminApi.listUsers({ page: pageAt, ...(q ? { search: q } : {}) })
    // 期间用户翻页/搜索变了就丢弃这次结果，避免用旧页数据覆盖新页
    if (res && page.value === pageAt && searchQuery.value.trim() === q) {
      users.value = res
    }
  } catch {
    /* 静默失败：下一轮重试，不打扰正在看页面的人 */
  } finally {
    silentInFlight = false
  }
}

const startSilentPoll = () => {
  stopSilentPoll()
  silentTimer = window.setInterval(silentRefresh, SILENT_POLL_MS)
}
const stopSilentPoll = () => {
  if (silentTimer) {
    window.clearInterval(silentTimer)
    silentTimer = null
  }
}

// 页面可见性：切到后台停表（省请求），切回立即拉一次再续表
const onVisibility = () => {
  if (document.hidden) stopSilentPoll()
  else { silentRefresh(); startSilentPoll() }
}

onMounted(() => { load(); loadPlans(); loadNodeOptions(); startSilentPoll(); document.addEventListener('visibilitychange', onVisibility) })
onActivated(() => { load(); loadPlans(); loadNodeOptions(); startSilentPoll() })
onDeactivated(stopSilentPoll)
onUnmounted(() => { stopSyncWatch(); stopSilentPoll(); document.removeEventListener('visibilitychange', onVisibility) })
</script>

<template>
  <div class="au">
    <div class="au-head">
      <h1 class="ch-page-title">用户</h1>
      <div class="au-head-right">
        <Input v-model="searchQuery" placeholder="搜索 ID/邮箱/Token..." class="au-search" />
        <!-- 绑定节点筛选：选中后只显示绑在该节点上的用户 -->
        <Select
          v-model="nodeFilter"
          :options="nodeFilterOptions"
          placeholder="绑定节点"
          wrapper-class="au-node-filter"
        />
        <Button variant="secondary" size="sm" :disabled="syncingAll" @click="syncAllTraffic"><Icon name="refresh" :size="16" /> {{ syncingAll ? '同步中...' : '同步流量' }}</Button>
        <Button variant="primary" size="sm" @click="openCreate"><Icon name="plus" :size="16" /> 新建用户</Button>
      </div>
    </div>

    <Card padding="false">
      <div class="au-table-wrap">
        <table class="ch-table">
          <thead>
            <tr>
              <th>ID</th>
              <th>邮箱</th>
              <th>套餐</th>
              <th>绑定节点</th>
              <th>流量</th>
              <th>到期</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="u in users" :key="u.id">
              <td class="mono">{{ u.id }}</td>
              <td>{{ u.email || '—' }}</td>
              <td>
                <Badge v-if="u.plan_name" :variant="u.plan_type === 'period' ? 'vless' : 'trojan'" size="sm">
                  {{ u.plan_name }}
                </Badge>
                <span v-else class="au-muted">无</span>
              </td>
              <td class="au-bound-nodes">{{ boundNodesText(u) }}</td>
              <td class="mono au-traffic-cell">{{ trafficDisplay(u) }}</td>
              <td>{{ u.expired_at ? new Date(u.expired_at).toLocaleDateString() : '永久' }}</td>
              <td><Badge :variant="u.enabled ? 'success' : 'danger'" dot>{{ u.enabled ? '启用' : '禁用' }}</Badge></td>
              <td>
                <div class="ch-actions">
                  <Button variant="ghost" size="sm" title="查看/复制 Token" @click="viewDetail(u)"><Icon name="copy" :size="14" /></Button>
                  <Button variant="ghost" size="sm" title="编辑" @click="openEdit(u)"><Icon name="edit" :size="14" /></Button>
                  <Button v-if="u.plan_type === 'period'" variant="ghost" size="sm" title="重置当月流量" @click="resetTraffic(u)"><Icon name="refresh" :size="14" /></Button>
                  <Button variant="ghost" size="sm" title="改绑节点" @click="openRebind(u)"><Icon name="server" :size="14" /></Button>
                  <Button v-if="u.plan_type === 'total'" variant="ghost" size="sm" title="续费" @click="renewUser(u)"><Icon name="refresh" :size="14" /></Button>
                  <Button variant="ghost" size="sm" title="删除" @click="remove(u)"><Icon name="trash" :size="14" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="8" class="ch-empty">
                加载失败：{{ loadError }}
                <Button variant="secondary" size="sm" @click="load">重试</Button>
              </td>
            </tr>
            <tr v-else-if="!users.length">
              <td colspan="8" class="ch-empty">
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
      :has-next="hasNext"
      :loading="loading"
      @prev="prev"
      @next="next"
    />

    <Modal v-model="modalOpen" :title="editing ? '编辑用户' : '新建用户'" width="480px">
      <div class="ch-form">
        <label class="ch-field">
          <span>邮箱</span>
          <Input v-model="form.email" placeholder="user@example.com" />
        </label>
        <label class="ch-field">
          <span>套餐模板</span>
          <Select v-model="form.plan_id" :options="planOptions" />
        </label>
        <!-- 套餐预览 -->
        <div v-if="selectedPlan" class="au-plan-preview">
          <template v-if="selectedPlan.type === 'period'">
            <p>到期时间：绑定后自动计算（当前 + {{ selectedPlan.months }}个月）</p>
            <p>当月限额：{{ fmtBytes(selectedPlan.monthly_traffic) }}</p>
            <p>周期总流量：{{ fmtBytes(selectedPlan.period_traffic) }}</p>
          </template>
          <template v-else>
            <p>总流量：{{ fmtBytes(selectedPlan.total_traffic) }}</p>
            <p>有效期：永久</p>
          </template>
        </div>
        <label class="ch-field ch-field-row">
          <input v-model="form.enabled" type="checkbox" />
          <span>启用</span>
        </label>

      </div>
      <template #footer>
        <Button variant="secondary" size="sm" :disabled="saving" @click="modalOpen = false">取消</Button>
        <Button variant="primary" size="sm" :loading="saving" :success="saved" @click="save">保存</Button>
      </template>
    </Modal>

    <!-- 用户详情 -->
    <Modal v-model="detailOpen" :title="detail ? `用户 #${detail.id} 详情` : '用户详情'" width="520px">
      <div v-if="detailLoading" class="au-detail-loading"><Skeleton :rows="3" height="28px" /></div>
      <div v-else-if="detail" class="au-detail">
        <div class="au-detail-row">
          <span class="au-detail-label">邮箱</span>
          <span class="au-detail-value">{{ detail.email || '—' }}</span>
        </div>
        <div class="au-detail-row">
          <span class="au-detail-label">套餐</span>
          <span class="au-detail-value">
            <Badge v-if="detail.plan_name" :variant="detail.plan_type === 'period' ? 'vless' : 'trojan'" size="sm">
              {{ detail.plan_name }}
            </Badge>
            <span v-else>无套餐（无流量）</span>
          </span>
        </div>
        <div v-if="detail.plan_type === 'period' && detail.plan_months > 1 && detail.next_traffic_reset_at" class="au-detail-row">
          <span class="au-detail-label">流量重置</span>
          <span class="au-detail-value">
            {{ Math.max(0, Math.ceil((new Date(detail.next_traffic_reset_at) - new Date()) / (1000 * 60 * 60 * 24))) }} 天后
          </span>
        </div>
        <div class="au-detail-row">
          <span class="au-detail-label">登录 Token</span>
          <span class="au-detail-value mono">{{ detail.token }}</span>
          <CopyButton :text="detail.token" label="复制" />
        </div>
        <div class="au-detail-row au-detail-row-stack">
          <!-- 「重置订阅」按钮紧跟在「订阅地址」四个字右边：它作用的对象就是这条链接，
               放在这一行比塞进列表操作列更好找，也不占操作列的位置。 -->
          <span class="au-detail-label au-detail-label-inline">
            订阅地址
            <Button
              variant="ghost"
              size="sm"
              title="重置订阅（换 token/uuid，旧订阅链接立即失效）"
              @click="openSubReset(detail)"
            >
              <Icon name="refresh" :size="12" /> 重置
            </Button>
          </span>
          <code class="au-detail-code">{{ detailSubUrl }}</code>
          <CopyButton :text="detailSubUrl" label="复制订阅" />
          <Button variant="secondary" size="sm" @click="openPasswordReset">修改密码</Button>
        </div>
        <div class="au-detail-hint">
          把「登录 Token」给用户，用户在 <code>/login</code> 页粘贴即可登录；
          或直接给「订阅地址」导入代理客户端。
        </div>
      </div>
    </Modal>

    <!-- 管理员重置用户密码 -->
    <Modal v-model="pwdOpen" title="修改用户密码" width="420px">
      <div class="ch-form">
        <label class="ch-field">
          <span>新密码</span>
          <Input v-model="pwdForm.password" type="password" placeholder="至少 6 位" />
        </label>
        <label class="ch-field">
          <span>确认新密码</span>
          <Input v-model="pwdForm.confirm" type="password" placeholder="再次输入新密码" />
        </label>
        <p class="au-pwd-hint">保存后该用户的旧登录态会全部失效，需用新密码重新登录。</p>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="pwdOpen = false">取消</Button>
        <Button variant="primary" size="sm" :loading="pwdSaving" @click="savePassword">保存</Button>
      </template>
    </Modal>

    <!-- 重置订阅：确认（说明旧链接立即失效）→ 成功后展示新链接 -->
    <Modal v-model="subResetOpen" title="重置订阅" width="480px">
      <div v-if="!subResetResult" class="ch-form">
        <div class="au-sub-warn">
          <Icon name="warning" :size="16" />
          <div>
            <p>即将重置 <strong>{{ subResetLabel }}</strong> 的订阅：</p>
            <ul>
              <li>订阅链接与节点凭据（token / uuid）会换成新的，<strong>旧订阅链接立即失效</strong>，已导入旧链接的客户端会断开；</li>
              <li>账号、密码、套餐、已用流量、到期时间都不受影响，当前登录态也不掉。</li>
            </ul>
            <p>只在链接泄露或需要作废旧客户端时才用。</p>
          </div>
        </div>
      </div>
      <div v-else class="ch-form">
        <div class="au-sub-ok">
          <Icon name="check" :size="16" />
          <span>已重置，旧链接已失效。把下面的新订阅地址发给用户：</span>
        </div>
        <div class="au-sub-url-row">
          <code class="au-detail-code">{{ subResetUrl }}</code>
          <CopyButton :text="subResetUrl" label="复制订阅" />
        </div>
        <p class="au-detail-hint">
          用户端已登录的页面会自动拿到新地址（无需手动通知）；这里复制是为了直接转发给用户。
        </p>
      </div>
      <template #footer>
        <template v-if="!subResetResult">
          <Button variant="secondary" size="sm" :disabled="subResetSaving" @click="subResetOpen = false">取消</Button>
          <Button variant="danger" size="sm" :loading="subResetSaving" @click="confirmSubReset">确认重置</Button>
        </template>
        <Button v-else variant="primary" size="sm" @click="subResetOpen = false">完成</Button>
      </template>
    </Modal>

    <!-- 改绑节点：当前绑定只读展示 + 目标节点下拉 -->
    <Modal v-model="rebindOpen" title="改绑节点" width="460px">
      <div class="au-form">
        <label class="au-field">
          <span>用户</span>
          <div class="au-rebind-current">{{ rebindTarget?.email || `用户 #${rebindTarget?.id}` }}</div>
        </label>
        <label class="au-field">
          <span>当前节点</span>
          <div class="au-rebind-current">{{ boundNodesText(rebindTarget) }}</div>
        </label>
        <label class="au-field">
          <span>目标节点</span>
          <Select v-model="rebindTo" :options="rebindTargetOptions" placeholder="选择目标节点" />
        </label>
        <p class="au-detail-hint">
          <template v-if="boundNodeIds(rebindTarget).length > 1">
            该用户当前绑了 {{ boundNodeIds(rebindTarget).length }} 台节点：本次为新增一条绑定，原有绑定保持不变。
          </template>
          <template v-else>
            改绑后该用户的节点配置会迁到目标节点，原节点上的绑定会被移除；用户端订阅地址不变。
          </template>
        </p>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" :disabled="rebindSaving" @click="rebindOpen = false">取消</Button>
        <Button variant="primary" size="sm" :loading="rebindSaving" @click="confirmRebind">确认改绑</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Users.css"></style>
