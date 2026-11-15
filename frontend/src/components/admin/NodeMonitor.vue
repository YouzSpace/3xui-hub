<script setup>
/**
 * 节点监控区块（仪表盘用）：汇总卡 + 地球/实时网络 + 节点卡片。
 *
 * 数据源：复用现有的 GET /admin-api/nodes?all=1（不新增接口），5 秒轮询一次。
 * 两种节点的数据新鲜度不同，如实显示、不假装实时：
 *   - xray 裸内核节点：agent 每 5 秒上报（cpu/内存/连接数/活跃用户/实时上下行）
 *   - 3x-ui 面板节点：健康检查每 5 分钟一次（只有 cpu/内存），卡片上标「x 分钟前」
 *
 * 实时曲线是**本次浏览观测**：每轮把当前值记一个点，只保留最近 5 分钟，刷新即清空。
 * 不落库、不做历史 —— 历史趋势需要服务端采样表，是另一件事。
 */
import { computed, onActivated, onBeforeUnmount, onDeactivated, onMounted, ref } from 'vue'
import Card from '@/components/ui/Card.vue'
import Badge from '@/components/ui/Badge.vue'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import Select from '@/components/ui/Select.vue'
import StatCard from '@/components/ui/StatCard.vue'
import NodeGlobe from '@/components/admin/NodeGlobe.vue'
import { adminApi } from '@/api/admin'
import { chartGeometry } from '@/utils/chart'
import { regionFlag, regionName } from '@/utils/globe'
import {
  average,
  finite,
  formatBytes,
  formatPercent,
  formatSpeed,
  relativeTime,
  sumOrNull,
  usageLevel,
} from '@/utils/format'

/** 轮询间隔：与 agent 的上报节奏对齐，再快拿到的也是重复值。 */
const POLL_MS = 5000
/** 24h 汇总的取数间隔：全表聚合，没必要跟着 5 秒轮询跑。 */
const SUMMARY_MS = 60 * 1000
/** 采样缓存窗口（毫秒）：3 分钟曲线窗口 + 富余（页面隐藏丢失的采样也能兜住）。 */
const WINDOW_MS = 5 * 60 * 1000
/** 曲线展示窗口（秒）：三张曲线卡统一 3 分钟（用户定稿 2026-11-06）。 */
const SPARK_WINDOW_SEC = 180

const nodes = ref([])
const regions = ref([])
const loading = ref(true)
const failed = ref(false)
const lastSuccessAt = ref(0)
/** 「节点一览」默认折叠（用户定稿）：标题右侧三角按钮展开；只折叠不告警。 */
const nodesOpen = ref(false)
const globeRef = ref(null)
/** 24h 监控汇总（提供「近 24 小时流量」合计与 TOP1 机器） */
const trafficSummary = ref(null)
/** 上次取 24h 汇总的时间戳（降频用，非响应式）。 */
let lastSummaryAt = 0

/** 本次浏览观测到的采样点：{ t, up, down, cpu, mem, online } */
const samples = ref([])
/** 每秒刷新的“现在”，只用于让相对时间（「更新于 N 秒前」）自己走字。 */
const now = ref(Date.now())
/**
 * 图表专用的“现在”，只跟轮询同频（POLL_MS）更新。
 *
 * 为什么和 now 拆开：曲线窗口的滑动（windowSamples → liveSpark /
 * cpuSpark / memSpark）会重算 SVG 路径，而采样点本来就是每 POLL_MS 一个。
 * 若也挂在每秒的 now 上，就等于每秒白算 3 次 chartGeometry —— 节点多时肉眼可见地卡。
 * 拆开后：now 每秒只 patch 那一处文本节点，图表仍保持 5 秒重算一次，两边都正确。
 */
const chartNow = ref(Date.now())

let pollTimer = null
let clockTimer = null

/* ── 取数 ──────────────────────────────────────────────────── */

const onlineNodes = computed(() => nodes.value.filter((node) => node.status === 'online'))

