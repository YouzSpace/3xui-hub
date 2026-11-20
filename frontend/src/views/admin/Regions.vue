<script setup>
/**
 * Admin 地区与调度：地区列表 + 展开看节点 + 容量水位 + 迁移/均衡入口。
 *
 * 口径（与后端一致）：
 *  - 地区 = 一组节点；用户建号时只在「自己绑定的节点」里分配（user_nodes）。
 *  - 未设地区的节点按「全量下发」处理 —— 升级后一个地区都不建，行为与升级前完全一致。
 *  - 迁移 / 均衡都是后台任务：接口立即回 job_id，进度靠轮询 /migrations/jobs/:job_id。
 *
 * 后端接口由并行任务实现，字段缺失时一律兜底成「—」/「不限」，页面不能崩。
 */
import { ref, computed, onMounted, onActivated, onBeforeUnmount } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import ProgressBar from '@/components/ui/ProgressBar.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { useAsyncData } from '@/composables/useAsyncData'
import { useConfirm } from '@/composables/useConfirm'
import { useCrudForm } from '@/composables/useCrudForm'

const toast = useToast()
const { confirm } = useConfirm()

// 页面主体数据（地区列表 + 节点全量）交给 useAsyncData：切页 abort 旧请求、
// keep-alive 退场后不落数据、卸载后不写 ref —— 这几个边界不用每个页面各想一遍。
const {
  data: pageData,
  loading,
  error: loadError,
  run: runLoad,
} = useAsyncData(fetchPage, { initial: { regions: [], nodes: [] }, fallbackMessage: '加载失败' })
const regions = computed(() => pageData.value?.regions ?? [])
// 节点全量列表：未归属节点块 + 迁移时的目标节点都从这里取
const nodesAll = computed(() => pageData.value?.nodes ?? [])
// /regions/overview 的原始响应；取不到时用地区列表兜底（并行开发期间后端可能还没这个接口）
const overview = ref(null)

const expandedId = ref(null)
const regionNodes = ref([])
const regionNodesLoading = ref(false)

// ===== 小工具 =====
const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : null)
/** 百分比：分母缺失 / 为 0 时返回 null（页面显示「—」，不是 0%） */
const pct = (used, total) => {
  const u = num(used) ?? 0
  const t = num(total)
  if (t == null || t <= 0) return null
  return Math.round((u / t) * 100)
}
const statusVariant = (s) => ({ online: 'success', offline: 'danger', maintenance: 'warning' }[s] || 'info')
const driverLabel = (t) => (t === '3xui' ? '3x-ui' : (t || '—'))
const regionName = (id) => regions.value.find((r) => r.id === id)?.name || ''
const capText = (v) => (v == null || v === 0 ? '不限' : v)

// ===== 总览 =====
const sumBy = (key) => regions.value.reduce((acc, r) => acc + (num(r[key]) ?? 0), 0)
const totalNodes = computed(() => num(overview.value?.node_total) ?? nodesAll.value.length)
const totalCapacity = computed(() => num(overview.value?.capacity_total) ?? sumBy('capacity_total'))
const totalUsed = computed(() => num(overview.value?.capacity_used) ?? sumBy('capacity_used'))
const totalUsage = computed(() => pct(totalUsed.value, totalCapacity.value))

/** 节点使用率：优先用后端算好的 agree_ratio（0~1），否则按 已绑/容量 现算 */
const nodeUsage = (n) => {
  const ratio = num(n?.agree_ratio)
  if (ratio != null) return Math.round(ratio * 100)
  return pct(n?.bound_users, n?.capacity)
}

const unassignedNodes = computed(() => nodesAll.value.filter((n) => n.region_id == null))

// ===== 容量告警（>=85% 黄、>=95% 红）=====
// 接口 GET /regions/alerts 的 data = { alerts: [...], counts: { warn, critical } }。
// 后端还没这个端点（404）时 counts 归零、两条告警条都不渲染 —— 宁可什么都不显示，也不编告警。
const alertCounts = ref({ warn: 0, critical: 0 })

