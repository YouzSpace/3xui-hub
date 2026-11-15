<script setup>
/**
 * Admin Dashboard —— 业务概览 + 节点监控 + 流量（两栏：曲线/Top5 + CPU/内存排行）+ 最近用户。
 *
 * 分工：
 *   - 业务概览（用户 / 服务器在线 / 节点 / 今日成交额）在这里，进页面拉一次 —— 快照性质。
 *   - 节点监控整块在 NodeMonitor.vue（自带 5 秒轮询），这里不重复拉节点列表。
 *   - 流量区块插在 NodeMonitor 与「最近用户」之间，按参考图排成两栏：
 *       左栏（宽）上「最近 24 小时流量」曲线卡 → 下「流量消耗 Top 5」卡；
 *       右栏（窄）上「平均 CPU 占用排行」→ 下「平均内存占用百分比排行」。
 *     窄屏（≤980px）堆叠成单栏。
 *
 * 贯穿的一条原则：**没有数据就是没有数据**。后端把窗口内无样本节点的
 * up/down/total 返回 null（不是 0），这里一律显示「— / 无数据」，绝不兜底成 0 ——
 * 0 会被读成「这台机器在这个窗口里没流量」，采购/扩容决策直接看错。
 */
import { computed, ref, onMounted, onActivated, watch } from 'vue'
import { RouterLink } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Badge from '@/components/ui/Badge.vue'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import Select from '@/components/ui/Select.vue'
import StatCard from '@/components/ui/StatCard.vue'
import Modal from '@/components/ui/Modal.vue'
import NodeMonitor from '@/components/admin/NodeMonitor.vue'
import { useToast } from '@/components/ui/Toast.vue'
import { adminApi } from '@/api/admin'
import { chartGeometry } from '@/utils/chart'
import { finite, formatBytes, formatPercent, formatSpeed, sumOrNull } from '@/utils/format'

/* ── 业务概览 ──────────────────────────────────────────────── */

const users = ref([])
const userTotal = ref(0)
/** 活跃人数 / 连接数（在线 xray 节点上报的 active_users / conns 合计；无上报为 null） */
const activeUsers = ref(null)
const activeConns = ref(null)
const servers = ref({ total: 0, online: 0 })
/** 入站配置总数（原来的「节点」口径：所有服务器的入站加起来） */
const inboundTotal = ref(0)
const revenue = ref(null)

/* ── 流量（排行 + 汇总曲线）────────────────────────────────── */

const WINDOWS = [
  { label: '最近 24 小时', value: '24h' },
  { label: '最近 7 天', value: '7d' },
  { label: '最近 30 天', value: '30d' },
  { label: '本月', value: 'month' },
]

/** 曲线卡标题随窗口变化（参考图：标题就是窗口名 + 流量）。 */
const WINDOW_TITLES = {
  '24h': '最近24小时流量',
  '7d': '最近7天流量',
  '30d': '最近30天流量',
  month: '本月流量',
}

/**
 * 各窗口的采样粒度（秒）—— 与后端 series 的归并粒度一一对应
 * （见 MonitorService::series：24h 原始 5 分钟点 / 7d 按小时 / 30d·month 按天）。
 * points 里的值是该区间的**增量字节**，除以粒度才是速率。
 */
const BUCKET_SECONDS = { '24h': 300, '7d': 3600, '30d': 86400, month: 86400 }

/**
 * 曲线画布几何：不引图表库，直接用 chartGeometry。
 *
 * 刻度**叠在绘图区里**（左/右/下三边内缩），绘图区铺满整张卡片宽度 ——
 * 外部再挂两列 HTML 会让曲线两侧各白掉 62px。内缩量由 padTop/padBottom 留出：
 * 顶部给最高的那枚 Y 刻度、底部给时间轴一行，曲线不会压到字上。
 * 刻度文字用 HTML 绝对定位叠在 SVG 上（preserveAspectRatio="none" 下 SVG 内的字会被横向拉伸）。
 */
const CHART = { width: 600, height: 180, padLeft: 0, padRight: 0, padTop: 20, padBottom: 24 }

/** 视图选择：'aggregate' = 全机合计（默认），其余是具体节点 id。 */
const AGGREGATE = 'aggregate'

const toast = useToast()

const windowKey = ref('24h')
const summary = ref(null)
const series = ref([])
const trafficLoading = ref(true)
const trafficFailed = ref(false)
const selectedView = ref(AGGREGATE)

const trafficNodes = computed(() => (Array.isArray(summary.value?.nodes) ? summary.value.nodes : []))
const totals = computed(() => summary.value?.totals || { up: null, down: null, total: null })

/** 曲线卡标题（随窗口变化）。 */
const trafficTitle = computed(() => WINDOW_TITLES[windowKey.value] || WINDOW_TITLES['24h'])

/** 窗口内有没有任何一份采样：决定「全空」提示，而不是把 0 当成有数据。 */
const trafficHasData = computed(() => trafficNodes.value.some((node) => finite(node.total)))

