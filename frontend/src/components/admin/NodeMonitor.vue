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
import Select from '@/components/ui/Select.vue'
import StatCard from '@/components/ui/StatCard.vue'
import NodeGlobe from '@/components/admin/NodeGlobe.vue'
import { adminApi } from '@/api/admin'
import { chartGeometry } from '@/utils/chart'
import { regionFlag, regionName } from '@/utils/globe'
import {
  average,
  finite,
  formatPercent,
  formatSpeed,
  relativeTime,
  sumOrNull,
  usageLevel,
} from '@/utils/format'

/** 轮询间隔：与 agent 的上报节奏对齐，再快拿到的也是重复值。 */
const POLL_MS = 5000
/** 实时曲线保留窗口（毫秒）。 */
const WINDOW_MS = 5 * 60 * 1000

const nodes = ref([])
const regions = ref([])
const loading = ref(true)
const failed = ref(false)
const lastSuccessAt = ref(0)
const liveWindow = ref(60)
const motionOn = ref(true)
const globeRef = ref(null)

/** 本次浏览观测到的采样点：{ t, up, down, cpu, mem, online } */
const samples = ref([])
/** 每秒刷新的“现在”，只用于让相对时间（「更新于 N 秒前」）自己走字。 */
const now = ref(Date.now())
/**
 * 图表专用的“现在”，只跟轮询同频（POLL_MS）更新。
 *
 * 为什么和 now 拆开：曲线窗口的滑动（windowSamples → liveChart / livePeak /
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

/** 实时连接数 / 活跃用户合计（只有 xray 节点上报，面板节点是 null）。 */
const liveConns = computed(() => sumOrNull(onlineNodes.value.map((node) => node.metrics?.conns)))
const liveUsers = computed(() => sumOrNull(onlineNodes.value.map((node) => node.metrics?.active_users)))

/** 实时曲线窗口内的采样点。 */
const windowSamples = computed(() => {
  const from = chartNow.value - liveWindow.value * 1000

  return samples.value.filter((sample) => sample.t >= from)
})

const liveChart = computed(() =>
  chartGeometry(
    windowSamples.value,
    [{ key: 'up' }, { key: 'down' }],
    { width: 600, height: 150, padTop: 10, padBottom: 10 },
  ),
)

const livePeak = computed(() => {
  const values = windowSamples.value.flatMap((sample) => [sample.up, sample.down]).filter(finite)

  return values.length ? Math.max(...values) : null
})