const loadAlerts = async () => {
  try {
    const res = await adminApi.regionsAlerts()
    const counts = res?.counts || {}
    alertCounts.value = { warn: num(counts.warn) ?? 0, critical: num(counts.critical) ?? 0 }
  } catch (e) {
    alertCounts.value = { warn: 0, critical: 0 }
  }
}

// ===== 容量推荐（每台节点一条）=====
// 接口 GET /nodes/:id/capacity-recommend 是逐节点的：只问当前页面上真正渲染的节点，
// 一次最多 50 台（节点多时宁可不显示，也不打一串请求）。
// 后端字段可能是 Go 名（Suggest）也可能带 json tag（suggest），两种都认。
const recById = ref({})
// 整批都失败 = 后端还没这个端点：本次会话不再追问，免得每次刷新打一串 404
const recUnavailable = ref(false)
const REC_MAX = 50

const field = (o, ...names) => {
  for (const k of names) if (o && o[k] != null) return o[k]
  return null
}

const loadRecs = async (list) => {
  if (recUnavailable.value) return
  const ids = (list || []).map((n) => n?.id).filter((id) => id != null).slice(0, REC_MAX)
  if (!ids.length) return
  try {
    const res = await Promise.allSettled(ids.map((id) => adminApi.capacityRecommend(id)))
    const next = { ...recById.value }
    res.forEach((r, i) => {
      if (r.status === 'fulfilled' && r.value) next[ids[i]] = r.value
    })
    recById.value = next
    if (res.every((r) => r.status === 'rejected')) recUnavailable.value = true
  } catch (e) {
    // 连方法都没有（后端刚合入 / mock 环境）：静默降级，建议列显示「—」
    recUnavailable.value = true
  }
}

const recOf = (n) => recById.value[n?.id] || null
/** 建议容量：拿不到或 <=0 都算「没有建议」，返回 null（模板据此显示「—」） */
const suggestOf = (n) => {
  const s = num(field(recOf(n), 'Suggest', 'suggest'))
  return s != null && s > 0 ? s : null
}
const confOf = (n) => String(field(recOf(n), 'Confidence', 'confidence') || '').toLowerCase()
// 置信度图标：high ✅ / medium ⚠️ / low 💡（后端给了别的值也按 low 处理）
const CONF_ICON = { high: '✅', medium: '⚠️', low: '💡' }
const confIcon = (n) => CONF_ICON[confOf(n)] || '💡'
const recReason = (n) => String(field(recOf(n), 'Reason', 'reason') || '')

const adoptingId = ref(null)
/** 一键采用：把建议值写成该节点的容量上限（PUT /nodes/:id/capacity） */
const adopt = async (n) => {
  const s = suggestOf(n)
  if (!s) return
  adoptingId.value = n.id
  try {
    await adminApi.setNodeCapacity(n.id, s)
    toast.success(`已把「${n.name}」的容量上限设为 ${s}`)
    await load()
  } catch (e) {
    toast.error(e.message || '采用建议失败')
  } finally {
    adoptingId.value = null
  }
}

// ===== 加载 =====
const loadOverview = async () => {
  try {
    overview.value = await adminApi.regionOverview()
  } catch (e) {
    // 总览接口没就绪不影响列表：按地区汇总兜底（节点总数用已加载节点数）
    overview.value = null
  }
}

const loadRegionNodes = async (id) => {
  regionNodesLoading.value = true
  try {
    const rn = await adminApi.listRegionNodes(id)
    // 后端返回 { region_id, region_name, nodes: [...] }
    regionNodes.value = Array.isArray(rn) ? rn : (rn?.nodes ?? [])
    // 展开的这批节点才需要建议容量（逐节点接口，只为看得见的行问）
    await loadRecs(regionNodes.value)
  } catch (e) {
    regionNodes.value = []
    toast.error(e.message || '加载地区节点失败')
  } finally {
    regionNodesLoading.value = false
  }
}

const refreshExpanded = async () => {
  if (expandedId.value) await loadRegionNodes(expandedId.value)
}

