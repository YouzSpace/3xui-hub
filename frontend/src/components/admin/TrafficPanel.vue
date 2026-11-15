<script setup>
/**
 * TrafficPanel.vue — 仪表盘「流量」卡（服务器口径 + 用户套餐口径，上下合并成一张卡）。
 *
 * 为什么单独拆出来：这一块自带一套完整状态（机器榜 / 用户榜 / 明细 / 两个二级弹窗 / CSV），
 * 留在 Dashboard.vue 里既撑大页面又和曲线、资源排行混在一起。拆开后页面只负责布局与曲线。
 *
 * 边界（耦合面刻意做小）：
 *   - 入参只有三个：`windowKey`（机器口径时间窗，v-model，与页面的曲线卡共用同一个窗口）、
 *     `nodes`（页面已取好的 /monitor/summary 节点表，本组件不重复请求）、`loading`；
 *   - 出参两个：`update:windowKey`（用户改了时间窗）、`refresh`（用户点了刷新 → 页面重新取 summary）。
 *   - 用户口径的时间窗是本组件私有的（USER_WINDOWS）：它和机器口径不是同一套桶表。
 *
 * 顶部对比条固定看「近 24 小时」：一眼看出服务器实际流量与套餐计费量差多少、倍率是否合适。
 * 末格给的是**平均倍率 = 计费 ÷ 用户原始**（weighted 就是原始乘节点倍率，所以这个比值就是实测平均倍率）；
 * 服务器那格只用于看两个口径的量级差距（网卡双向计数，天然≈用户原始的 2 倍，不能当倍率分母）。
 */
import { computed, onActivated, onMounted, ref, watch } from 'vue'
import Card from '@/components/ui/Card.vue'
import Badge from '@/components/ui/Badge.vue'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import { useToast } from '@/components/ui/Toast.vue'
import { adminApi } from '@/api/admin'
import { finite, formatBytes, formatSpeed, peakTime } from '@/utils/format'

const props = defineProps({
  /** 机器口径时间窗（v-model:windowKey）：与页面的曲线卡共用，避免两处各选一个窗口。 */
  windowKey: { type: String, default: '24h' },
  /** 页面已取好的 /monitor/summary 节点表（含 up/down/total/cpu/mem/peak_*）。 */
  nodes: { type: Array, default: () => [] },
  /** 页面正在取 summary（转圈用）。 */
  loading: { type: Boolean, default: false },
})

const emit = defineEmits(['update:windowKey', 'refresh'])

const toast = useToast()

/* ── 机器口径 ─────────────────────────────────────────────── */

const WINDOWS = [
  { label: '最近 24 小时', value: '24h' },
  { label: '最近 7 天', value: '7d' },
  { label: '最近 30 天', value: '30d' },
  { label: '本月', value: 'month' },
]

/** 「查看全部」弹窗标题用的窗口名（页面曲线卡另有一份，两处文案要保持一致）。 */
const WINDOW_TITLES = {
  '24h': '最近24小时流量',
  '7d': '最近7天流量',
  '30d': '最近30天流量',
  month: '本月流量',
}

const windowTitle = computed(() => WINDOW_TITLES[props.windowKey] || WINDOW_TITLES['24h'])

/** 有数据节点按双向合计降序；无数据（null）一律排最后。 */
const byTotalDesc = (list) => {
  const rank = (node) => (finite(node.total) ? node.total : null)

  return [...list].sort((a, b) => {
    const left = rank(a)
    const right = rank(b)
    if (left === null && right === null) return 0
    if (left === null) return 1
    if (right === null) return -1
    return right - left
  })
}

/** Top 5：按双向降序取前 5 名（取不到 5 名就有几名算几名）。 */
const TOP_N = 5

const trafficNodes = computed(() => (Array.isArray(props.nodes) ? props.nodes : []))
const topNodes = computed(() => byTotalDesc(trafficNodes.value).slice(0, TOP_N))
const trafficHasData = computed(() => trafficNodes.value.some((node) => finite(node.total)))

/** Top5 进度条的分母：有数据节点里的最大值；全无数据时没有分母。 */
const maxTotal = computed(() => {
  const values = trafficNodes.value.map((node) => node.total).filter(finite)
  return values.length ? Math.max(...values) : null
})

/*
 * 「查看全部」弹窗：后端 /monitor/summary 本来就返回全部 enabled 节点，
 * 前端只是用 topNodes 截了前 5 —— 不额外请求。
 */