/** 有数据节点按双向合计降序；无数据（null）一律排最后，且不参与「真的零流量」的混排。 */
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

/** 流量消耗 Top 5：按双向降序取前 5 名（无数据的节点在最后，取不到 5 名就有几名算几名）。 */
const TOP_N = 5

const topNodes = computed(() => byTotalDesc(trafficNodes.value).slice(0, TOP_N))

/*
 * 「查看全部」弹窗：全部机器。
 * 后端 /admin-api/monitor/summary 本来就返回全部 enabled 节点（MonitorService::summary 遍历全表），
 * 前端只是用 topNodes 截了前 5 —— 所以这里不需要新接口，也不额外请求。
 * 列与 Top5 卡同一套（机器 / ↑ / ↓ / 合计 / 峰值 / 倍率），倍率另从 node-ranking 借。
 */
const allNodesOpen = ref(false)
const allNodes = computed(() => byTotalDesc(trafficNodes.value))
const maxAllTotal = computed(() => {
  const values = allNodes.value.map((node) => node.total).filter(finite)
  return values.length ? Math.max(...values) : null
})
/** 机器倍率：summary 不含倍率，从 node-ranking 的 rows 按 node_id 借（唯一入口是 TrafficSyncService::multiplierFor）。 */
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

/**
 * 资源排行（CPU / 内存）：按平均占用降序，无样本（avg 为 null）排最后。
 * 与流量 Top5 同一套排序语义，但分母是各自那一列的平均值。
 */
const byAvgDesc = (list, key) => {
  const rank = (node) => (finite(node[key]) ? node[key] : null)

  return [...list].sort((a, b) => {
    const left = rank(a)
    const right = rank(b)
    if (left === null && right === null) return 0
    if (left === null) return 1
    if (right === null) return -1
    return right - left
  })
}

const cpuRanked = computed(() => byAvgDesc(trafficNodes.value, 'cpu_avg'))
const memRanked = computed(() => byAvgDesc(trafficNodes.value, 'mem_avg'))

/** 流量 Top5 进度条的分母：有数据节点里的最大值；全无数据时没有分母。 */
const maxTotal = computed(() => {
  const values = trafficNodes.value.map((node) => node.total).filter(finite)
  return values.length ? Math.max(...values) : null
})

/** 资源排行进度条分母：该列有数据节点里的最大值；全无数据时没有分母。 */
const maxCpu = computed(() => {
  const values = trafficNodes.value.map((node) => node.cpu_avg).filter(finite)
  return values.length ? Math.max(...values) : null
})

const maxMem = computed(() => {
  const values = trafficNodes.value.map((node) => node.mem_avg).filter(finite)
  return values.length ? Math.max(...values) : null
})

/** 进度条宽度：没有数据不画进度（0%），不伪装成「占 0」。 */
const barWidth = (value, max) => {
  if (!finite(value) || !finite(max) || max <= 0) return '0%'

  return `${Math.max(2, Math.min(100, (value / max) * 100))}%`
}

/** 字节单元格。formatBytes 对 null / 负数返回 '—'。 */
const cellBytes = (value) => formatBytes(value)

/** 百分比单元格（排行卡右侧的平均值）。formatPercent 对 null 返回 '—'。 */
const cellPercent = (value) => formatPercent(value, 1)

/**
 * 「峰值 X at 今天/昨天 HH:MM」里的时间部分。
 * 只给「今天 / 昨天」两个相对日（更早的窗口回落到 M/D），因为参考图里就这两种，
 * 而 7d/30d 窗口的峰值多半在前几天 —— 那时显示绝对日期比「3 天前」更好定位。
 */
const peakTime = (iso) => {
  if (!iso) return null

  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return null

  const pad = (n) => String(n).padStart(2, '0')
  const hhmm = `${pad(at.getHours())}:${pad(at.getMinutes())}`

  const dayStart = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
  const today = dayStart(new Date())
  const target = dayStart(at)

  if (target === today) return `今天 ${hhmm}`
  if (target === today - 86400000) return `昨天 ${hhmm}`

  return `${at.getMonth() + 1}/${at.getDate()} ${hhmm}`
}

/** 流量 Top5 行的峰值文案：「峰值 X MB/s at 昨天 HH:MM」；无峰值信息返回 null（不显示这一行）。 */
const peakRateText = (node) => {
  if (!finite(node.peak_rate)) return null

  const when = peakTime(node.peak_rate_at)
  return when ? `峰值 ${formatSpeed(node.peak_rate)} at ${when}` : `峰值 ${formatSpeed(node.peak_rate)}`
}

/** CPU / 内存排行行的峰值文案：「峰值 X% at 昨天 HH:MM」。 */
const peakPercentText = (node, key) => {
  if (!finite(node[key])) return null

  const when = peakTime(node[`${key}_at`])
  return when ? `峰值 ${formatPercent(node[key])} at ${when}` : `峰值 ${formatPercent(node[key])}`
}