/**
 * 整页数据装载：主体（地区 + 节点）失败就整页报错；总览 / 告警 / 建议容量各自降级，
 * 接口没就绪时只少一块，不影响列表。
 */
async function fetchPage() {
  const [rs, ns] = await Promise.all([adminApi.listRegions(), adminApi.listNodes({ all: 1 })])
  // 后端 /regions 返回 { regions: [...] }，/nodes 直接返回数组 —— 两种形态都要兼容
  const nextRegions = Array.isArray(rs) ? rs : (rs?.regions ?? [])
  const nextNodes = Array.isArray(ns) ? ns : []
  await loadOverview()
  await loadAlerts()
  // 未归属节点表一进页面就渲染，它的建议容量也要问（地区内的节点等展开时再问）
  await loadRecs(nextNodes.filter((n) => n.region_id == null))
  // 展开的地区可能已被删掉，收起；还在就刷新它的节点
  if (expandedId.value && !nextRegions.some((r) => r.id === expandedId.value)) {
    expandedId.value = null
    regionNodes.value = []
  } else {
    await refreshExpanded()
  }
  return { regions: nextRegions, nodes: nextNodes }
}

const load = async () => {
  if (loading.value) return
  await runLoad()
  // 失败不吞：把具体原因弹出来（哪个接口、后端给的原话）
  if (loadError.value) toast.error(loadError.value)
}

const toggle = async (r) => {
  if (expandedId.value === r.id) {
    expandedId.value = null
    regionNodes.value = []
    return
  }
  expandedId.value = r.id
  regionNodes.value = []
  await loadRegionNodes(r.id)
}

// ===== 地区新建 / 编辑 / 删除 =====
const formOpen = ref(false)

// 表单状态（值、校验、提交中、回填）交给 useCrudForm；toast 措辞留在页面里
const {
  form,
  submitting: saving,
  submit: saveRegion,
  reset: resetForm,
  setForm,
} = useCrudForm({
  initial: () => ({ id: null, name: '', sort: 0 }),
  validate: (f) => ({ name: String(f.name ?? '').trim() ? '' : '地区名不能为空' }),
  onInvalid: (errs) => toast.error(Object.values(errs)[0]),
  onSubmit: async (f) => {
    const name = String(f.name ?? '').trim()
    const sort = Number(f.sort)
    const payload = {
      name,
      sort: Number.isFinite(sort) ? sort : 0,
    }
    if (f.id) await adminApi.updateRegion(f.id, payload)
    else await adminApi.createRegion(payload)
  },
  onSuccess: async () => {
    toast.success(form.value.id ? '已保存' : '已新建地区')
    formOpen.value = false
    await load()
  },
  onError: (e) => toast.error(e?.message || '保存失败'),
})

const openCreate = () => {
  resetForm()
  // 新建默认排到最后一个
  setForm({ sort: regions.value.length })
  formOpen.value = true
}
const openEdit = (r) => {
  setForm({ id: r.id, name: r.name, sort: r.sort ?? 0 })
  formOpen.value = true
}

