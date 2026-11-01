<script setup>
/**
 * Admin 系统监控：队列 worker 心跳 + 定时任务状态 + 异步任务（含失败重试）。
 * 操作日志已拆到独立页「操作审计」（分页 + 筛选、不轮询）。
 */
import { ref, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const loading = ref(false)
const tasks = ref({ events: [], recent: [] })
const worker = ref({ alive: false, last_seen: null, pending: 0, running: 0, failed_count: 0, recent_failed: [] })


/**
 * 后端时间字段统一为 ISO8601（UTC 带 Z），可直接交给 Date 解析（不要手工拼 'Z'）。
 * 空值 → 各自占位符；非法日期 → '—'，避免把字面量 "Invalid Date" 渲染到页面。
 */
const fmtTime = (v, fallback = '—') => {
  if (!v) return fallback
  const d = new Date(v)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

// 异步任务：实时列表 + 失败可重试（原 SystemStatus 没有这块，异步任务只在用户页露出）
const asyncTasks = ref([])
const retryingTaskId = ref(null)
const taskTypeLabels = { traffic_sync: '流量同步', node_init: '节点接入初始化', node_action: '节点操作' }
const taskStatusMeta = {
  pending: { label: '等待中', variant: 'info' },
  running: { label: '执行中', variant: 'warning' },
  succeeded: { label: '已完成', variant: 'success' },
  partial: { label: '部分完成', variant: 'warning' },
  failed: { label: '失败', variant: 'danger' },
}
const taskTypeLabel = (t) => taskTypeLabels[t] || t || '异步任务'
const taskStatus = (s) => taskStatusMeta[s] || { label: s || '未知', variant: 'info' }
const taskDone = (t) => (t.completed || 0) + (t.failed || 0)
const taskPercent = (t) => (!t.total ? (t.status === 'succeeded' ? 100 : 0) : Math.min(100, Math.round((taskDone(t) / t.total) * 100)))
const canRetryTask = (t) => t.status === 'failed' && (t.attempts || 0) < (t.max_attempts || 0)

// 定时任务说明：鼠标悬停任务名时弹出的小卡片。措辞与后端 internal/task/defaults.go 的
// 注释逐条对齐（那边是这些任务的唯一实现处），改任务时两边一起改。
//
// ⚠️ 时间一律写**北京时间**：cron 表达式是 UTC，但本表「下次运行」列走 fmtTime() →
// toLocaleString() 按浏览器本地时区渲染，用户看到的就是北京时间。说明里若写 UTC，
// 会出现「表里 13:20、说明写 05:20」这种自相矛盾（2026-11-02 踩过）。
const scheduleDesc = {
  'traffic-sync': {
    title: '流量同步',
    body: '每隔 5 分钟把各节点上的流量用量拉回面板记账。3x-ui 节点走面板接口轮询；裸内核（xray）节点由节点 agent 主动推送，这条任务会跳过它们。',
  },
  'ban-check': {
    title: '到期 / 超量扫描',
    body: '每隔 5 分钟检查所有启用用户：套餐到期或流量用超就关闭节点的使用权限。只关流量、不封账号，用户仍可登录续费。',
  },
  'health-check': {
    title: '节点健康检查',
    body: '每隔 5 分钟探测所有已启用节点，刷新在线状态与延迟，节点掉线会触发通知。',
  },
  'monthly-reset-traffic': {
    title: '周期套餐月流量重置',
    body: '每天 08:00（北京时间）检查一遍，把到了重置时间点的周期套餐用户当月已用流量清零。',
  },
  'invite-code-refresh': {
    title: '邀请码刷新',
    body: '每天 08:10（北京时间）刷新已到期的邀请码，旧码立即失效、只认最新一码，防止邀请码被长期外传复用。',
  },
  'db-backup': {
    title: '数据库备份',
    body: '每天 11:30（北京时间）导出一次数据库，并按保留份数自动清理旧备份。',
  },
  'monitor-rollup': {
    title: '监控数据汇总',
    body: '每天 12:30（北京时间）把原始的监控采样点汇总成按天的统计，供面板图表使用；原始采样随后按保留策略清理。',
  },
  'expire-pending-orders': {
    title: '待支付订单过期',
    body: '每小时把超过 1 小时仍未支付的订单标记为过期，避免死单一直占着用户的下单入口。',
  },
  'proxy-maintain': {
    title: '代理商城维护',
    body: '回采 upstream 代理商的用量、标记到期权益、停用到期的上游子账号。',
  },
  'async-task-timeout': {
    title: '异步任务超时清扫',
    body: '每隔 5 分钟清理长时间卡在「执行中」的异步任务，把它们标成超时失败，避免任务列表永远转圈。',
  },
  'mail-notify-scan': {
    title: '邮件自动通知',
    body: '扫描需要发通知的事件（到期提醒、流量告警等）并发出邮件。四个通知开关全关时整条任务直接跳过。',
  },
  'mail-schedule-scan': {
    title: '定时邮件扫描',
    body: '检查有没到发送时间的定时邮件/公告邮件并发出；没配置定时邮件时这条任务不会真正执行。',
  },
  'third-party-fetch': {
    title: '第三方订阅拉取',
    body: '每 2 小时拉一次已启用的第三方订阅内容。总开关关闭、或没有启用中的订阅时跳过。',
  },
  'xray-warp-rotate': {
    title: 'WARP 出口换 IP',
    body: '每隔 5 分钟检查并轮换 WARP 出站 IP。没有配置 WARP 出站时跳过。',
  },
  'xray-refresh-outbound-subs': {
    title: '出站订阅自动刷新',
    body: '每隔 5 分钟把「出站订阅」里的节点列表重新拉一遍，内容有变化就更新到节点并提升该节点的配置版本，让节点重拉配置生效。表里这条显示的上次运行时间很久，是因为「没有启用中的出站订阅时整条任务会跳过、也不写运行记录」，不是坏了。',
  },
  'geodata-update': {
    title: 'GeoIP / GeoSite 更新',
    body: '每周一 12:45（北京时间）检查 geoip.dat / geosite.dat 有没新版本，有就下载并分发到所有已启用的裸内核节点。',
  },
  'users-reconcile-enable': {
    title: '用户与节点对账',
    body: '每隔 5 分钟把面板里的「谁该在哪些入站」与节点上的实际状态逐条比对，补齐缺失的、报告漂移（只报告不擅自改）。裸内核节点靠 agent 的 client_list 指令读回实际状态。',
  },
  'reality-audit': {
    title: 'REALITY 入站巡检',
    body: '每天 12:10（北京时间）巡检 REALITY 入站的配置是否合法（目标站证书 SAN、SNI 等），发现问题只记录不自动改。',
  },
  'xray-certificates-sync': {
    title: '本机证书目录同步',
    body: '每 6 小时把面板机上 acme.sh 续签好的证书搬进「TLS 证书库」。证书内容真变了才会提升引用它的节点配置版本；没变只刷库，不会让节点白重载。',
  },
  'xray-certificates-renew': {
    title: '远程证书自动续签',
    body: '每天 13:20（北京时间）检查证书库，到期前 21 天把证书交给「这个域名实际落地的节点」重新签发（节点自己做 ACME），回执的证书写回库里同一行并提升节点配置版本。面板机 acme.sh 自己管的证书不归它管。',
  },
}

// scheduleDescFor 取某条任务的说明；没登记的返回 null（不弹窗、不报错）。
const scheduleDescFor = (cmd) => scheduleDesc[cmd] || null

// hoverTip 控制悬浮说明：进入任务名时记录任务与鼠标位置，离开即清空。
const hoverTip = ref(null)
const showScheduleTip = (e, cmd) => {
  const d = scheduleDescFor(cmd)
  if (!d) return
  const r = e.currentTarget.getBoundingClientRect()
  hoverTip.value = { ...d, cmd, x: r.left, y: r.bottom + 8 }
}
const hideScheduleTip = () => { hoverTip.value = null }


const loadAsyncTasks = async () => {
  try {
    asyncTasks.value = (await adminApi.listAsyncTasks()) || []
  } catch (e) {
    asyncTasks.value = []
  }
}

const retryTask = async (t) => {
  if (!canRetryTask(t) || retryingTaskId.value) return
  retryingTaskId.value = t.id
  try {
    await adminApi.retryAsyncTask(t.id)
    toast.success('重试任务已提交')
    await loadAsyncTasks()
  } catch (e) {
    toast.error(e.message || '重试失败')
  } finally {
    retryingTaskId.value = null
  }
}

// 定时任务自动刷新（下次运行时间会变化）
let timer = null

const load = async () => {
  loading.value = true
  try {
    const data = await adminApi.getSystemStatus()
    tasks.value = data.tasks || { events: [], recent: [] }
    worker.value = data.worker || worker.value
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
  await loadAsyncTasks()
}

onMounted(() => {
  load()
  timer = setInterval(load, 30000)
})
onActivated(load)
</script>

<template>
  <div class="sys">
    <div class="sys-head">
      <h1 class="sys-title">系统监控</h1>
      <Button variant="secondary" size="sm" :loading="loading" @click="load">
        <Icon name="refresh" :size="14" /> 刷新
      </Button>
    </div>

    <!-- Worker 状态 -->
    <Card padding="false">
      <div class="sys-card-head">
        <h2 class="sys-card-title">队列 Worker</h2>
        <Badge :variant="worker.alive ? 'success' : 'danger'" dot>
          {{ worker.alive ? '在线' : '离线' }}
        </Badge>
      </div>
      <div class="sys-metrics">
        <div class="sys-metric">
          <span class="sys-metric-label">最近心跳</span>
          <span class="sys-metric-value">{{ fmtTime(worker.last_seen, '从未') }}</span>
        </div>
        <div class="sys-metric">
          <span class="sys-metric-label">排队中</span>
          <span class="sys-metric-value">{{ worker.pending }}</span>
        </div>
        <div class="sys-metric">
          <span class="sys-metric-label">运行中</span>
          <span class="sys-metric-value">{{ worker.running }}</span>
        </div>
        <div class="sys-metric">
          <span class="sys-metric-label">失败累计</span>
          <span class="sys-metric-value" :class="{ 'sys-danger': worker.failed_count > 0 }">{{ worker.failed_count }}</span>
        </div>
      </div>
      <div v-if="worker.recent_failed?.length" class="sys-failed">
        <div class="sys-failed-title">最近失败任务</div>
        <div v-for="(j, i) in worker.recent_failed" :key="i" class="sys-failed-item mono">
          <Badge variant="danger" dot>failed</Badge>
          <span class="sys-failed-name">{{ j.name || '（payload 无任务名）' }}</span>
          <!-- attempts 为 null：failed_jobs 与 payload 都没有该字段，后端不再伪造 0 -->
          <span class="sys-failed-meta">queue={{ j.queue }}<template v-if="j.attempts != null"> · attempts={{ j.attempts }}</template> · {{ fmtTime(j.failed_at, '') }}</span>
          <span v-if="j.error_summary" class="sys-failed-meta">{{ j.error_summary }}</span>
        </div>
      </div>
    </Card>

    <!-- 定时任务 -->
    <Card padding="false">
      <div class="sys-card-head">
        <h2 class="sys-card-title">定时任务</h2>
      </div>
      <div class="sys-table-wrap">
        <table class="sys-table">
          <thead>
            <tr>
              <th>任务</th>
              <th>Cron</th>
              <th>下次运行</th>
              <th>上次运行</th>
              <th>状态</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in tasks.events" :key="t.command">
              <td class="mono">
                <span
                  class="sys-task-name"
                  :class="{ 'is-help': scheduleDescFor(t.command) }"
                  @mouseenter="showScheduleTip($event, t.command)"
                  @mouseleave="hideScheduleTip"
                >{{ t.command }}</span>
              </td>
              <td class="mono">{{ t.expression }}</td>
              <td>{{ fmtTime(t.next_run, '—') }}</td>
              <td>{{ fmtTime(t.last_run, '从未') }}</td>
              <td>
                <template v-if="t.last_status">
                  <Badge :variant="t.last_status === 'finished' ? 'success' : 'danger'" dot>
                    {{ t.last_status === 'finished' ? '正常' : '失败' }}
                  </Badge>
                  <span v-if="t.last_error" class="sys-err-text">{{ t.last_error }}</span>
                </template>
                <span v-else class="sys-muted">尚未记录</span>
              </td>
            </tr>
            <tr v-if="!tasks.events?.length">
              <td colspan="5" class="sys-empty">暂无定时任务</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="tasks.recent?.length" class="sys-recent">
        <div class="sys-recent-title">最近运行记录</div>
        <div v-for="(r, i) in tasks.recent" :key="i" class="sys-recent-item mono">
          <Badge :variant="r.status === 'finished' ? 'success' : 'danger'" dot>{{ r.status === 'finished' ? '正常' : '失败' }}</Badge>
          <span class="sys-recent-cmd">{{ r.command }}</span>
          <span class="sys-recent-meta">{{ fmtTime(r.ran_at, '') }} · {{ r.duration_ms }}ms</span>
          <span v-if="r.error" class="sys-err-text">{{ r.error }}</span>
        </div>
      </div>
    </Card>

    <!-- 定时任务说明：跟随鼠标任务名，移开即消失。用 Teleport 挂到 body，
         避免被表格容器的 overflow-x 裁剪掉。 -->
    <Teleport to="body">
      <div
        v-if="hoverTip"
        class="sys-task-tip"
        :style="{ left: hoverTip.x + 'px', top: hoverTip.y + 'px' }"
        role="tooltip"
      >
        <div class="sys-task-tip-title">{{ hoverTip.title }}</div>
        <div class="sys-task-tip-body">{{ hoverTip.body }}</div>
      </div>
    </Teleport>

    <!-- 异步任务（失败可重试；与定时任务不同，这是后台任务体系的实时列表） -->
    <Card padding="false">
      <div class="sys-card-head">
        <h2 class="sys-card-title">异步任务</h2>
        <span class="sys-muted">{{ asyncTasks.length }} 条</span>
      </div>
      <div class="sys-table-wrap">
        <table class="sys-table">
          <thead>
            <tr>
              <th>类型</th>
              <th>状态</th>
              <th>进度</th>
              <th>创建</th>
              <th>错误</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in asyncTasks" :key="t.id">
              <td class="mono">{{ taskTypeLabel(t.type) }}</td>
              <td><Badge :variant="taskStatus(t.status).variant" dot>{{ taskStatus(t.status).label }}</Badge></td>
              <td>
                <div class="sys-progress"><span :style="{ width: taskPercent(t) + '%' }"></span></div>
                <span class="sys-muted">
                  {{ taskDone(t) }} / {{ t.total || 0 }}
                  <template v-if="t.failed"> · 失败 {{ t.failed }}</template>
                </span>
              </td>
              <td class="sys-muted">{{ fmtTime(t.created_at, '—') }}</td>
              <td class="sys-err-cell">
                <span v-if="t.error" class="sys-err-text">{{ t.error }}</span>
                <span v-else class="sys-muted">—</span>
              </td>
              <td>
                <Button
                  v-if="canRetryTask(t)"
                  variant="secondary"
                  size="sm"
                  :loading="retryingTaskId === t.id"
                  @click="retryTask(t)"
                >重试</Button>
                <span v-else class="sys-muted">—</span>
              </td>
            </tr>
            <tr v-if="!asyncTasks.length">
              <td colspan="6" class="sys-empty">没有异步任务</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>
  </div>
</template>

<style scoped>
.sys { display: flex; flex-direction: column; gap: var(--space-4); }
.sys-head {
  display: flex; align-items: center; justify-content: space-between; gap: var(--space-3);
}
.sys-title { margin: 0; font-size: var(--text-xl); font-weight: var(--font-bold); }
.sys-card-head {
  display: flex; align-items: center; justify-content: space-between;
  padding: var(--space-4); border-bottom: 1px solid var(--border-subtle);
}
.sys-card-title { margin: 0; font-size: var(--text-base); font-weight: var(--font-semibold); }
.sys-metrics {
  display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
  gap: var(--space-3); padding: var(--space-4);
}
.sys-metric { display: flex; flex-direction: column; gap: var(--space-1); }
.sys-metric-label { font-size: var(--text-xs); color: var(--text-muted); }
.sys-metric-value { font-size: var(--text-base); font-weight: var(--font-medium); }
.sys-danger { color: var(--color-danger, #dc2626); }
.sys-failed {
  padding: 0 var(--space-4) var(--space-4); display: flex; flex-direction: column; gap: var(--space-2);
}
.sys-failed-title { font-size: var(--text-xs); color: var(--text-muted); font-weight: var(--font-medium); }
.sys-failed-item {
  display: flex; align-items: center; gap: var(--space-2); font-size: var(--text-sm); flex-wrap: wrap;
}
.sys-failed-name { flex: 1; min-width: 120px; }
.sys-failed-meta { font-size: var(--text-xs); color: var(--text-muted); }
.sys-table-wrap { overflow-x: auto; }
.sys-table {
  width: 100%; border-collapse: collapse; font-size: var(--text-sm);
}
.sys-table th, .sys-table td {
  padding: var(--space-3) var(--space-4); text-align: left;
  border-bottom: 1px solid var(--border-subtle); white-space: nowrap;
}
.sys-table th {
  color: var(--text-muted); font-weight: var(--font-medium); font-size: var(--text-xs);
}
.sys-table tbody tr:hover { background: var(--bg-hover); }

/* 任务名：只做悬浮提示，不动表格外观（不加下划线/不变色），鼠标移上去指针提示可查 */
.sys-task-name.is-help {
  cursor: help;
}

/* 悬浮说明卡片 */
.sys-task-tip {
  position: fixed;
  z-index: 1200;
  max-width: 340px;
  padding: var(--space-3);
  background: var(--bg-elevated);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-lg, 0 8px 24px rgba(0, 0, 0, .28));
  pointer-events: none; /* 鼠标穿过，避免抖动 */
}
.sys-task-tip-title {
  font-size: var(--text-sm);
  font-weight: var(--font-semibold);
  color: var(--text-primary);
  margin-bottom: var(--space-1);
}
.sys-task-tip-body {
  font-size: var(--text-xs);
  line-height: 1.6;
  color: var(--text-secondary);
}
.sys-table td.sys-err-cell { max-width: 280px; white-space: normal; }
.sys-empty { text-align: center; color: var(--text-muted); padding: var(--space-6); }
.sys-muted { color: var(--text-muted); }
.sys-path { color: var(--text-muted); font-size: var(--text-xs); }
.sys-err-text {
  display: inline-block; color: var(--color-danger, #dc2626); font-size: var(--text-xs);
  word-break: break-all;
}
.sys-recent {
  padding: 0 var(--space-4) var(--space-4); display: flex; flex-direction: column; gap: var(--space-2);
}
.sys-recent-title { font-size: var(--text-xs); color: var(--text-muted); font-weight: var(--font-medium); }
.sys-recent-item { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; font-size: var(--text-sm); }
.sys-recent-cmd { flex: 1; min-width: 100px; }
.sys-recent-meta { font-size: var(--text-xs); color: var(--text-muted); }
.sys-progress {
  width: 90px; height: 4px; border-radius: 2px; background: var(--bg-hover);
  overflow: hidden; margin-bottom: 4px;
}
.sys-progress span { display: block; height: 100%; background: var(--accent); }
.mono { font-family: var(--font-mono); font-size: var(--text-sm); }
</style>
