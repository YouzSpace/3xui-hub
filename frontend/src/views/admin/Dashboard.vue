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
import TrafficPanel from '@/components/admin/TrafficPanel.vue'
import { useToast } from '@/components/ui/Toast.vue'
import { adminApi } from '@/api/admin'
import { chartGeometry } from '@/utils/chart'
import { finite, formatBytes, formatPercent, formatSpeed, peakTime, sumOrNull } from '@/utils/format'

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

/** 字节单元格。formatBytes 对 null / 负数返回 '—'。 */
const cellBytes = (value) => formatBytes(value)

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

/** 百分比单元格（排行卡右侧的平均值）。formatPercent 对 null 返回 '—'。 */
const cellPercent = (value) => formatPercent(value, 1)

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

const load = () => {
  loadBusiness()
  loadTraffic()
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

        <!-- 流量卡（服务器口径 Top5 + 用户套餐口径排行，见 TrafficPanel.vue）：
             与曲线卡同列、同宽，纵向接着排；两列各自堆叠，互不拉伸 -->
        <TrafficPanel
          v-model:window-key="windowKey"
          :nodes="trafficNodes"
          :loading="trafficLoading"
          @refresh="loadTraffic"
        />
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

  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Dashboard.css"></style>