/** 汇总卡里的迷你曲线（CPU / 内存，按 0–100 固定刻度）。 */
const cpuSpark = computed(() =>
  chartGeometry(samples.value, [{ key: 'cpu' }], { width: 160, height: 34, padTop: 3, padBottom: 3, max: 100 }),
)
const memSpark = computed(() =>
  chartGeometry(samples.value, [{ key: 'mem' }], { width: 160, height: 34, padTop: 3, padBottom: 3, max: 100 }),
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
    const data = await adminApi.listNodes({ all: 1 })
    nodes.value = Array.isArray(data) ? data : []
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

function toggleMotion() {
  globeRef.value?.toggleMotion()
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

    <!-- 监控汇总（在线数在页面的业务概览里，这里不重复） -->
    <div class="nm-summary">
      <StatCard
        label="实时连接"
        :value="liveConns ?? '—'"
        :note="`活跃用户 ${liveUsers ?? '—'} · 仅 xray 节点上报`"
      />

      <StatCard label="平均 CPU" :value="formatPercent(avgCpu, 1)" note="在线节点的平均值">
        <svg class="nm-spark" viewBox="0 0 160 34" preserveAspectRatio="none" aria-hidden="true">
          <path class="nm-line nm-line--cpu" vector-effect="non-scaling-stroke" :d="cpuSpark.paths[0]?.d" />
        </svg>
      </StatCard>

      <StatCard label="平均内存" :value="formatPercent(avgMem, 1)" note="在线节点的平均值">
        <svg class="nm-spark" viewBox="0 0 160 34" preserveAspectRatio="none" aria-hidden="true">
          <path class="nm-line nm-line--mem" vector-effect="non-scaling-stroke" :d="memSpark.paths[0]?.d" />
        </svg>
      </StatCard>

      <StatCard label="覆盖地区" :value="coverage.total" suffix="个">
        <template #note>
          {{ coverage.online }} 个地区有在线节点<template v-if="unmapped"> · {{ unmapped }} 个节点无法识别地区</template>
        </template>
      </StatCard>
    </div>

    <!-- 地球 + 实时网络 -->
    <div class="nm-observatory">
      <Card class="nm-panel">
        <div class="nm-panel-head">
          <h2 class="nm-panel-title">节点地区</h2>
          <Button variant="secondary" size="sm" @click="toggleMotion">
            {{ motionOn ? '暂停旋转' : '开始旋转' }}
          </Button>
        </div>

        <div class="nm-globe-row">
          <NodeGlobe
            ref="globeRef"
            :nodes="nodes"
            @regions="regions = $event"
            @motion="motionOn = $event"
          />
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
        </div>

        <p class="nm-note">
          位置为国家 / 地区示意，非机房精确位置；连线与光点为示意动画，不代表真实链路或流量。
        </p>
      </Card>

      <Card class="nm-panel">
        <div class="nm-panel-head">
          <h2 class="nm-panel-title">上下行速率</h2>
          <Select
            v-model="liveWindow"
            size="sm"
            :options="[
              { label: '最近 60 秒', value: 60 },
              { label: '最近 5 分钟', value: 300 },
            ]"
          />
        </div>

        <div class="nm-live">
          <div>
            <span class="nm-legend nm-legend--up" aria-hidden="true"></span>
            <span class="nm-live-label">上传</span>
            <strong>{{ formatSpeed(liveUp) }}</strong>
          </div>
          <div>
            <span class="nm-legend nm-legend--down" aria-hidden="true"></span>
            <span class="nm-live-label">下载</span>
            <strong>{{ formatSpeed(liveDown) }}</strong>
          </div>
        </div>

        <div class="nm-plot">
          <svg
            v-if="liveChart.count >= 2"
            class="nm-svg"
            viewBox="0 0 600 150"
            preserveAspectRatio="none"
            role="img"
            :aria-label="`实时上下行速率曲线，${liveChart.count} 个采样点`"
          >
            <path class="nm-grid" d="M0 10H600 M0 75H600 M0 140H600" vector-effect="non-scaling-stroke" />
            <path
              v-for="line in liveChart.paths"
              :key="line.key"
              class="nm-line"
              :class="`nm-line--${line.key}`"
              :d="line.d"
              vector-effect="non-scaling-stroke"
            />
          </svg>
          <!-- 只有一个点时画不出线，与其显示一片空网格（像加载失败），不如说清楚在攒点 -->
          <p v-else class="nm-plot-empty">
            {{ liveChart.count === 1 ? '正在采集…（每 5 秒一个点）' : '等待采样…' }}
          </p>
        </div>

        <p class="nm-note">
          本次浏览观测，刷新即清空 · 峰值 {{ formatSpeed(livePeak) }} ·
          只有 xray 节点上报实时速率，面板节点不提供
        </p>
      </Card>
    </div>

    <!-- 节点卡片 -->
    <div class="nm-nodes-head">
      <h2 class="nm-panel-title">节点一览 <span class="nm-count">{{ nodes.length }}</span></h2>
      <span class="nm-updated">
        {{ lastSuccessAt ? `更新于 ${relativeTime(new Date(lastSuccessAt).toISOString(), now)}` : '尚未更新' }}
      </span>
    </div>

    <div class="nm-nodes">
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

<style scoped>
.nm {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}

.nm-banner {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-3) var(--space-4);
  background: var(--danger-muted);
  border: 1px solid var(--danger);
  border-radius: var(--radius-lg);
  font-size: var(--text-sm);
}

/* ── 汇总卡 ── */
.nm-summary {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: var(--space-4);
}
.nm-spark {
  width: 100%;
  height: 34px;
  display: block;
  margin-top: var(--space-1);
}

/* ── 地球 / 实时网络 ── */
.nm-observatory {
  display: grid;
  grid-template-columns: 1.13fr 1fr;
  gap: var(--space-4);
  align-items: start;
}
.nm-panel {
  min-width: 0;
}
.nm-panel-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
  margin-bottom: var(--space-4);
}
.nm-panel-title {
  margin: 0;
  font-size: var(--text-lg);
  font-weight: var(--font-semibold);
  /* 窄栏里标题会被右侧的按钮/下拉挤成两行，「率」字孤行很难看 */
  white-space: nowrap;
}
.nm-globe-row {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--space-4);
}
.nm-region-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  flex: 0 0 132px;
  max-height: 250px;
  overflow: auto;
}
.nm-region-list button {
  width: 100%;
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2);
  border: 1px solid transparent;
  border-radius: var(--radius-md);
  background: transparent;
  color: var(--text-secondary);
  font-size: var(--text-xs);
  text-align: left;
}
.nm-region-list button:hover {
  background: var(--bg-hover);
}
.nm-region-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.nm-region-list strong {
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
}
.nm-region-list small {
  color: var(--text-muted);
  font-weight: var(--font-regular);
}
.nm-led {
  flex-shrink: 0;
  width: 6px;
  height: 6px;
  border-radius: var(--radius-full);
  background: var(--text-muted);
}
.nm-led.is-lit {
  background: var(--success);
}
.nm-region-empty {
  font-size: var(--text-xs);
  color: var(--text-muted);
  padding: var(--space-2);
}
.nm-note {
  margin: var(--space-3) 0 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
  line-height: 1.6;
}