/* ── 曲线 ──────────────────────────────────────────────────── */

const aggregateEntry = computed(() => series.value.find((item) => item?.aggregate === true) || null)
const nodeEntries = computed(() => series.value.filter((item) => item?.aggregate !== true))

const viewOptions = computed(() => [
  { label: '合计（全部节点）', value: AGGREGATE },
  ...nodeEntries.value.map((item) => ({ label: item.name, value: item.node_id })),
])

const selectedEntry = computed(() =>
  selectedView.value === AGGREGATE
    ? aggregateEntry.value
    : nodeEntries.value.find((item) => item.node_id === selectedView.value) || null,
)

const bucketSeconds = computed(() => BUCKET_SECONDS[windowKey.value] || 300)

/**
 * 增量字节 → 速率（B/s）与累计字节。
 * 24h 的 5 分钟点与 30d 的天点原始值差 288 倍，不换算的话切窗口相当于换了个
 * Y 轴口径，趋势没法比。累计量是**窗口内单调递增**的累计和（逐点把增量加上去），
 * 不是采样表里那台机器的历史累计值 —— 那是个跨重启就会倒退的绝对值。
 */
const chartSamples = computed(() => {
  const points = selectedEntry.value?.points || []
  const seconds = bucketSeconds.value

  let upCum = 0
  let downCum = 0

  return points.map((point) => {
    upCum += finite(point.up) ? point.up : 0
    downCum += finite(point.down) ? point.down : 0

    return {
      t: point.t,
      upRate: finite(point.up) ? point.up / seconds : null,
      downRate: finite(point.down) ? point.down / seconds : null,
      upCum,
      downCum,
    }
  })
})

/** 4 条线：速率走左轴（实线），累计走右轴（虚线）。 */
const chart = computed(() =>
  chartGeometry(
    chartSamples.value,
    [
      { key: 'upRate' },
      { key: 'downRate' },
      { key: 'upCum', axis: 2 },
      { key: 'downCum', axis: 2 },
    ],
    CHART,
  ),
)

/** 左轴刻度（速率）。 */
const yTicksLeft = computed(() => {
  const { ceiling, y } = chart.value

  return [
    { key: 'max', top: y(ceiling), label: formatSpeed(ceiling) },
    { key: 'mid', top: y(ceiling / 2), label: formatSpeed(ceiling / 2) },
    { key: 'zero', top: y(0), label: formatSpeed(0) },
  ]
})

/** 右轴刻度（累计量）。 */
const yTicksRight = computed(() => {
  const { ceiling2, y2 } = chart.value

  return [
    { key: 'max', top: y2(ceiling2), label: formatBytes(ceiling2) },
    { key: 'mid', top: y2(ceiling2 / 2), label: formatBytes(ceiling2 / 2) },
    { key: 'zero', top: y2(0), label: formatBytes(0) },
  ]
})

const gridPath = computed(() => {
  const { ceiling, y } = chart.value

  return [ceiling, ceiling / 2, 0].map((value) => `M0 ${y(value).toFixed(2)}H${CHART.width}`).join(' ')
})

/** 末点四值（图例里的当前值）。取不到就是 null，走格式化函数的占位。 */
const latestPoint = computed(() => {
  const points = chartSamples.value
  return points.length ? points[points.length - 1] : null
})

/**
 * hover 读数：鼠标横向位置 → 最近采样点。
 * 用 SVG 的 viewBox 宽度换算（preserveAspectRatio="none" 下 x 是线性的），
 * 取离光标最近的点而不是插值 —— 插值会显示一个从未采样到的数。
 */
const hoverIndex = ref(null)

const onPlotMove = (event) => {
  const points = chartSamples.value
  if (!points.length) return

  const rect = event.currentTarget.getBoundingClientRect()
  if (!rect.width) return

  const ratio = Math.max(0, Math.min(1, (event.clientX - rect.left) / rect.width))
  const t = chart.value.start + ratio * (chart.value.end - chart.value.start)

  let nearest = 0
  let best = Infinity
  points.forEach((point, index) => {
    const distance = Math.abs(point.t - t)
    if (distance < best) {
      best = distance
      nearest = index
    }
  })

  hoverIndex.value = nearest
}

const onPlotLeave = () => {
  hoverIndex.value = null
}

const hoverPoint = computed(() =>
  hoverIndex.value === null ? null : chartSamples.value[hoverIndex.value] || null,
)

const hoverX = computed(() => (hoverPoint.value ? chart.value.x(hoverPoint.value.t) : null))

/** tooltip 靠右半区时翻到竖线左侧，避免整块飘出卡片。 */
const hoverFlipped = computed(() => hoverX.value !== null && hoverX.value > CHART.width * 0.62)

/**
 * 采样点时间格式化。`t` 是后端给的 **epoch 秒**（DSN loc=UTC，Unix() 是绝对时间），
 * 所以直接 new Date(t * 1000) 就是浏览器本地时区 —— 与「峰值 at 今天 14:20」同一套口径。
 */