const avgCpu = computed(() => average(onlineNodes.value.map((node) => node.metrics?.cpu)))
const avgMem = computed(() => average(onlineNodes.value.map((node) => node.metrics?.mem)))

/** 覆盖地区：能定位的节点算进来的地区数；在线地区 = 至少有一个在线节点的地区。 */
const coverage = computed(() => {
  const all = new Set()
  const online = new Set()
  for (const node of nodes.value) {
    if (!node.region) continue
    all.add(node.region)
    if (node.status === 'online') online.add(node.region)
  }

  return { total: all.size, online: online.size }
})

/** 实时上下行合计。全都拿不到就是 null —— 不能显示成 0 B/s。 */
const liveUp = computed(() => sumOrNull(onlineNodes.value.map((node) => node.metrics?.up_speed)))
const liveDown = computed(() => sumOrNull(onlineNodes.value.map((node) => node.metrics?.down_speed)))

/** 实时速率卡的**大字**：上下行总和（用户定稿）。两边都无数据才是 null。 */
const liveTotal = computed(() => {
  if (liveUp.value === null && liveDown.value === null) return null

  return (liveUp.value ?? 0) + (liveDown.value ?? 0)
})

/**
 * 近 24 小时全网流量（网卡口径，与曲线卡同一个 totals）。
 * 拿不到就是 null（不兜底成 0），卡片显示「—」。
 */
const traffic24h = computed(() => {
  const up = trafficSummary.value?.totals?.up
  const down = trafficSummary.value?.totals?.down
  const ok = finite(up) || finite(down)

  return {
    up: finite(up) ? up : null,
    down: finite(down) ? down : null,
    total: ok ? (finite(up) ? up : 0) + (finite(down) ? down : 0) : null,
  }
})

/** 24h 流量最大的机器（徽章用）；全无数据返回 null。 */
const trafficTopNode = computed(() => {
  const rows = Array.isArray(trafficSummary.value?.nodes) ? trafficSummary.value.nodes : []
  const ranked = rows.filter((n) => finite(n.total)).sort((a, b) => b.total - a.total)
  return ranked[0] || null
})

/** 实时曲线窗口内的采样点（三张曲线卡统一 3 分钟）。 */
const windowSamples = computed(() => {
  const from = chartNow.value - SPARK_WINDOW_SEC * 1000

  return samples.value.filter((sample) => sample.t >= from)
})

/** 汇总卡里的实时速率迷你走势：**只画总和一根**（用户定稿）。 */
const liveSpark = computed(() =>
  chartGeometry(
    windowSamples.value.map((sample) => ({
      ...sample,
      // 单个方向缺失按 0 计，两个方向都在才是一条完整的和
      total: (finite(sample.up) ? sample.up : 0) + (finite(sample.down) ? sample.down : 0),
    })),
    [{ key: 'total' }],
    { width: 160, height: 34, padTop: 3, padBottom: 3 },
  ),
)

/** 汇总卡里的迷你曲线（CPU / 内存，按 0–100 固定刻度），同样走 3 分钟窗口。 */
const cpuSpark = computed(() =>
  chartGeometry(windowSamples.value, [{ key: 'cpu' }], { width: 160, height: 34, padTop: 3, padBottom: 3, max: 100 }),
)
const memSpark = computed(() =>
  chartGeometry(windowSamples.value, [{ key: 'mem' }], { width: 160, height: 34, padTop: 3, padBottom: 3, max: 100 }),
)

const unmapped = computed(() => nodes.value.filter((node) => !node.region).length)

/* ── 轮询与采样 ────────────────────────────────────────────── */

function pushSample() {
  const at = Date.now()
  const online = onlineNodes.value

  samples.value = [
    ...samples.value.filter((sample) => sample.t >= at - WINDOW_MS),
    {
      t: at,
      up: sumOrNull(online.map((node) => node.metrics?.up_speed)),
      down: sumOrNull(online.map((node) => node.metrics?.down_speed)),
      cpu: average(online.map((node) => node.metrics?.cpu)),
      mem: average(online.map((node) => node.metrics?.mem)),
      online: online.length,
    },
  ]
}