.nm-live {
  display: flex;
  gap: var(--space-8);
  padding: var(--space-2) 0 var(--space-4);
}
.nm-live > div {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}
.nm-live-label {
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.nm-live strong {
  font-size: var(--text-xl);
  font-weight: var(--font-semibold);
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
}
.nm-legend {
  display: inline-block;
  width: 7px;
  height: 7px;
  border-radius: var(--radius-full);
  margin-bottom: var(--space-1);
}
.nm-legend--up {
  background: var(--success);
}
.nm-legend--down {
  background: var(--accent);
}
.nm-plot {
  min-height: 150px;
}
.nm-svg {
  width: 100%;
  height: 150px;
  display: block;
  overflow: visible;
}
.nm-grid {
  stroke: var(--border-subtle);
  stroke-width: 1;
  stroke-dasharray: 3 5;
  fill: none;
}
.nm-line {
  fill: none;
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}
.nm-line--up {
  stroke: var(--success);
}
.nm-line--down {
  stroke: var(--accent);
}
.nm-line--cpu {
  stroke: var(--proto-vless);
}
.nm-line--mem {
  stroke: var(--accent);
}
.nm-plot-empty {
  margin: 0;
  padding: var(--space-8) 0;
  text-align: center;
  font-size: var(--text-sm);
  color: var(--text-muted);
}

/* ── 节点卡片 ── */
.nm-nodes-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}
.nm-count {
  font-size: var(--text-xs);
  font-weight: var(--font-medium);
  color: var(--text-muted);
  background: var(--bg-elevated);
  border-radius: var(--radius-md);
  padding: 1px 7px;
  margin-left: var(--space-1);
}
.nm-updated {
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.nm-nodes {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: var(--space-4);
}
.nm-node {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  min-width: 0;
}
.nm-node-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.nm-node-flag {
  font-size: var(--text-lg);
  line-height: 1;
  flex-shrink: 0;
}
.nm-node-title {
  flex: 1;
  min-width: 0;
}
.nm-node-title h3 {
  margin: 0;
  font-size: var(--text-sm);
  font-weight: var(--font-semibold);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.nm-node-title p {
  margin: 2px 0 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
  /* 副标题 + 右端 agent 版本同一行：左边可省略，右边不换行 */
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  min-width: 0;
}
.nm-node-sub {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}
.nm-meters {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.nm-meter {
  display: grid;
  grid-template-columns: 36px 1fr;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.nm-meter strong {
  justify-self: end;
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
  color: var(--text-primary);
}
.nm-bar {
  grid-column: 1 / -1;
  display: block;
  height: 4px;
  border-radius: var(--radius-full);
  background: var(--bg-elevated);
  overflow: hidden;
}
.nm-bar b {
  display: block;
  height: 100%;
  border-radius: var(--radius-full);
  background: var(--accent);
  transition: width var(--duration-slow) var(--ease-out);
}
.nm-meter.is-warn .nm-bar b {
  background: var(--warning);
}
.nm-meter.is-critical .nm-bar b {
  background: var(--danger);
}
.nm-meter.is-ok .nm-bar b {
  background: var(--success);
}
.nm-meter.is-unknown .nm-bar {
  background: transparent;
  border: 1px dashed var(--border-subtle);
}
.nm-node-foot {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-1) var(--space-3);
  padding-top: var(--space-2);
  border-top: 1px solid var(--border-subtle);
  font-size: var(--text-xs);
  color: var(--text-muted);
  font-variant-numeric: tabular-nums;
}
.nm-fresh {
  margin-left: auto;
}
/* agent 版本：默认低调，靠右；落后于面板时标黄，标题里写明「现在多少 / 面板多少」 */
.nm-agent {
  cursor: help;
  /* 推到副标题这一行的右端：左边地区那串可以省略，版本号不许被挤掉 */
  margin-left: auto;
  white-space: nowrap;
  font-variant-numeric: tabular-nums;
}
.nm-agent.is-outdated {
  color: var(--warning);
}
.nm-empty {
  grid-column: 1 / -1;
  text-align: center;
  color: var(--text-muted);
}

/* 侧栏占掉 ~240px，内容区比视口窄，两栏在 1100px 以下再塌成一栏
   （1280 断点会让 1280~1440 的常见桌面宽度提前塌掉，地球被拉成一整行、留白很大） */
@media (max-width: 1100px) {
  .nm-observatory {
    grid-template-columns: 1fr;
  }
  .nm-nodes {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (max-width: 900px) {
  .nm-summary {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .nm-globe-row {
    flex-direction: column;
    align-items: stretch;
  }
  .nm-region-list {
    flex: 1 1 auto;
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    max-height: none;
  }
}

@media (max-width: 640px) {
  .nm-summary,
  .nm-nodes {
    grid-template-columns: 1fr;
  }
  .nm-region-list {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .nm-live {
    gap: var(--space-6);
  }
}
</style>