const clockOf = (t, withDate) => {
  const at = new Date(t * 1000)
  if (Number.isNaN(at.getTime())) return '—'

  const pad = (n) => String(n).padStart(2, '0')
  const hm = `${pad(at.getHours())}:${pad(at.getMinutes())}`
  return withDate ? `${pad(at.getMonth() + 1)}-${pad(at.getDate())} ${hm}` : hm
}

/** hover tooltip 顶部那行时间（含日期，跨天也能定位）。 */
const hoverTime = computed(() => (hoverPoint.value ? clockOf(hoverPoint.value.t, true) : null))

/**
 * 横轴刻度：等分窗口取 5 个点。
 * 24h 显示「时:分」（窗口内只有一天，日期是噪音）；7d/30d/本月显示「月-日」。
 * 首尾两枚贴边显示（0% / 100% 各自不越界），中间三枚居中。
 */
const xTicks = computed(() => {
  const { start, end } = chart.value
  const points = chartSamples.value.length
  if (!points || !Number.isFinite(start) || !Number.isFinite(end) || end <= start) return []

  const count = 5
  const span = end - start
  const withDate = windowKey.value !== '24h'

  return Array.from({ length: count }, (_, i) => {
    const ratio = i / (count - 1)
    const t = start + span * ratio

    return {
      key: i,
      label: clockOf(Math.round(t), withDate),
      left: ratio * 100,
      // 首枚左对齐、末枚右对齐，中间居中 —— 否则文字会溢出绘图区被卡片裁掉
      align: i === 0 ? 'start' : i === count - 1 ? 'end' : 'center',
    }
  })
})

/** 曲线标题：合计视图说明口径，单节点视图点明是哪台。 */
const chartScope = computed(() =>
  selectedView.value === AGGREGATE ? '全部节点合计' : selectedEntry.value?.name || '节点',
)

/* ── CSV ───────────────────────────────────────────────────── */

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

/** 明细 CSV 纯字符串（导出**全部节点**，与 Top5 同序：降序、无数据在最后）。 */
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

const csvFilename = () => `monitor-${windowKey.value}-${dateStamp()}.csv`