const allNodesOpen = ref(false)
const allNodes = computed(() => byTotalDesc(trafficNodes.value))
const maxAllTotal = computed(() => {
  const values = allNodes.value.map((node) => node.total).filter(finite)
  return values.length ? Math.max(...values) : null
})

/** 机器倍率：summary 不含倍率，从 node-ranking 的 rows 按 node_id 借。 */
const multiplierMap = computed(() => {
  const map = {}
  for (const row of nodeRankRows.value) {
    map[row.node_id] = row.multiplier
  }
  return map
})
const nodeMultiplierText = (id) => {
  const v = multiplierMap.value[id]
  return finite(v) ? `${v}×` : null
}

/* ── 展示小工具 ───────────────────────────────────────────── */

/** 进度条宽度：没有数据不画进度（0%），不伪装成「占 0」。 */
const barWidth = (value, max) => {
  if (!finite(value) || !finite(max) || max <= 0) return '0%'

  return `${Math.max(2, Math.min(100, (value / max) * 100))}%`
}

const cellBytes = (value) => formatBytes(value)

/** 机器行的峰值文案：「峰值 X MB/s at 昨天 HH:MM」；无峰值信息返回 null。 */
const peakRateText = (node) => {
  if (!finite(node.peak_rate)) return null

  const when = peakTime(node.peak_rate_at)
  return when ? `峰值 ${formatSpeed(node.peak_rate)} at ${when}` : `峰值 ${formatSpeed(node.peak_rate)}`
}

/* ── 机器口径 CSV ─────────────────────────────────────────── */

const CSV_HEADER = ['节点', '上行(字节)', '下行(字节)', '双向(字节)', 'CPU均值(%)', '内存均值(%)']

/**
 * CSV 单元格：缺失值输出**空字段**（表格软件读作「没有」），不写 0，
 * 也不写「—」（那会让整列变成文本，用户没法求和）。含逗号/引号/换行的按 RFC4180 转义。
 */