async function load() {
  try {
    // 节点列表每次轮询都要；24h 汇总则降频取（见 SUMMARY_MS）——它是全表聚合，
    // 跟着 5 秒轮询跑等于每 5 秒对全部节点重算一次 24h，纯属白压数据库。
    const needSummary = Date.now() - lastSummaryAt >= SUMMARY_MS
    const [data, summary] = await Promise.all([
      adminApi.listNodes({ all: 1 }),
      needSummary ? adminApi.monitorSummary('24h').catch(() => null) : Promise.resolve(null),
    ])
    nodes.value = Array.isArray(data) ? data : []
    if (needSummary) {
      // 取到才覆盖：一次抖动不要把卡片打回「—」
      if (summary) trafficSummary.value = summary
      lastSummaryAt = Date.now()
    }
    lastSuccessAt.value = Date.now()
    failed.value = false
    pushSample()
  } catch (e) {
    // 失败保留上一次数据，只把状态标成「数据中断」—— 不要把页面清空成 0
    failed.value = true
  } finally {
    loading.value = false
  }
}

function startTimers() {
  stopTimers()
  pollTimer = setInterval(() => {
    // 标签页不可见时不打接口（数据本来也没人看）
    if (!document.hidden) load()
  }, POLL_MS)
  // 这个 1 秒时钟只驱动 relativeTime 那一段文本（「更新于 N 秒前」）。
  // 它**不**参与任何图表计算 —— 图表窗口看的是 chartNow（见下）。
  clockTimer = setInterval(() => {
    now.value = Date.now()
  }, 1000)
}

function stopTimers() {
  if (pollTimer) clearInterval(pollTimer)
  if (clockTimer) clearInterval(clockTimer)
  pollTimer = null
  clockTimer = null
}

/* ── 交互 ──────────────────────────────────────────────────── */

function focusRegion(code) {
  globeRef.value?.focusRegion(code)
}

function statusVariant(status) {
  return { online: 'success', offline: 'danger', maintenance: 'warning' }[status] || 'info'
}

function statusText(status) {
  return { online: '在线', offline: '离线', maintenance: '维护' }[status] || status
}

/**
 * agent 版本是否落后于面板（manifest 里的最新版）。
 *
 * 只有【上报了版本 + 面板也给了最新版 + 两者不等】才算落后：
 * 老 agent 不上报版本时显示「未上报」，不标黄（那不是「落后」，是「不知道」）。
 */
function agentOutdated(node) {
  const have = node.agent_version
  const want = node.agent_latest
  return !!have && !!want && have !== want
}

/** agent 那格的悬浮说明：把「现在多少、面板多少」讲清楚。 */
function agentTitle(node) {
  const have = node.agent_version || '未上报'
  const want = node.agent_latest
  if (!want) return `节点 agent ${have}`
  if (!node.agent_version) return `节点还没上报 agent 版本；面板最新 ${want}`
  if (have === want) return `已是最新 ${have}`
  return `节点 ${have}，面板最新 ${want}（可在「节点列表」里升级）`
}

/** 进度条宽度：没有数据就不画（不是 0%）。 */
function barWidth(value) {
  if (!finite(value) || value < 0) return '0%'

  return `${Math.max(2, Math.min(100, value))}%`
}

function onVisibilityChange() {
  if (!document.hidden) {
    load()
    now.value = Date.now()
  }
}

onMounted(() => {
  load()
  startTimers()
  document.addEventListener('visibilitychange', onVisibilityChange)
})

// keep-alive 页面：切回来立即拉一次，切走时停掉轮询
onActivated(() => {
  load()
  startTimers()
})
onDeactivated(stopTimers)

onBeforeUnmount(() => {
  stopTimers()
  document.removeEventListener('visibilitychange', onVisibilityChange)
})
</script>