/** 前端 Blob 下载（BOM 让 Excel 认 UTF-8，否则中文表头乱码）。 */
const exportCsv = () => {
  const blob = new Blob([`﻿${buildCsv()}`], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = csvFilename()
  document.body.appendChild(link)
  link.click()
  document.body.removeChild(link)
  URL.revokeObjectURL(url)
  toast.success('已导出 CSV')
}

/* ── 取数 ──────────────────────────────────────────────────── */

const loadBusiness = async () => {
  // 三块各自独立：任何一块挂了都不该把整行清空
  const [usersResult, nodesResult, revenueResult] = await Promise.allSettled([
    adminApi.listUsers({ page: 1 }),
    adminApi.listNodes({ all: 1 }),
    adminApi.getRevenueStats(),
  ])

  if (usersResult.status === 'fulfilled') {
    const data = usersResult.value
    userTotal.value = data.pagination?.total ?? data.length
    users.value = data.slice(0, 5)
  }

  if (nodesResult.status === 'fulfilled') {
    const nodes = Array.isArray(nodesResult.value) ? nodesResult.value : []
    servers.value = { total: nodes.length, online: nodes.filter((n) => n.status === 'online').length }
    inboundTotal.value = nodes.reduce((sum, n) => {
      const inbounds = n.inbounds || {}
      return sum + Object.values(inbounds).reduce((s, arr) => s + (Array.isArray(arr) ? arr.length : 0), 0)
    }, 0)
    // 活跃人数 / 连接数：只认在线节点上报的值（面板节点恒为 null，sumOrNull 会跳过）
    const onlineNodes = nodes.filter((n) => n.status === 'online')
    activeUsers.value = sumOrNull(onlineNodes.map((n) => n.metrics?.active_users))
    activeConns.value = sumOrNull(onlineNodes.map((n) => n.metrics?.conns))
  }

  if (revenueResult.status === 'fulfilled') {
    revenue.value = revenueResult.value
  }
}

const loadTraffic = async () => {
  trafficLoading.value = true
  trafficFailed.value = false

  try {
    const [sum, ser] = await Promise.all([
      adminApi.monitorSummary(windowKey.value),
      adminApi.monitorSeries(windowKey.value),
    ])

    summary.value = sum || null
    series.value = Array.isArray(ser) ? ser : []

    // 选中的视图在新数据里不存在（该节点已删除 / 停用）→ 回到默认的合计视图
    if (selectedView.value !== AGGREGATE && !nodeEntries.value.some((item) => item.node_id === selectedView.value)) {
      selectedView.value = AGGREGATE
    }
  } catch (e) {
    // 失败保留上一次成功的数据，只把状态标成失败并给重试入口 —— 不清空成 0
    trafficFailed.value = true
    toast.error(e?.message || '流量数据加载失败')
  } finally {
    trafficLoading.value = false
  }
}

/* ── 用户 × 机器流量（issue #43） ─────────────────────────── */

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

/* 右栏 CPU / 内存排行：卡片里只放前 3 条，全部走二级弹窗（右栏全展开会过长） */
const RANK_PREVIEW = 3
const allCpuOpen = ref(false)
const allMemOpen = ref(false)

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
/** 机器榜以网卡合计排序；没有网卡数据的机器用用户原始代替做分母（只影响条长，不影响数字）。 */

/** 套餐类型徽章文案：period → 周期·N 个月；total → 总量；无套餐 → —。 */
const planTypeLabel = (row) => {
  if (row?.plan_type === 'period') {
    return row.plan_months ? `周期·${row.plan_months} 个月` : '周期'
  }
  if (row?.plan_type === 'total') return '总量'

  return '—'
}

/**
 * 占比百分比（设计稿：条上叠百分比文字）。
 * 分母为 null / 0 时返回 null（不画 0%，也不画 NaN%）。
 */
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

/* ── 用户流量 CSV（设计稿：导出 CSV） ─────────────────────── */

/**
 * 用户榜 CSV：完整邮箱（导出是台账用途，管理员本来就能在用户列表看到）；
 * 打码是**界面展示**的选择，不该把导出也变成不可用的数据。
 * 缺失值仍输出空字段（不写 0 / 不写「—」，否则整列变文本没法求和）。
 *
 * 参数化 rows/max：卡片内导出 Top 50（userRows），弹窗内导出全量（allUsers）——
 * 两处口径一致，只有数据范围不同。
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

const load = () => {
  loadBusiness()
  loadTraffic()
  loadUserTraffic()
}

/** 服务器在线状态：与节点监控那边同一套口径（全部在线 / 部分离线 / 全部离线）。 */
const serverHealth = computed(() => {
  const { total, online } = servers.value
  if (!total) return { state: 'empty', label: '暂无服务器' }
  if (!online) return { state: 'offline', label: '全部离线' }
  if (online < total) return { state: 'degraded', label: '部分离线' }
  return { state: 'healthy', label: '全部在线' }
})

const money = (value) => `¥${Number(value || 0).toFixed(2)}`

// 切换时间窗立即重取（两个接口都是按 window 查询的）
watch(windowKey, loadTraffic)

onMounted(load)
// keep-alive 页面：切回来拉一次最新的（不是轮询）
onActivated(load)
</script>

<template>
  <div class="ad">
    <h1 class="ch-page-title">仪表盘</h1>

    <!-- 业务概览 -->
    <div class="dsh-cards">
      <!-- 用户总数卡：活跃人数与连接数并进说明行（原来各占一格） -->
      <StatCard label="用户总数" :value="userTotal">
        <template #note>
          <span class="dsh-note-line">活跃 {{ activeUsers ?? '—' }} · 连接 {{ activeConns ?? '—' }}</span>
        </template>
      </StatCard>

      <StatCard
        label="服务器"
        :value="servers.online"
        :denominator="servers.total"
        note="在线 / 总数"
        :health="serverHealth.label"
        :health-state="serverHealth.state"
      />

      <StatCard label="节点" :value="inboundTotal" note="所有服务器的入站配置数" />

      <!-- 今日成交额保持在第一行第 4 格（原来的位置不动） -->
      <StatCard label="今日成交额" :value="revenue ? money(revenue.today.amount) : '—'">
        <template #note>
          <template v-if="revenue">
            {{ revenue.today.orders }} 单 · 累计 {{ money(revenue.total.amount) }}（{{ revenue.total.orders }} 单）
          </template>
          <template v-else>—</template>
        </template>
      </StatCard>
    </div>

    <!-- 节点监控（自带轮询） -->
    <!-- 节点监控（自带轮询）；「最近用户」从页面底部挪进地球右侧的插槽 -->
    <NodeMonitor>
      <template #observatory-side>
        <Card class="dsh-recent-card">
          <div class="dsh-section-head">
            <h2 class="dsh-section-title">最近用户 <span class="dsh-count">{{ userTotal }}</span></h2>
            <RouterLink to="/admin/users" class="dsh-link">查看全部</RouterLink>
          </div>
          <ul v-if="users.length" class="dsh-recent">
            <li v-for="u in users" :key="u.id" class="dsh-recent-item">
              <span class="dsh-recent-email">{{ u.email || `用户 #${u.id}` }}</span>
              <Badge v-if="u.plan_name" :variant="u.plan_type === 'period' ? 'vless' : 'trojan'">{{ u.plan_name }}</Badge>
              <Badge :variant="u.enabled ? 'success' : 'danger'" dot>{{ u.enabled ? '启用' : '禁用' }}</Badge>
            </li>
          </ul>
          <p v-else class="dsh-empty">暂无用户</p>
        </Card>
      </template>
    </NodeMonitor>

    <!-- 流量：窗口选择 -->
    <div v-if="trafficFailed" class="dsh-banner" role="alert">
      <Badge variant="danger" dot>加载失败</Badge>
      <span class="dsh-banner-text">流量数据暂时取不到，下方是上一次成功获取的数据。</span>
      <Button variant="secondary" size="sm" @click="loadTraffic">重试</Button>
    </div>

    <!-- 流量：两栏（左 = 曲线 + Top5，右 = CPU / 内存排行）；窄屏堆叠 -->
    <div class="dsh-traffic">
      <div class="dsh-col dsh-col--wide">
        <!-- 曲线卡：默认全机合计，可切单节点 -->
        <Card v-if="trafficHasData">
          <div class="dsh-section-head is-stack">
            <h2 class="dsh-section-title">{{ trafficTitle }}</h2>
            <div class="dsh-toolbar">
              <Select
                v-model="selectedView"
                wrapper-class="dsh-node-select"
                size="sm"
                :options="viewOptions"
              />
            </div>
          </div>

          <div class="dsh-legend">
            <span class="dsh-legend-item"><i class="dsh-dot dsh-dot--up" aria-hidden="true"></i>上传速率</span>
            <span class="dsh-legend-item"><i class="dsh-dot dsh-dot--down" aria-hidden="true"></i>下载速率</span>
            <span class="dsh-legend-item"><i class="dsh-dot dsh-dot--up dsh-dot--dashed" aria-hidden="true"></i>累计上传</span>
            <span class="dsh-legend-item"><i class="dsh-dot dsh-dot--down dsh-dot--dashed" aria-hidden="true"></i>累计下载</span>
            <span class="dsh-total">↑ 总上传 {{ cellBytes(totals.up) }} · ↓ 总下载 {{ cellBytes(totals.down) }}</span>
          </div>

          <div class="dsh-plot">
            <div class="dsh-plot-inner" @mousemove="onPlotMove" @mouseleave="onPlotLeave">
              <svg
                class="dsh-svg"
                viewBox="0 0 600 180"
                preserveAspectRatio="none"
                role="img"
                :aria-label="`${chartScope} 上下行速率与累计量曲线，${chart.count} 个采样点`"
              >
                <path class="dsh-grid" :d="gridPath" vector-effect="non-scaling-stroke" />
                <path
                  v-for="line in chart.paths"
                  :key="line.key"
                  class="dsh-line"
                  :class="[`dsh-line--${line.key}`, { 'is-cumulative': line.key.endsWith('Cum') }]"
                  :d="line.d"
                  vector-effect="non-scaling-stroke"
                />
                <line
                  v-if="hoverX !== null"
                  class="dsh-hover"
                  :x1="hoverX"
                  :x2="hoverX"
                  y1="0"
                  :y2="CHART.height"
                  vector-effect="non-scaling-stroke"
                />
              </svg>

              <!-- Y 轴刻度叠在绘图区里（左/右各一枚列，不占宽度） -->
              <div class="dsh-ytick dsh-ytick--left" aria-hidden="true">
                <span
                  v-for="tick in yTicksLeft"
                  :key="tick.key"
                  class="dsh-ytick-label"
                  :style="{ top: `${tick.top}px` }"
                >{{ tick.label }}</span>
              </div>
              <div class="dsh-ytick dsh-ytick--right" aria-hidden="true">
                <span
                  v-for="tick in yTicksRight"
                  :key="tick.key"
                  class="dsh-ytick-label"
                  :style="{ top: `${tick.top}px` }"
                >{{ tick.label }}</span>
              </div>

              <!-- 横轴时间刻度：压在绘图区底部一行，不额外占卡片高度 -->
              <div class="dsh-xtick" aria-hidden="true">
                <span
                  v-for="tick in xTicks"
                  :key="tick.key"
                  class="dsh-xtick-label"
                  :class="`is-${tick.align}`"
                  :style="{ left: `${tick.left}%` }"
                >{{ tick.label }}</span>
              </div>

              <!-- hover 读数：贴着竖线显示，不占图例（图例不带数值） -->
              <div
                v-if="hoverPoint"
                class="dsh-tip"
                :class="{ 'is-flip': hoverFlipped }"
                :style="{ left: `${(hoverX / CHART.width) * 100}%` }"
              >
                <span class="dsh-tip-time">{{ hoverTime }}</span>
                <span><i class="dsh-dot dsh-dot--up" aria-hidden="true"></i>上传速率 {{ formatSpeed(hoverPoint.upRate) }}</span>
                <span><i class="dsh-dot dsh-dot--down" aria-hidden="true"></i>下载速率 {{ formatSpeed(hoverPoint.downRate) }}</span>
                <span><i class="dsh-dot dsh-dot--up dsh-dot--dashed" aria-hidden="true"></i>累计上传 {{ formatBytes(hoverPoint.upCum) }}</span>
                <span><i class="dsh-dot dsh-dot--down dsh-dot--dashed" aria-hidden="true"></i>累计下载 {{ formatBytes(hoverPoint.downCum) }}</span>
              </div>
            </div>
          </div>

          <p v-if="chart.count < 2" class="dsh-plot-empty">{{ chartScope }} 在此时间窗内暂无采样数据</p>
          <p v-else class="dsh-note">
            左轴是速率（区间增量 ÷ 区间时长，{{ windowKey === '24h' ? '5 分钟' : windowKey === '7d' ? '1 小时' : '1 天' }}一个点，实线）；
            右轴是窗口内累计量（虚线，单调递增）。汇总在后端按同一桶归并后再相加，各节点采样时刻的秒级差不会造成假峰谷。
          </p>
        </Card>

        <!-- 流量消耗 Top 5 -->
        <Card>
          <div class="dsh-section-head">
            <h2 class="dsh-section-title">流量消耗 Top 5</h2>
            <div class="dsh-toolbar">
              <Select v-model="windowKey" wrapper-class="dsh-window-select" size="sm" :options="WINDOWS" />
              <Button variant="secondary" size="sm" :loading="trafficLoading" @click="loadTraffic">
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

          <p v-if="trafficLoading && !summary" class="dsh-empty">正在加载流量数据…</p>

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
        </Card>
      </div>

      <div class="dsh-col dsh-col--narrow">
        <!-- 平均 CPU 占用排行（卡片内只显示前 3，全部走弹窗） -->
        <Card>
          <div class="dsh-section-head">
            <h2 class="dsh-section-title">平均 CPU 占用排行</h2>
            <Button
              v-if="cpuRanked.length > RANK_PREVIEW"
              variant="ghost"
              size="sm"
              @click="allCpuOpen = true"
            >
              查看全部
            </Button>
          </div>

          <p v-if="trafficLoading && !summary" class="dsh-empty">正在加载…</p>

          <ul v-else-if="cpuRanked.length" class="dsh-rank">
            <li v-for="(node, index) in cpuRanked.slice(0, RANK_PREVIEW)" :key="node.id" class="dsh-rank-item">
              <div class="dsh-rank-head">
                <span class="dsh-rank-seq">{{ index + 1 }}.</span>
                <span class="dsh-rank-name">{{ node.name }}</span>
                <strong class="dsh-rank-value">{{ cellPercent(node.cpu_avg) }}</strong>
                <Badge v-if="!finite(node.cpu_avg)" variant="info" class="dsh-rank-badge">无数据</Badge>
              </div>
              <p v-if="peakPercentText(node, 'cpu_max')" class="dsh-rank-peak">
                {{ peakPercentText(node, 'cpu_max') }}
              </p>
              <i class="dsh-bar dsh-bar--cpu" :class="{ 'is-unknown': !finite(node.cpu_avg) }">
                <b :style="{ width: barWidth(node.cpu_avg, maxCpu) }"></b>
              </i>
            </li>
          </ul>

          <p v-else class="dsh-empty">暂无采样数据：采集随每 5 分钟健康检查进行，稍后即可看到</p>
        </Card>

        <!-- 平均内存占用百分比排行（同样只显示前 3） -->
        <Card>
          <div class="dsh-section-head">
            <h2 class="dsh-section-title">平均内存占用百分比排行</h2>
            <Button
              v-if="memRanked.length > RANK_PREVIEW"
              variant="ghost"
              size="sm"
              @click="allMemOpen = true"
            >
              查看全部
            </Button>
          </div>

          <p v-if="trafficLoading && !summary" class="dsh-empty">正在加载…</p>

          <ul v-else-if="memRanked.length" class="dsh-rank">
            <li v-for="(node, index) in memRanked.slice(0, RANK_PREVIEW)" :key="node.id" class="dsh-rank-item">
              <div class="dsh-rank-head">
                <span class="dsh-rank-seq">{{ index + 1 }}.</span>
                <span class="dsh-rank-name">{{ node.name }}</span>
                <strong class="dsh-rank-value">{{ cellPercent(node.mem_avg) }}</strong>
                <Badge v-if="!finite(node.mem_avg)" variant="info" class="dsh-rank-badge">无数据</Badge>
              </div>
              <p v-if="peakPercentText(node, 'mem_max')" class="dsh-rank-peak">
                {{ peakPercentText(node, 'mem_max') }}
              </p>
              <i class="dsh-bar dsh-bar--mem" :class="{ 'is-unknown': !finite(node.mem_avg) }">
                <b :style="{ width: barWidth(node.mem_avg, maxMem) }"></b>
              </i>
            </li>
          </ul>

          <p v-else class="dsh-empty">暂无采样数据：采集随每 5 分钟健康检查进行，稍后即可看到</p>
        </Card>
      </div>
    </div>

    <!-- 用户 × 机器流量（issue #43）：用户榜表格（可展开按机器明细）+ 机器卡（网卡/用户原始/倍率并排） -->
    <div class="dsh-ut">
      <!-- 上：用户流量排行（整行宽度：7 列表格） -->
      <Card class="dsh-ut-card">
        <div class="dsh-section-head is-stack">
          <h2 class="dsh-section-title">用户流量排行 · {{ userWindowLabel }}</h2>
          <div class="dsh-toolbar">
            <div class="dsh-windows" role="group" aria-label="用户流量时间窗">
              <button
                v-for="w in USER_WINDOWS"
                :key="w.value"
                type="button"
                class="dsh-win"
                :class="{ 'is-active': userWindow === w.value }"
                :aria-pressed="userWindow === w.value"
                @click="userWindow = w.value"
              >{{ w.label }}</button>
            </div>
            <Button variant="secondary" size="sm" :loading="userTrafficLoading" @click="loadUserTraffic">
              <Icon name="refresh" :size="16" /> 刷新
            </Button>
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
      </Card>
    </div>

    <!-- 数据起始日：新表从上线当天累积，7d/30d 要等满（缺数据 ≠ 0） -->
    <p v-if="userDataStart" class="dsh-note dsh-ut-start">
      数据自 {{ userDataStart }} 起开始累积：24 小时窗口当天可用，7 天 / 30 天窗口需等满才有完整数据；窗口内无数据的用户不显示。
    </p>
    <p v-else-if="!userTrafficLoading" class="dsh-note dsh-ut-start">
      暂无数据：用户流量从上线当天开始累积（24 小时窗口当天可用）。
    </p>

    <!--
      「查看全部」两个弹窗（issue #47）：卡片内只留 Top 5，全量按需展开。
      机器弹窗直接复用 Top5 卡那一套行 UI（同一套 dsh-rank 样式），只是行数与分母换成全部机器；
      用户弹窗复用用户榜表格，去掉每行的「明细」展开列（弹窗里再套一层展开会更晕）。
    -->
    <!-- 右栏两个排行的「全部」二级弹窗：卡片内只放前 3 条，全量在这里展 -->
    <Modal v-model="allCpuOpen" title="平均 CPU 占用排行 · 全部节点" width="520px">
      <p class="dsh-modal-hint">共 {{ cpuRanked.length }} 台在线节点，按平均占用降序</p>

      <ul v-if="cpuRanked.length" class="dsh-rank">
        <li v-for="(node, index) in cpuRanked" :key="node.id" class="dsh-rank-item">
          <div class="dsh-rank-head">
            <span class="dsh-rank-seq">{{ index + 1 }}.</span>
            <span class="dsh-rank-name">{{ node.name }}</span>
            <strong class="dsh-rank-value">{{ cellPercent(node.cpu_avg) }}</strong>
            <Badge v-if="!finite(node.cpu_avg)" variant="info" class="dsh-rank-badge">无数据</Badge>
          </div>
          <p v-if="peakPercentText(node, 'cpu_max')" class="dsh-rank-peak">
            {{ peakPercentText(node, 'cpu_max') }}
          </p>
          <i class="dsh-bar dsh-bar--cpu" :class="{ 'is-unknown': !finite(node.cpu_avg) }">
            <b :style="{ width: barWidth(node.cpu_avg, maxCpu) }"></b>
          </i>
        </li>
      </ul>
      <p v-else class="dsh-empty">暂无采样数据</p>
    </Modal>

    <Modal v-model="allMemOpen" title="平均内存占用百分比排行 · 全部节点" width="520px">
      <p class="dsh-modal-hint">共 {{ memRanked.length }} 台在线节点，按平均占用降序</p>

      <ul v-if="memRanked.length" class="dsh-rank">
        <li v-for="(node, index) in memRanked" :key="node.id" class="dsh-rank-item">
          <div class="dsh-rank-head">
            <span class="dsh-rank-seq">{{ index + 1 }}.</span>
            <span class="dsh-rank-name">{{ node.name }}</span>
            <strong class="dsh-rank-value">{{ cellPercent(node.mem_avg) }}</strong>
            <Badge v-if="!finite(node.mem_avg)" variant="info" class="dsh-rank-badge">无数据</Badge>
          </div>
          <p v-if="peakPercentText(node, 'mem_max')" class="dsh-rank-peak">
            {{ peakPercentText(node, 'mem_max') }}
          </p>
          <i class="dsh-bar dsh-bar--mem" :class="{ 'is-unknown': !finite(node.mem_avg) }">
            <b :style="{ width: barWidth(node.mem_avg, maxMem) }"></b>
          </i>
        </li>
      </ul>
      <p v-else class="dsh-empty">暂无采样数据</p>
    </Modal>

    <Modal v-model="allNodesOpen" :title="`全部机器流量 · ${trafficTitle}`" width="640px">
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

          <!-- 弹窗内保留「点用户看按机器明细」（issue #47 明确要求），与卡片内是同一份交互 -->
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
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Dashboard.css"></style>