const csvCell = (value) => {
  if (value === null || value === undefined || value === '') return ''
  const text = String(value)
  return /[",\n\r]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text
}

/** 机器明细 CSV（导出**全部节点**，与 Top5 同序）。 */
const buildCsv = () => {
  const lines = [CSV_HEADER.join(',')]

  for (const node of byTotalDesc(trafficNodes.value)) {
    lines.push([node.name, node.up, node.down, node.total, node.cpu_avg, node.mem_avg].map(csvCell).join(','))
  }

  return lines.join('\n')
}

const dateStamp = (date = new Date()) => {
  const pad = (n) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

const csvFilename = () => `monitor-${props.windowKey}-${dateStamp()}.csv`

/** 前端 Blob 下载（BOM 让 Excel 认 UTF-8，否则中文表头乱码）。 */
const downloadCsv = (content, filename) => {
  const blob = new Blob([`﻿${content}`], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  document.body.appendChild(link)
  link.click()
  document.body.removeChild(link)
  URL.revokeObjectURL(url)
}

const exportCsv = () => {
  downloadCsv(buildCsv(), csvFilename())
  toast.success('已导出 CSV')
}

/* ── 用户套餐口径 ─────────────────────────────────────────── */

/**
 * 用户维度只有 24h / 7d / 30d 三档（小时表服务滑动窗口，天表服务自然日窗口），
 * 与机器口径的 `WINDOWS`（多一个「本月」）不是同一套，所以单独一份选项。
 */
const USER_WINDOWS = [
  { label: '最近 24 小时', value: '24h' },
  { label: '最近 7 天', value: '7d' },
  { label: '最近 30 天', value: '30d' },
]

/** 窗口标题后缀（设计稿：「用户流量排行 · 近 7 天」）。 */
const USER_WINDOW_LABELS = { '24h': '近 24 小时', '7d': '近 7 天', '30d': '近 30 天' }

const userWindow = ref('24h')
const userRanking = ref(null)
const nodeRanking = ref(null)
/** 点开的用户（核心视角：这个用户在哪些机器上用了多少） */
const expandedUserId = ref(null)
const nodeDetail = ref(null)
const userTrafficLoading = ref(true)
const userTrafficFailed = ref(false)
const userTrafficLoadingDetail = ref(false)

const userRows = computed(() => (Array.isArray(userRanking.value?.rows) ? userRanking.value.rows : []))
const nodeRankRows = computed(() => (Array.isArray(nodeRanking.value?.rows) ? nodeRanking.value.rows : []))
const userWindowLabel = computed(() => USER_WINDOW_LABELS[userWindow.value] || USER_WINDOW_LABELS['24h'])

/*
 * 用户卡：卡片内只展示 Top 5，「查看全部」弹窗按需拉全量。
 *
 * 为什么按需拉：主页面默认只请求 50 条（后端 DEFAULT_LIMIT），大多数人只看前几名；
 * 一进页面就拉 500 条会让仪表盘首屏多背几百 KB。弹窗打开时才拉，且同一窗口内复用。
 */
const topUserRows = computed(() => userRows.value.slice(0, TOP_N))

const allUsersOpen = ref(false)
const allUsers = ref([])
const allUsersLoading = ref(false)

const openAllUsers = async () => {
  allUsersOpen.value = true
  if (allUsers.value.length) return // 同一窗口内已拉过

  allUsersLoading.value = true
  try {
    // 500 = 后端 MAX_LIMIT（UserTrafficService::MAX_LIMIT）
    const data = await adminApi.userTrafficRanking(userWindow.value, 500)
    allUsers.value = Array.isArray(data?.rows) ? data.rows : []
  } catch (e) {
    toast.error(e?.message || '用户流量加载失败')
  } finally {
    allUsersLoading.value = false
  }
}

/** 换窗口后缓存的全量数据作废：否则弹窗里还是上一个窗口的排名。
 *  弹窗正开着就立刻按新窗口重拉，避免用户看着空表格以为没数据。 */
watch(userWindow, () => {
  allUsers.value = []
  if (allUsersOpen.value) {
    openAllUsers()
  }
})

/**
 * 用户榜进度条 / 百分比的分母：该窗口内**全部用户**的加权用量合计（后端 total_weighted）。
 *
 * 为什么不用「榜内最大值」：那算出来的是「占第一名的比例」，第一名恒 100%、其余人偏小，
 * 看着像"比例不对"。用户要的是「这个人占全体用户消耗的份额」——分母必须是全体合计，
 * 且它是窗口级常量，卡片（Top5）与「查看全部」弹窗用同一个分母，口径一致。
 */
const userTotalWeighted = computed(() => {
  const total = userRanking.value?.total_weighted
  return finite(total) && total > 0 ? total : null
})

/** 套餐类型徽章文案：period → 周期·N 个月；total → 总量；无套餐 → —。 */
const planTypeLabel = (row) => {
  if (row?.plan_type === 'period') {
    return row.plan_months ? `周期·${row.plan_months} 个月` : '周期'
  }
  if (row?.plan_type === 'total') return '总量'

  return '—'
}

/** 占比百分比：分母为 null / 0 时返回 null（不画 0%，也不画 NaN%）。 */
const sharePercent = (value, max) => {
  if (!finite(value) || !finite(max) || max <= 0) return null

  return Math.round((value / max) * 1000) / 10
}

/** 占比文字：整数不带小数，其余保留 1 位（64% / 12.5%）。 */
const shareText = (value) => {
  if (!finite(value)) return '—'

  return `${Number.isInteger(value) ? value.toFixed(0) : value.toFixed(1)}%`
}

/** 「数据自 YYYY-MM-DD 起」：新表从上线当天开始累积，7d/30d 要等满（缺数据 ≠ 0）。 */
const userDataStart = computed(() => {
  const starts = [userRanking.value?.data_start, nodeRanking.value?.data_start].filter(Boolean)
  return starts.length ? starts.sort()[0] : null
})

/** 展开行的明细：本用户在该窗口内各机器占多少（加权降序）。 */
const loadUserNodes = async (userId) => {
  userTrafficLoadingDetail.value = true

  try {
    const data = await adminApi.userTrafficNodes(userId, userWindow.value)
    // 请求返回前用户又点了别的行（或关了）：丢弃过期结果
    if (expandedUserId.value === userId) nodeDetail.value = data || null
  } catch (e) {
    if (expandedUserId.value === userId) nodeDetail.value = { rows: [] }
    toast.error(e?.message || '用户机器明细加载失败')
  } finally {
    userTrafficLoadingDetail.value = false
  }
}

const toggleUser = async (userId) => {
  if (expandedUserId.value === userId) {
    expandedUserId.value = null
    nodeDetail.value = null
    return
  }

  expandedUserId.value = userId
  nodeDetail.value = null
  await loadUserNodes(userId)
}

const nodeDetailRows = computed(() => (Array.isArray(nodeDetail.value?.rows) ? nodeDetail.value.rows : []))
const nodeDetailMax = computed(() => {
  const values = nodeDetailRows.value.map((row) => row.weighted).filter(finite)
  return values.length ? Math.max(...values) : null
})
/** 明细里各机器占该用户的百分比（后端 share；缺失时用榜内最大值兜底算相对条长）。 */
const nodeDetailShare = (node) =>
  finite(node.share) ? `${Number.isInteger(node.share) ? node.share.toFixed(0) : node.share.toFixed(1)}%` : shareText(sharePercent(node.weighted, nodeDetailMax.value))

/** 倍率标注（设计稿：JP1（1.5×））。 */
const multiplierText = (value) => (finite(value) ? `${value}×` : '—')

const loadUserTraffic = async () => {
  userTrafficLoading.value = true
  userTrafficFailed.value = false

  try {
    const [users, nodes] = await Promise.all([
      adminApi.userTrafficRanking(userWindow.value),
      adminApi.nodeTrafficRanking(userWindow.value),
    ])

    userRanking.value = users || null
    nodeRanking.value = nodes || null

    // 展开中的用户若在新窗口里已经没数据：收起；否则按新窗口刷新明细
    if (expandedUserId.value !== null) {
      const stillListed = (users?.rows || []).some((row) => row.user_id === expandedUserId.value)
      if (!stillListed) {
        expandedUserId.value = null
        nodeDetail.value = null
      } else {
        await loadUserNodes(expandedUserId.value)
      }
    }
  } catch (e) {
    userTrafficFailed.value = true
    toast.error(e?.message || '用户流量数据加载失败')
  } finally {
    userTrafficLoading.value = false
  }
}

watch(userWindow, loadUserTraffic)

/* ── 顶部对比条：服务器 24h vs 用户套餐口径 24h ───────────── */

/** 服务器口径（网卡双向，monitor_samples 差分）近 24 小时。 */
const compareServer = ref(null)
/** 用户套餐口径近 24 小时（只要合计，rows 不用）。 */
const compareUser = ref(null)

const loadCompare = async () => {
  // 两块各自独立：任一块失败只影响自己那半格，不连累另一块
  const [server, users] = await Promise.allSettled([
    adminApi.monitorSummary('24h'),
    adminApi.userTrafficRanking('24h'),
  ])

  if (server.status === 'fulfilled') compareServer.value = server.value?.totals || null
  if (users.status === 'fulfilled') compareUser.value = users.value || null
}

const compareServerTotal = computed(() => {
  const up = compareServer.value?.up
  const down = compareServer.value?.down
  if (!finite(up) && !finite(down)) return null

  return (finite(up) ? up : 0) + (finite(down) ? down : 0)
})

/**
 * 对比比值：**用户套餐计费 ÷ 服务器网卡（双向，含全部损耗）**，≈1 为平衡。
 *
 * 用户定稿的口径与告警：
 *   - **≥ 1 不告警**（用户套餐扣的够覆盖服务器真实消耗，甚至更多 —— 不必打扰）；
 *   - **< 1 才告警**（服务器实际跑得比用户套餐扣的多 → 有流量没记进用户套餐，要盯）。
 */
const compareRatioValue = computed(() => {
  const serverTotal = compareServerTotal.value
  const plan = compareUser.value?.total_weighted
  if (!finite(serverTotal) || serverTotal <= 0 || !finite(plan) || plan <= 0) return null

  return plan / serverTotal
})

/** 告警等级：≥1 → ok（不告警）；0.9~1 → warn（略偏离）；<0.9 → bad（偏离过大）。 */
const balanceOf = (ratio) => {
  if (!finite(ratio) || ratio <= 0) return 'unknown'
  if (ratio >= 1) return 'ok'
  return ratio >= 0.9 ? 'warn' : 'bad'
}

const compareBalance = computed(() => balanceOf(compareRatioValue.value))

const compareRatioText = computed(() => {
  const r = compareRatioValue.value
  if (!finite(r)) return null

  return `${r.toFixed(2)}×`
})

/* ── 用户口径 CSV ─────────────────────────────────────────── */

/**
 * 用户榜 CSV：完整邮箱（导出是台账用途，管理员本来就能在用户列表看到）；
 * 打码是**界面展示**的选择，不该把导出也变成不可用的数据。
 * 缺失值仍输出空字段（不写 0 / 不写「—」，否则整列变文本没法求和）。
 */
const buildUserTrafficCsv = (rows = userRows.value, maxWeighted = userTotalWeighted.value) => {
  const header = [
    '排名', '用户ID', '邮箱', '套餐用量(字节)', '占比(%)',
    '上行原始(字节)', '下行原始(字节)', '套餐类型', '套餐名', '套餐月数',
  ]

  const lines = [header.join(',')]
  rows.forEach((row, index) => {
    lines.push([
      index + 1,
      row.user_id,
      row.email,
      row.weighted,
      sharePercent(row.weighted, maxWeighted),
      row.up,
      row.down,
      row.plan_type,
      row.plan_name,
      row.plan_months,
    ].map(csvCell).join(','))
  })

  return lines.join('\n')
}

const userTrafficCsvFilename = () => `user-traffic-${userWindow.value}-${dateStamp()}.csv`

const exportUserTrafficCsv = () => {
  downloadCsv(buildUserTrafficCsv(), userTrafficCsvFilename())
  toast.success('已导出用户流量 CSV')
}

/** 弹窗内导出：同一套列，数据换成全量（最多 500 位）。 */
const exportAllUsersCsv = () => {
  downloadCsv(
    buildUserTrafficCsv(allUsers.value, userTotalWeighted.value),
    `user-traffic-all-${userWindow.value}-${dateStamp()}.csv`,
  )
  toast.success(`已导出 ${allUsers.value.length} 位用户的流量 CSV`)
}

/* ── 取数 ─────────────────────────────────────────────────── */

/**
 * 整卡「刷新」：机器口径（父级取 summary）+ 用户口径 + 顶部对比条一起重取。
 *
 * 只在机器口径那一行放一个刷新按钮（用户定稿）：这张卡上下两段现在是一块东西，
 * 两个刷新按钮点哪个都容易漏刷另一半。
 */
const refreshAll = () => {
  emit('refresh')
  loadUserTraffic()
  loadCompare()
}
/** 转圈状态：父级取数中或本组件的用户榜/对比条还在拉，都算「正在刷新」。 */
const refreshing = computed(() => props.loading || userTrafficLoading.value)

const load = () => {
  loadUserTraffic()
  loadCompare()
}

onMounted(load)
// keep-alive 页面：切回来拉一次最新的（不是轮询）
onActivated(load)

/**
 * 测试用出口：本组件的状态与纯函数都靠 defineExpose 暴露，
 * 页面级的 DOM 断言不用它（DOM 由真实渲染产生）。
 */
defineExpose({
  userWindow,
  userRanking,
  userRows,
  allUsers,
  allUsersOpen,
  allNodes,
  allNodesOpen,
  expandedUserId,
  userTotalWeighted,
  userDataStart,
  csvFilename,
  buildCsv,
  exportCsv,
  buildUserTrafficCsv,
  userTrafficCsvFilename,
  exportUserTrafficCsv,
  openAllUsers,
  loadUserTraffic,
  refreshAll,
  refreshing,
  nodeMultiplierText,
  maxAllTotal,
  maxTotal,
  topNodes,
  allUsersLoading,
  userTrafficLoading,
  compareRatioText,
  compareRatioValue,
  compareBalance,
  compareServerTotal,
})
</script>

<template>
  <Card class="dsh-ut-card">
    <!-- 顶部对比条：突出**总流量**（服务器实际 vs 用户套餐计费），上下行只是次要小字 -->
    <div class="dsh-cmp">
      <div class="dsh-cmp-item">
        <span class="dsh-cmp-label">服务器 · 近 24 小时</span>
        <span class="dsh-cmp-total">{{ cellBytes(compareServerTotal) }}</span>
        <span class="dsh-cmp-flows">
          <span class="dsh-up">↑ {{ cellBytes(compareServer?.up) }}</span>
          <span class="dsh-down">↓ {{ cellBytes(compareServer?.down) }}</span>
        </span>
      </div>
      <div class="dsh-cmp-item">
        <span class="dsh-cmp-label">用户套餐 · 近 24 小时</span>
        <span class="dsh-cmp-total">{{ cellBytes(compareUser?.total_weighted) }}</span>
        <span class="dsh-cmp-flows">
          <span class="dsh-up">↑ {{ cellBytes(compareUser?.total_up) }}</span>
          <span class="dsh-down">↓ {{ cellBytes(compareUser?.total_down) }}</span>
        </span>
      </div>
      <div class="dsh-cmp-item dsh-cmp-item--ratio">
        <span class="dsh-cmp-label">用户套餐 ÷ 服务器（≈1 才平衡）</span>
        <span class="dsh-cmp-total" :class="compareBalance ? `is-${compareBalance}` : ''">{{ compareRatioText || '—' }}</span>
        <!-- 只在 <1（用户套餐扣得比服务器实际少 = 偏低）时告警；≥1 不告警 -->
        <span v-if="compareBalance === 'bad'" class="dsh-cmp-flows dsh-cmp-alert">明显偏低，查倍率/漏记</span>
        <span v-else-if="compareBalance === 'warn'" class="dsh-cmp-flows dsh-cmp-alert">略偏低</span>
      </div>
    </div>

    <!-- ① 机器口径：按服务器实际流量排行 -->
    <div class="dsh-section-head">
      <h2 class="dsh-section-title">流量消耗 Top 5</h2>
      <div class="dsh-toolbar">
        <Select
          :model-value="windowKey"
          wrapper-class="dsh-window-select"
          size="sm"
          :options="WINDOWS"
          @update:model-value="emit('update:windowKey', $event)"
        />
        <Button variant="secondary" size="sm" :loading="refreshing" @click="refreshAll">
          <Icon name="refresh" :size="16" /> 刷新
        </Button>
        <Button variant="secondary" size="sm" @click="exportCsv">
          <Icon name="inbox" :size="16" /> 导出 CSV
        </Button>
        <Button variant="secondary" size="sm" :disabled="!trafficNodes.length" @click="allNodesOpen = true">
          查看全部
        </Button>
      </div>
    </div>

    <p v-if="loading && !trafficNodes.length" class="dsh-empty">正在加载流量数据…</p>

    <template v-else-if="trafficHasData">
      <ul class="dsh-rank">
        <li v-for="(node, index) in topNodes" :key="node.id" class="dsh-rank-item">
          <div class="dsh-rank-head">
            <span class="dsh-rank-seq">{{ index + 1 }}.</span>
            <span class="dsh-rank-name">{{ node.name }}</span>
            <span class="dsh-rank-flows">
              <span class="dsh-up">↑ {{ cellBytes(node.up) }}</span>
              <span class="dsh-down">↓ {{ cellBytes(node.down) }}</span>
            </span>
            <Badge v-if="!finite(node.total)" variant="info" class="dsh-rank-badge">无数据</Badge>
          </div>
          <p v-if="peakRateText(node)" class="dsh-rank-peak">{{ peakRateText(node) }}</p>
          <i class="dsh-bar" :class="{ 'is-unknown': !finite(node.total) }">
            <b :style="{ width: barWidth(node.total, maxTotal) }"></b>
          </i>
        </li>
      </ul>
    </template>

    <p v-else class="dsh-empty">暂无采样数据：采集随每 5 分钟健康检查进行，稍后即可看到</p>

    <hr class="dsh-ut-split" />

    <!-- ② 用户套餐口径：按从用户套餐里扣掉的量排行 -->
    <div class="dsh-section-head is-stack">
      <h2 class="dsh-section-title">用户流量排行 · {{ userWindowLabel }}</h2>
      <div class="dsh-toolbar">
        <!-- 时间窗与上面机器口径**同一个下拉样式**（用户定稿：不要一排 pill） -->
        <Select
          :model-value="userWindow"
          wrapper-class="dsh-window-select"
          size="sm"
          :options="USER_WINDOWS"
          @update:model-value="userWindow = $event"
        />
        <Button variant="secondary" size="sm" :disabled="!userRows.length" @click="exportUserTrafficCsv">
          <Icon name="inbox" :size="16" /> 导出 CSV
        </Button>
        <Button variant="secondary" size="sm" :disabled="!userRows.length" @click="openAllUsers">
          查看全部
        </Button>
      </div>
    </div>

    <div v-if="userTrafficFailed" class="dsh-banner" role="alert">
      <Badge variant="danger" dot>加载失败</Badge>
      <span class="dsh-banner-text">用户流量暂时取不到，下方是上一次成功获取的数据。</span>
      <Button variant="secondary" size="sm" @click="loadUserTraffic">重试</Button>
    </div>

    <p v-if="userTrafficLoading && !userRanking" class="dsh-empty">正在加载用户流量…</p>

    <div v-else-if="userRows.length" class="dsh-ut-list">
      <div
        v-for="row in topUserRows"
        :key="row.user_id"
        class="dsh-ut-row dsh-ut-item"
        :class="{ 'is-open': expandedUserId === row.user_id }"
        role="button"
        tabindex="0"
        :aria-expanded="expandedUserId === row.user_id"
        @click="toggleUser(row.user_id)"
        @keydown.enter.prevent="toggleUser(row.user_id)"
        @keydown.space.prevent="toggleUser(row.user_id)"
      >
        <!-- 上排：用户ID/用量/套餐/原始↑↓/百分比（左）+ 明细（右），进度条在下方 -->
        <div class="dsh-ut-item-head">
          <span class="dsh-ut-uid">#{{ row.user_id }}</span>
          <span class="dsh-ut-strong">{{ cellBytes(row.weighted) }}</span>
          <span class="dsh-ut-plan dsh-ut-col-plan">{{ planTypeLabel(row) }}</span>
          <span class="dsh-ut-item-flows dsh-ut-col-raw">
            <span class="dsh-up">↑ {{ cellBytes(row.up) }}</span>
            <span class="dsh-down">↓ {{ cellBytes(row.down) }}</span>
          </span>
          <span class="dsh-ut-pct">{{ shareText(sharePercent(row.weighted, userTotalWeighted)) }}</span>
          <span class="dsh-ut-toggle" :class="{ 'is-open': expandedUserId === row.user_id }">
            <Icon class="dsh-ut-toggle-ico" :name="expandedUserId === row.user_id ? 'chevron-down' : 'chevron-right'" :size="16" />
            <span class="dsh-ut-toggle-txt">明细</span>
          </span>
        </div>
        <!-- 下排：整条进度条（占全体用户合计的比例） -->
        <i class="dsh-ut-item-bar">
          <b :style="{ width: barWidth(row.weighted, userTotalWeighted) }"></b>
        </i>

        <!-- 该用户在各机器上的分布（本次需求的核心视角） -->
        <div v-if="expandedUserId === row.user_id" class="dsh-ut-detail" @click.stop>
          <p class="dsh-ut-detail-title">该用户按机器明细（{{ userWindowLabel }} · 套餐口径）</p>

          <p v-if="userTrafficLoadingDetail" class="dsh-ut-detail-empty">正在加载机器明细…</p>
          <p v-else-if="!nodeDetailRows.length" class="dsh-ut-detail-empty">该窗口内没有按机器的流量记录</p>
          <ul v-else class="dsh-ut-node-list">
            <li v-for="node in nodeDetailRows" :key="node.node_id" class="dsh-ut-node">
              <span class="dsh-ut-node-name">
                {{ node.name }}（{{ multiplierText(node.multiplier) }}）
              </span>
              <span class="dsh-ut-node-bar">
                <b :style="{ width: barWidth(node.weighted, nodeDetailMax) }"></b>
              </span>
              <span class="dsh-ut-node-value">{{ cellBytes(node.weighted) }}</span>
              <span class="dsh-ut-node-share">{{ nodeDetailShare(node) }}</span>
            </li>
          </ul>
        </div>
      </div>
    </div>

    <p v-else class="dsh-empty">暂无用户流量数据</p>

    <p class="dsh-note dsh-ut-note">
      用户榜为<strong>套餐口径</strong>（上下行合计 × 节点倍率，与用户面板用量一致）；
      ↑↓ 小字是原始字节（不乘倍率）。展开任一用户可看它在各机器的分布。
    </p>

    <!-- 数据起始日：新表从上线当天累积，7d/30d 要等满（缺数据 ≠ 0） -->
    <p v-if="userDataStart" class="dsh-note dsh-ut-start">
      数据自 {{ userDataStart }} 起开始累积：24 小时窗口当天可用，7 天 / 30 天窗口需等满才有完整数据；窗口内无数据的用户不显示。
    </p>
    <p v-else-if="!userTrafficLoading" class="dsh-note dsh-ut-start">
      暂无数据：用户流量从上线当天开始累积（24 小时窗口当天可用）。
    </p>

    <!-- 「全部机器流量」弹窗（复用同一套机器行 UI，只是数据换全量、分母换全部机器） -->
    <Modal v-model="allNodesOpen" :title="`全部机器流量 · ${windowTitle}`" width="640px">
      <div class="dsh-modal-bar">
        <span class="dsh-modal-hint">共 {{ allNodes.length }} 台机器，按双向合计降序</span>
        <Button variant="secondary" size="sm" :disabled="!allNodes.length" @click="exportCsv">
          <Icon name="inbox" :size="16" /> 导出 CSV
        </Button>
      </div>

      <ul v-if="allNodes.length" class="dsh-rank">
        <li v-for="(node, index) in allNodes" :key="node.id" class="dsh-rank-item">
          <div class="dsh-rank-head">
            <span class="dsh-rank-seq">{{ index + 1 }}.</span>
            <span class="dsh-rank-name">{{ node.name }}</span>
            <span v-if="nodeMultiplierText(node.id)" class="dsh-ut-plan">{{ nodeMultiplierText(node.id) }}</span>
            <span class="dsh-rank-flows">
              <span class="dsh-up">↑ {{ cellBytes(node.up) }}</span>
              <span class="dsh-down">↓ {{ cellBytes(node.down) }}</span>
            </span>
            <Badge v-if="!finite(node.total)" variant="info" class="dsh-rank-badge">无数据</Badge>
          </div>
          <p v-if="peakRateText(node)" class="dsh-rank-peak">{{ peakRateText(node) }}</p>
          <i class="dsh-bar" :class="{ 'is-unknown': !finite(node.total) }">
            <b :style="{ width: barWidth(node.total, maxAllTotal) }"></b>
          </i>
        </li>
      </ul>
      <p v-else class="dsh-empty">暂无采样数据</p>
    </Modal>

    <!-- 「全部用户」弹窗：与卡片内同一套行 UI（不显示「序号」列，弹窗里再套展开会更晕） -->
    <Modal v-model="allUsersOpen" :title="`用户流量排行 · ${userWindowLabel}`" width="880px">
      <div class="dsh-modal-bar">
        <span class="dsh-modal-hint">
          套餐口径（上下行合计 × 节点倍率）；最多 {{ allUsers.length }} 位有流量的用户
        </span>
        <Button variant="secondary" size="sm" :disabled="!allUsers.length" @click="exportAllUsersCsv">
          <Icon name="inbox" :size="16" /> 导出 CSV
        </Button>
      </div>

      <p v-if="allUsersLoading" class="dsh-empty">正在加载用户流量…</p>

      <div v-else-if="allUsers.length" class="dsh-ut-list">
        <div
          v-for="row in allUsers"
          :key="row.user_id"
          class="dsh-ut-row dsh-ut-item"
          :class="{ 'is-open': expandedUserId === row.user_id }"
          role="button"
          tabindex="0"
          :aria-expanded="expandedUserId === row.user_id"
          @click="toggleUser(row.user_id)"
          @keydown.enter.prevent="toggleUser(row.user_id)"
          @keydown.space.prevent="toggleUser(row.user_id)"
        >
          <!-- 与主页用户卡同一套卡片样式（上排数据 + 下方整条进度条） -->
          <div class="dsh-ut-item-head">
            <span class="dsh-ut-uid">#{{ row.user_id }}</span>
            <span class="dsh-ut-strong">{{ cellBytes(row.weighted) }}</span>
            <span class="dsh-ut-plan dsh-ut-col-plan">{{ planTypeLabel(row) }}</span>
            <span class="dsh-ut-item-flows dsh-ut-col-raw">
              <span class="dsh-up">↑ {{ cellBytes(row.up) }}</span>
              <span class="dsh-down">↓ {{ cellBytes(row.down) }}</span>
            </span>
            <span class="dsh-ut-pct">{{ shareText(sharePercent(row.weighted, userTotalWeighted)) }}</span>
            <span class="dsh-ut-toggle" :class="{ 'is-open': expandedUserId === row.user_id }">
              <Icon class="dsh-ut-toggle-ico" :name="expandedUserId === row.user_id ? 'chevron-down' : 'chevron-right'" :size="16" />
              <span class="dsh-ut-toggle-txt">明细</span>
            </span>
          </div>
          <i class="dsh-ut-item-bar">
            <b :style="{ width: barWidth(row.weighted, userTotalWeighted) }"></b>
          </i>

          <!-- 弹窗内保留「点用户看按机器明细」，与卡片内是同一份交互 -->
          <div v-if="expandedUserId === row.user_id" class="dsh-ut-detail" @click.stop>
            <p class="dsh-ut-detail-title">该用户按机器明细（{{ userWindowLabel }} · 套餐口径）</p>

            <p v-if="userTrafficLoadingDetail" class="dsh-ut-detail-empty">正在加载机器明细…</p>
            <p v-else-if="!nodeDetailRows.length" class="dsh-ut-detail-empty">该窗口内没有按机器的流量记录</p>
            <ul v-else class="dsh-ut-node-list">
              <li v-for="node in nodeDetailRows" :key="node.node_id" class="dsh-ut-node">
                <span class="dsh-ut-node-name">
                  {{ node.name }}（{{ multiplierText(node.multiplier) }}）
                </span>
                <span class="dsh-ut-node-bar">
                  <b :style="{ width: barWidth(node.weighted, nodeDetailMax) }"></b>
                </span>
                <span class="dsh-ut-node-value">{{ cellBytes(node.weighted) }}</span>
                <span class="dsh-ut-node-share">{{ nodeDetailShare(node) }}</span>
              </li>
            </ul>
          </div>
        </div>
      </div>
      <p v-else class="dsh-empty">暂无用户流量数据</p>
    </Modal>
  </Card>
</template>

<style scoped src="@/assets/styles/components/admin/TrafficPanel.css"></style>