<template>
  <div class="nm">
    <div v-if="failed" class="nm-banner" role="alert">
      <Badge variant="danger" dot>数据中断</Badge>
      <span>节点状态暂不可确认，正在重试；下方显示的是最后一次成功获取的数据。</span>
    </div>

    <!-- 监控汇总（活跃人数/连接数已并进上面业务概览的用户总数卡，这里不再重复） -->
    <div class="nm-summary">
      <!-- 近 24 小时流量：跟着 CPU/内存同一行，不再单独占一行 -->
      <StatCard
        label="近 24 小时流量"
        :value="formatBytes(traffic24h.total)"
        :health="trafficTopNode ? `TOP1 ${trafficTopNode.name}` : ''"
        health-state="top"
      >
        <template #note>
          <span class="nm-note-line">↑ 上传 {{ formatBytes(traffic24h.up) }} · ↓ 下载 {{ formatBytes(traffic24h.down) }}</span>
        </template>
      </StatCard>

      <StatCard label="平均 CPU" :value="formatPercent(avgCpu, 1)" note="在线节点的平均值" spark>
        <svg class="nm-spark" viewBox="0 0 160 34" preserveAspectRatio="none" aria-hidden="true">
          <path class="nm-area nm-area--cpu" :d="cpuSpark.areaPath[0]" />
          <path class="nm-line nm-line--cpu" vector-effect="non-scaling-stroke" :d="cpuSpark.paths[0]?.d" />
        </svg>
      </StatCard>

      <StatCard label="平均内存" :value="formatPercent(avgMem, 1)" note="在线节点的平均值" spark>
        <svg class="nm-spark" viewBox="0 0 160 34" preserveAspectRatio="none" aria-hidden="true">
          <path class="nm-area nm-area--mem" :d="memSpark.areaPath[0]" />
          <path class="nm-line nm-line--mem" vector-effect="non-scaling-stroke" :d="memSpark.paths[0]?.d" />
        </svg>
      </StatCard>

      <!-- 实时速率：大字是上下行**总和**，说明行才分开写上传/下载；曲线也只画总和一根 -->
      <StatCard label="实时速率" :value="formatSpeed(liveTotal)" note="" spark>
        <template #note>
          <span class="nm-note-line">
            ↑ 上传 {{ formatSpeed(liveUp) }} · ↓ 下载 {{ formatSpeed(liveDown) }}
          </span>
        </template>
        <svg class="nm-spark" viewBox="0 0 160 34" preserveAspectRatio="none" aria-hidden="true">
          <path class="nm-area nm-area--total" :d="liveSpark.areaPath[0]" />
          <path class="nm-line nm-line--total" vector-effect="non-scaling-stroke" :d="liveSpark.paths[0]?.d" />
        </svg>
      </StatCard>
    </div>

    <!-- 地球（左）+ 父组件传入的侧卡（右，现在放「最近用户」；原来这里是速率大卡） -->
    <div class="nm-observatory">
      <Card class="nm-panel">
        <div class="nm-panel-head">
          <h2 class="nm-panel-title">节点地区</h2>
          <!-- 覆盖地区从上面的汇总卡移到这里（右上角），省一格 -->
          <div class="nm-coverage">
            <span class="nm-coverage-value">
              覆盖 {{ coverage.total }} 个地区
            </span>
            <span class="nm-coverage-note">
              {{ coverage.online }} 个有在线节点<template v-if="unmapped"> · {{ unmapped }} 个无法识别</template>
            </span>
          </div>
        </div>

        <div class="nm-globe-row">
          <NodeGlobe
            ref="globeRef"
            :nodes="nodes"
            @regions="regions = $event"
          />
          <div class="nm-globe-side">
            <ul class="nm-region-list">
              <li v-for="region in regions" :key="region.code">
                <button type="button" @click="focusRegion(region.code)">
                  <span class="nm-led" :class="{ 'is-lit': region.online > 0 }" aria-hidden="true"></span>
                  <span class="nm-region-name">{{ regionName(region.code) }}</span>
                  <strong>{{ region.online }}<small> / {{ region.total }}</small></strong>
                </button>
              </li>
              <li v-if="!regions.length" class="nm-region-empty">暂无可定位的地区</li>
            </ul>
            <p class="nm-note nm-note--tight">拖动旋转 · 地区为示意位置</p>
          </div>
        </div>
      </Card>

      <slot name="observatory-side" />
    </div>

    <!-- 节点卡片：默认折叠，标题右侧三角按钮（只折叠，不做告警） -->
    <div class="nm-nodes-head">
      <button
        type="button"
        class="nm-nodes-toggle"
        :aria-expanded="nodesOpen"
        @click="nodesOpen = !nodesOpen"
      >
        <h2 class="nm-panel-title">节点一览 <span class="nm-count">{{ nodes.length }}</span></h2>
        <Icon :name="nodesOpen ? 'chevron-down' : 'chevron-right'" :size="18" />
      </button>
      <span class="nm-updated">
        {{ lastSuccessAt ? `更新于 ${relativeTime(new Date(lastSuccessAt).toISOString(), now)}` : '尚未更新' }}
      </span>
    </div>

    <div v-show="nodesOpen" class="nm-nodes">
      <Card v-for="node in nodes" :key="node.id" class="nm-node">
        <div class="nm-node-head">
          <span class="nm-node-flag" aria-hidden="true">{{ regionFlag(node.region) || '·' }}</span>
          <div class="nm-node-title">
            <h3>{{ node.name }}</h3>
            <p>
              <span class="nm-node-sub">
                {{ node.region ? regionName(node.region) : '未识别地区' }} ·
                {{ node.driver_type === 'xray' ? 'xray 节点' : '面板节点' }}
              </span>
              <!-- agent 版本挂在副标题这一行的右端：点 + 版本号作为一组右对齐，
                   与 manifest 不一致时版本号标黄。 -->
              <span v-if="node.driver_type === 'xray'" class="nm-agent-group">
                <span class="nm-agent-sep" aria-hidden="true">·</span>
                <span
                  :class="['nm-agent', { 'is-outdated': agentOutdated(node) }]"
                  :title="agentTitle(node)"
                >
                  agent {{ node.agent_version || '未上报' }}
                </span>
              </span>
            </p>
          </div>
          <Badge :variant="statusVariant(node.status)" dot>{{ statusText(node.status) }}</Badge>
        </div>

        <div class="nm-meters">
          <div class="nm-meter" :class="`is-${usageLevel(node.metrics?.cpu)}`">
            <span>CPU</span>
            <strong>{{ formatPercent(node.metrics?.cpu) }}</strong>
            <i class="nm-bar"><b :style="{ width: barWidth(node.metrics?.cpu) }"></b></i>
          </div>
          <div class="nm-meter" :class="`is-${usageLevel(node.metrics?.mem)}`">
            <span>内存</span>
            <strong>{{ formatPercent(node.metrics?.mem) }}</strong>
            <i class="nm-bar"><b :style="{ width: barWidth(node.metrics?.mem) }"></b></i>
          </div>
        </div>

        <div class="nm-node-foot">
          <span v-if="node.driver_type === 'xray'">
            连接 {{ node.metrics?.conns ?? '—' }} · 活跃 {{ node.metrics?.active_users ?? '—' }}
          </span>
          <span v-else>延迟 {{ node.latency }}ms</span>
          <span v-if="node.driver_type === 'xray'">
            ↑ {{ formatSpeed(node.metrics?.up_speed) }} ↓ {{ formatSpeed(node.metrics?.down_speed) }}
          </span>
          <span class="nm-fresh">{{ relativeTime(node.metrics?.at, now) }}</span>
        </div>
      </Card>

      <Card v-if="!loading && !nodes.length" class="nm-empty">尚未添加节点</Card>
    </div>
  </div>
</template>

<style scoped src="@/assets/styles/components/admin/NodeMonitor.css"></style>
