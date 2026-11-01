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
              <td class="mono">{{ t.command }}</td>
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