const removeRegion = async (r) => {
  if (!(await confirm({ title: '删除地区', message: `删除地区「${r.name}」？`, danger: true }))) return
  try {
    await adminApi.deleteRegion(r.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    // 地区下还有节点时后端会拒绝，msg 形如「该地区还有 N 台节点，请先移出」—— 原样展示
    toast.error(e.message || '删除失败')
  }
}

// ===== 后台任务（迁移 / 均衡）进度 =====
const jobOpen = ref(false)
const jobTitle = ref('后台任务')
const job = ref(null)
let jobTimer = null

const jobPercent = computed(() => {
  const total = num(job.value?.total) ?? 0
  const done = num(job.value?.done) ?? 0
  return total > 0 ? Math.round((done / total) * 100) : 0
})

const stopPoll = () => {
  if (jobTimer) {
    clearTimeout(jobTimer)
    jobTimer = null
  }
}

const isJobDone = (j) => ['finished', 'failed', 'error', 'done', 'success'].includes(String(j?.status || '').toLowerCase())

const startJob = async (jobId, label) => {
  if (!jobId) {
    toast.error('后端没有返回任务号，请重试')
    return
  }
  jobTitle.value = label
  job.value = { status: 'running', total: 0, done: 0, failed: 0 }
  jobOpen.value = true
  stopPoll()

  const tick = async () => {
    try {
      const j = (await adminApi.migrationJob(jobId)) || {}
      job.value = j
      if (!isJobDone(j)) {
        jobTimer = setTimeout(tick, 1500)
        return
      }
      stopPoll()
      const failed = num(j.failed) ?? 0
      if (String(j.status || '').toLowerCase() === 'failed' || j.error) {
        toast.error(j.error || `${label}失败`)
      } else {
        toast.success(`${label}完成：${num(j.done) ?? 0} / ${num(j.total) ?? 0}` + (failed ? `，失败 ${failed}` : ''))
      }
      await load()
    } catch (e) {
      stopPoll()
      toast.error(e.message || '查询任务进度失败')
    }
  }

  await tick()
}

// ===== 迁移（节点 → 另一台节点） =====
const migOpen = ref(false)
const migSource = ref(null)
const migTargetId = ref(null)
const migQty = ref('')
const migSubmitting = ref(false)

const openMigrate = (node) => {
  migSource.value = node
  migTargetId.value = null
  migQty.value = ''
  migOpen.value = true
}

const targetOptions = computed(() =>
  nodesAll.value
    .filter((n) => n.id !== migSource.value?.id)
    .map((n) => {
      const rn = regionName(n.region_id)
      return { value: n.id, label: `${n.name}${rn ? `（${rn}）` : '（未归属）'}` }
    }),
)

const submitMigrate = async () => {
  if (!migSource.value) return
  if (!migTargetId.value) {
    toast.error('请选择目标节点')
    return
  }
  // 留空 = 全部迁移（后端约定 count <= 0）
  const raw = String(migQty.value).trim()
  const count = raw === '' ? 0 : Number(raw)
  if (!Number.isInteger(count) || count < 0) {
    toast.error('迁移数量需为整数，或留空 = 全部')
    return
  }
  migSubmitting.value = true
  try {
    const res = await adminApi.migrateNode(migSource.value.id, { to_node_id: migTargetId.value, count })
    migOpen.value = false
    await startJob(res?.job_id, '迁移')
  } catch (e) {
    toast.error(e.message || '迁移失败')
  } finally {
    migSubmitting.value = false
  }
}

// ===== 均衡（地区内按容量重新分配） =====
const balancing = ref(null)

const balance = async (r) => {
  balancing.value = r.id
  try {
    const plan = await adminApi.balanceRegion(r.id, { dry: true })
    const count =
      num(plan?.moved) ?? num(plan?.count) ?? (Array.isArray(plan?.plan) ? plan.plan.length : 0)
    if (!count) {
      toast.info('当前没有需要均衡的用户')
      return
    }
    if (!(await confirm({ title: '均衡用户', message: `计划迁移 ${count} 个用户，确定执行？` }))) return
    const res = await adminApi.balanceRegion(r.id, { dry: false, limit: count })
    await startJob(res?.job_id, '均衡')
  } catch (e) {
    toast.error(e.message || '均衡失败')
  } finally {
    balancing.value = null
  }
}

// ===== 未归属节点：设置归属 / 容量 =====
const assignOpen = ref(false)
const assignNode = ref(null)
const assignRegionId = ref(null)
const assignCapacity = ref('')
const assignSaving = ref(false)

const regionOptions = computed(() => [
  { value: null, label: '不设置（全量下发）' },
  ...regions.value.map((r) => ({ value: r.id, label: r.name })),
])

const openAssign = (n) => {
  assignNode.value = n
  assignRegionId.value = n.region_id ?? null
  assignCapacity.value = n.capacity == null ? '' : String(n.capacity)
  assignOpen.value = true
}

const saveAssign = async () => {
  const id = assignNode.value?.id
  if (!id) return
  const raw = String(assignCapacity.value).trim()
  let capacity = null
  if (raw !== '') {
    capacity = Number(raw)
    if (!Number.isInteger(capacity) || capacity <= 0) {
      toast.error('容量需为正整数，或留空 = 不限')
      return
    }
  }
  assignSaving.value = true
  try {
    await adminApi.setNodeRegion(id, assignRegionId.value ?? null)
    await adminApi.setNodeCapacity(id, capacity)
    toast.success('已保存')
    assignOpen.value = false
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    assignSaving.value = false
  }
}

// 布局是 keep-alive：onMounted 已覆盖首屏，onActivated 只在「切回来」时刷新，
// 避免首挂载时 onMounted + onActivated 各拉一遍。
let firstActivate = true
onMounted(load)
onActivated(() => {
  if (firstActivate) {
    firstActivate = false
    return
  }
  load()
})
onBeforeUnmount(stopPoll)
</script>

<template>
  <div class="rg">
    <div class="rg-head">
      <h1 class="rg-title">地区与调度</h1>
      <div class="rg-head-actions">
        <Button variant="secondary" size="sm" :loading="loading" @click="load">
          <Icon name="refresh" :size="16" /> 刷新
        </Button>
        <Button variant="primary" size="sm" @click="openCreate">
          <Icon name="plus" :size="16" /> 新建地区
        </Button>
      </div>
    </div>

    <!-- 容量告警：>=95% 红、>=85% 黄。counts 来自 /regions/alerts；接口没就绪时两条都不出 -->
    <div v-if="alertCounts.critical > 0" class="rg-alert rg-alert--critical" data-test="alert-critical">
      <Icon name="activity" :size="16" />
      <span>{{ alertCounts.critical }} 个节点/地区容量超过 95%，需要尽快扩容或把用户迁走</span>
    </div>
    <div v-if="alertCounts.warn > 0" class="rg-alert rg-alert--warn" data-test="alert-warn">
      <Icon name="activity" :size="16" />
      <span>{{ alertCounts.warn }} 个节点/地区容量超过 85%，建议提前扩一台</span>
    </div>

    <p class="rg-hint">
      地区把节点分组：用户建号时只在<strong>自己绑定的节点</strong>里分配。未设地区的节点按
      <strong>全量下发</strong>处理，一个地区都不建时行为与升级前完全一致。
    </p>

    <div class="rg-stats">
      <Card class="rg-stat">
        <p class="rg-stat-label">节点总数</p>
        <p class="rg-stat-value">{{ totalNodes }}</p>
      </Card>
      <Card class="rg-stat">
        <p class="rg-stat-label">总容量</p>
        <p class="rg-stat-value">{{ capText(totalCapacity) }}</p>
      </Card>
      <Card class="rg-stat">
        <p class="rg-stat-label">已绑用户</p>
        <p class="rg-stat-value">{{ totalUsed }}</p>
      </Card>
      <Card class="rg-stat">
        <p class="rg-stat-label">使用率</p>
        <p class="rg-stat-value">{{ totalUsage == null ? '—' : `${totalUsage}%` }}</p>
        <ProgressBar v-if="totalUsage != null" :percent="totalUsage" />
      </Card>
    </div>

    <Card :padding="false" class="rg-card">
      <div class="rg-table-wrap">
        <table class="rg-table">
          <thead>
            <tr>
              <th>地区</th>
              <th>节点数</th>
              <th>已绑 / 容量</th>
              <th class="rg-col-usage">使用率</th>
              <th class="rg-col-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="r in regions" :key="r.id">
              <tr class="rg-row" @click="toggle(r)">
                <td>
                  <span class="rg-name">
                    <Icon :name="expandedId === r.id ? 'chevron-down' : 'chevron-right'" :size="16" />
                    {{ r.name }}
                  </span>
                </td>
                <td class="mono">{{ r.node_count ?? 0 }}</td>
                <td class="mono">{{ r.capacity_used ?? 0 }} / {{ capText(r.capacity_total) }}</td>
                <td class="rg-col-usage">
                  <template v-if="pct(r.capacity_used, r.capacity_total) != null">
                    <ProgressBar
                      :percent="pct(r.capacity_used, r.capacity_total)"
                      :label="`${pct(r.capacity_used, r.capacity_total)}%`"
                    />
                  </template>
                  <span v-else class="rg-muted">—</span>
                </td>
                <td class="rg-col-act" @click.stop>
                  <div class="rg-actions">
                    <Button
                      variant="ghost"
                      size="sm"
                      title="均衡该地区的用户"
                      :loading="balancing === r.id"
                      @click="balance(r)"
                    >
                      <Icon name="traffic" :size="16" />
                    </Button>
                    <Button variant="ghost" size="sm" title="编辑地区" @click="openEdit(r)">
                      <Icon name="edit" :size="16" />
                    </Button>
                    <Button variant="ghost" size="sm" title="删除地区" @click="removeRegion(r)">
                      <Icon name="trash" :size="16" />
                    </Button>
                  </div>
                </td>
              </tr>

              <tr v-if="expandedId === r.id" class="rg-expand">
                <td colspan="5">
                  <div v-if="regionNodesLoading" class="rg-muted rg-loading">加载中…</div>
                  <table v-else class="rg-table rg-table--inner">
                    <thead>
                      <tr>
                        <th>节点</th>
                        <th>地址</th>
                        <th>驱动</th>
                        <th>状态</th>
                        <th>已绑 / 容量</th>
                        <th>建议容量</th>
                        <th class="rg-col-usage">使用率</th>
                        <th class="rg-col-act">采用</th>
                        <th class="rg-col-act">操作</th>
                      </tr>
                    </thead>
                    <tbody>
                      <tr v-for="n in regionNodes" :key="n.id">
                        <td>{{ n.name }}</td>
                        <td class="mono">{{ n.host || '—' }}</td>
                        <td>{{ driverLabel(n.driver_type) }}</td>
                        <td>
                          <Badge :variant="statusVariant(n.status)" dot>{{ n.status || '未知' }}</Badge>
                        </td>
                        <td class="mono">{{ n.bound_users ?? 0 }} / {{ capText(n.capacity) }}</td>
                        <td class="rg-rec" :title="recReason(n)">
                          <template v-if="suggestOf(n)">
                            <span class="rg-rec-icon">{{ confIcon(n) }}</span>
                            <span class="mono">{{ suggestOf(n) }}</span>
                          </template>
                          <span v-else class="rg-muted">—</span>
                        </td>
                        <td class="rg-col-usage">
                          <template v-if="nodeUsage(n) != null">
                            <ProgressBar :percent="nodeUsage(n)" :label="`${nodeUsage(n)}%`" />
                          </template>
                          <span v-else class="rg-muted">—</span>
                        </td>
                        <td class="rg-col-act">
                          <Button
                            v-if="suggestOf(n)"
                            variant="secondary"
                            size="sm"
                            :loading="adoptingId === n.id"
                            @click="adopt(n)"
                          >采用</Button>
                          <span v-else class="rg-muted">—</span>
                        </td>
                        <td class="rg-col-act">
                          <div class="rg-actions">
                            <Button variant="secondary" size="sm" @click="openMigrate(n)">迁移</Button>
                          </div>
                        </td>
                      </tr>
                      <tr v-if="!regionNodes.length">
                        <td colspan="9" class="rg-empty">该地区暂无节点</td>
                      </tr>
                    </tbody>
                  </table>
                </td>
              </tr>
            </template>

            <tr v-if="!regions.length">
              <td colspan="5" class="rg-empty">还没有地区。节点不设地区时按全量下发，行为和升级前一致。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Card :padding="false" class="rg-card">
      <div class="rg-block-head">
        <h2 class="rg-block-title">未归属节点（全量下发）</h2>
        <span class="rg-muted">{{ unassignedNodes.length }} 台</span>
      </div>
      <div class="rg-table-wrap">
        <table class="rg-table">
          <thead>
            <tr>
              <th>节点</th>
              <th>地址</th>
              <th>驱动</th>
              <th>状态</th>
              <th>已绑 / 容量</th>
              <th>建议容量</th>
              <th class="rg-col-usage">使用率</th>
              <th class="rg-col-act">采用</th>
              <th class="rg-col-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="n in unassignedNodes" :key="n.id">
              <td>{{ n.name }}</td>
              <td class="mono">{{ n.host || '—' }}</td>
              <td>{{ driverLabel(n.driver_type) }}</td>
              <td><Badge :variant="statusVariant(n.status)" dot>{{ n.status || '未知' }}</Badge></td>
              <td class="mono">{{ n.bound_users ?? 0 }} / {{ capText(n.capacity) }}</td>
              <td class="rg-rec" :title="recReason(n)">
                <template v-if="suggestOf(n)">
                  <span class="rg-rec-icon">{{ confIcon(n) }}</span>
                  <span class="mono">{{ suggestOf(n) }}</span>
                </template>
                <span v-else class="rg-muted">—</span>
              </td>
              <td class="rg-col-usage">
                <template v-if="nodeUsage(n) != null">
                  <ProgressBar :percent="nodeUsage(n)" :label="`${nodeUsage(n)}%`" />
                </template>
                <span v-else class="rg-muted">—</span>
              </td>
              <td class="rg-col-act">
                <Button
                  v-if="suggestOf(n)"
                  variant="secondary"
                  size="sm"
                  :loading="adoptingId === n.id"
                  @click="adopt(n)"
                >采用</Button>
                <span v-else class="rg-muted">—</span>
              </td>
              <td class="rg-col-act">
                <div class="rg-actions">
                  <Button variant="secondary" size="sm" @click="openAssign(n)">设置归属 / 容量</Button>
                </div>
              </td>
            </tr>
            <tr v-if="!unassignedNodes.length">
              <td colspan="9" class="rg-empty">所有节点都已归属地区</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 新建 / 编辑地区 -->
    <Modal v-model="formOpen" :title="form.id ? '编辑地区' : '新建地区'" width="420px">
      <div class="rg-form">
        <label class="rg-field">
          <span>地区名</span>
          <Input v-model="form.name" placeholder="例如：香港" />
        </label>
        <label class="rg-field rg-field--sort">
          <span>排序（小的排前面）</span>
          <Input v-model="form.sort" type="number" step="1" mono placeholder="0" />
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="formOpen = false">取消</Button>
        <Button variant="primary" size="sm" :loading="saving" @click="saveRegion">保存</Button>
      </template>
    </Modal>

    <!-- 迁移：选目标节点 + 数量 -->
    <Modal v-model="migOpen" :title="`迁移 · ${migSource?.name || ''}`" width="460px">
      <div class="rg-form">
        <p class="rg-muted">
          把该节点上的用户改绑到另一台节点（只改绑定关系，不动物理配置）。
        </p>
        <label class="rg-field">
          <span>目标节点</span>
          <Select v-model="migTargetId" :options="targetOptions" placeholder="选择目标节点" />
        </label>
        <label class="rg-field rg-field--sort">
          <span>迁移数量（留空 = 全部）</span>
          <Input v-model="migQty" type="number" min="0" step="1" mono placeholder="留空 = 全部" />
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="migOpen = false">取消</Button>
        <Button variant="primary" size="sm" :loading="migSubmitting" @click="submitMigrate">确认迁移</Button>
      </template>
    </Modal>

    <!-- 未归属节点：设置归属 / 容量 -->
    <Modal v-model="assignOpen" :title="`设置归属 · ${assignNode?.name || ''}`" width="420px">
      <div class="rg-form">
        <label class="rg-field">
          <span>所属地区</span>
          <Select v-model="assignRegionId" :options="regionOptions" placeholder="不设置（全量下发）" />
        </label>
        <label class="rg-field rg-field--sort">
          <span>容量上限（留空 = 不限）</span>
          <Input v-model="assignCapacity" type="number" min="1" step="1" mono placeholder="留空 = 不限" />
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="assignOpen = false">取消</Button>
        <Button variant="primary" size="sm" :loading="assignSaving" @click="saveAssign">保存</Button>
      </template>
    </Modal>

    <!-- 后台任务进度 -->
    <Modal v-model="jobOpen" :title="`${jobTitle}进度`" width="440px">
      <div class="rg-form">
        <p class="rg-muted">
          状态：{{ job?.status || '—' }}
          <template v-if="job?.started_at"> · 开始 {{ job.started_at }}</template>
        </p>
        <ProgressBar :percent="jobPercent" :label="`${jobPercent}%`" />
        <p class="rg-job-line mono">
          已完成 {{ job?.done ?? 0 }} / {{ job?.total ?? 0 }}
          <span v-if="job?.failed">· 失败 {{ job.failed }}</span>
        </p>
        <p v-if="job?.error" class="rg-error">{{ job.error }}</p>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="jobOpen = false">关闭</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
/* 页面根容器限宽：宽屏下表格不被拉满视口（同 NodeMultipliers） */
.rg {
  max-width: 1200px;
}
.rg-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-4);
}
.rg-head-actions {
  display: flex;
  gap: var(--space-2);
}
.rg-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.rg-hint {
  margin: 0 0 var(--space-4);
  color: var(--text-secondary);
  font-size: var(--text-sm);
}
.rg-stats {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: var(--space-4);
  margin-bottom: var(--space-4);
}
/* 容量告警条：红 = 已超 95%，黄 = 已超 85% */
.rg-alert {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin-bottom: var(--space-3);
  padding: var(--space-2) var(--space-3);
  border: 1px solid;
  border-radius: var(--radius-md);
  font-size: var(--text-sm);
}
.rg-alert--critical {
  background: var(--danger-muted);
  border-color: var(--danger);
  color: var(--danger);
}
.rg-alert--warn {
  background: var(--warning-muted);
  border-color: var(--warning);
  color: var(--warning);
}
/* 建议容量：图标 + 数字挤在一格里 */
.rg-rec {
  display: flex;
  align-items: center;
  gap: var(--space-1);
}
.rg-rec-icon {
  font-size: var(--text-xs);
}
.rg-stat-label {
  margin: 0 0 var(--space-1);
  color: var(--text-muted);
  font-size: var(--text-xs);
}
.rg-stat-value {
  margin: 0 0 var(--space-2);
  font-family: var(--font-mono);
  font-size: var(--text-lg);
  font-weight: var(--font-semibold);
}
.rg-card {
  margin-bottom: var(--space-4);
}
.rg-table-wrap {
  overflow-x: auto;
}
.rg-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.rg-table th,
.rg-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  vertical-align: middle;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
.rg-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.rg-table tbody tr:last-child td {
  border-bottom: none;
}
.rg-row {
  cursor: pointer;
}
.rg-row:hover {
  background: var(--bg-hover);
}
.rg-name {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  font-weight: var(--font-medium);
}
.rg-col-usage {
  width: 200px;
}
.rg-col-act {
  text-align: right;
}
.rg-actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-1);
}
/* 展开区：底色比表格深一档，视觉上归到所属地区行下面 */
.rg-expand > td {
  background: var(--bg-base);
  padding: var(--space-2) var(--space-4) var(--space-4);
}
.rg-table--inner th,
.rg-table--inner td {
  padding: var(--space-2) var(--space-3);
}
.rg-table--inner thead th {
  border-bottom: 1px solid var(--border-subtle);
}
.rg-loading {
  padding: var(--space-3) 0;
}
.rg-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
.rg-block-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  padding: var(--space-4) var(--space-4) var(--space-2);
}
.rg-block-title {
  margin: 0;
  font-size: var(--text-base);
  font-weight: var(--font-semibold);
}
.rg-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.rg-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.rg-field--sort {
  max-width: 220px;
}
.rg-muted {
  color: var(--text-muted);
  font-size: var(--text-xs);
  margin: 0;
}
.rg-job-line {
  margin: 0;
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.rg-error {
  margin: 0;
  color: var(--danger);
  font-size: var(--text-sm);
}
.mono {
  font-family: var(--font-mono);
}

@media (max-width: 900px) {
  .rg-stats {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
@media (max-width: 768px) {
  .rg-head {
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-2);
  }
}
</style>
