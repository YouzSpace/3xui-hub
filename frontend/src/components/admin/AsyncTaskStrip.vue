<script setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref } from 'vue'
import Badge from '@/components/ui/Badge.vue'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const tasks = ref([])
const retryingId = ref(null)
let timer = null
let loading = false

const typeLabels = {
  traffic_sync: '流量同步',
  node_init: '节点接入初始化',
  node_action: '节点操作',
}

const statusMeta = {
  pending: { label: '等待中', variant: 'info' },
  running: { label: '执行中', variant: 'warning' },
  succeeded: { label: '已完成', variant: 'success' },
  partial: { label: '部分完成', variant: 'warning' },
  failed: { label: '失败', variant: 'danger' },
}

const recentTasks = computed(() => tasks.value.slice(0, 2))

const taskTypeLabel = (type) => typeLabels[type] || type || '异步任务'
const taskStatus = (status) => statusMeta[status] || { label: status || '未知', variant: 'info' }
const progressDone = (task) => (task.completed || 0) + (task.failed || 0)
const progressText = (task) => `${progressDone(task)} / ${task.total || 0}`
const progressPercent = (task) => {
  if (!task.total) return task.status === 'succeeded' ? 100 : 0
  return Math.min(100, Math.round((progressDone(task) / task.total) * 100))
}
const canRetry = (task) => task.status === 'failed' && (task.attempts || 0) < (task.max_attempts || 0)

// 只有存在进行中任务（pending/running）时才保持轮询，全部终态后自动停止
const hasActiveTasks = computed(() => tasks.value.some((t) => t.status === 'pending' || t.status === 'running'))

const emit = defineEmits(['task-finished'])

const TERMINAL = ['succeeded', 'partial', 'failed']
// 盯守的 task：父组件提交同步后传入，轮询直到该任务终态再刷新，
// 消除"任务完成太快、首次轮询已是终态而漏报 task-finished"的竞态
const watchedTaskId = ref(null)
const emittedTaskIds = new Set()

const loadTasks = async () => {
  if (loading) return
  loading = true
  try {
    const data = await adminApi.listAsyncTasks()
    const fresh = Array.isArray(data) ? data : []
    // 1) 快照跳变检测：上一轮进行中、本轮变终态
    const wasActive = tasks.value.filter((t) => t.status === 'pending' || t.status === 'running')
    tasks.value = fresh
    for (const prev of wasActive) {
      const now = fresh.find((f) => f.id === prev.id)
      if (now && TERMINAL.includes(now.status) && !emittedTaskIds.has(now.id)) {
        emittedTaskIds.add(now.id)
        emit('task-finished', now)
      }
    }
    // 2) 盯守任务检测：父组件提交的 task 一旦进入终态就 emit（即使首次看到就是终态）
    if (watchedTaskId.value !== null) {
      const t = fresh.find((f) => f.id === watchedTaskId.value)
      if (t && TERMINAL.includes(t.status) && !emittedTaskIds.has(t.id)) {
        emittedTaskIds.add(t.id)
        emit('task-finished', t)
        watchedTaskId.value = null
      }
    }
  } catch (e) {
    // Polling failures stay quiet; the next interval retries automatically.
  } finally {
    loading = false
  }
  hasActiveTasks.value || watchedTaskId.value !== null ? startPolling() : stopPolling()
}

const startPolling = () => {
  // 有活跃任务或存在盯守任务时都要轮询（盯守任务可能还没出现在列表里）
  if (timer || (!hasActiveTasks.value && watchedTaskId.value === null)) return
  timer = window.setInterval(loadTasks, 5000)
}

const stopPolling = () => {
  if (!timer) return
  window.clearInterval(timer)
  timer = null
}

const retryTask = async (task) => {
  if (!canRetry(task) || retryingId.value) return
  retryingId.value = task.id
  try {
    await adminApi.retryAsyncTask(task.id)
    toast.success('重试任务已提交')
    await loadTasks()
  } catch (e) {
    toast.error(e.message || '重试失败')
  } finally {
    retryingId.value = null
  }
}

onMounted(loadTasks)
onActivated(loadTasks)
onDeactivated(stopPolling)
onUnmounted(stopPolling)

// 供父组件在创建任务后主动刷新（例如提交同步后）
// watchTask(taskId)：盯守指定任务，到终态自动 emit('task-finished') 触发父组件刷新
const watchTask = (taskId) => {
  if (taskId) {
    watchedTaskId.value = taskId
    loadTasks() // 立即拉一次；若任务已是终态当场 emit
  }
}
defineExpose({ refresh: loadTasks, watchTask })
</script>

<template>
  <section v-if="recentTasks.length" class="task-strip" aria-label="最近异步任务">
    <div class="task-strip-title">
      <Icon name="activity" :size="16" />
      <span>最近任务</span>
    </div>
    <div class="task-list">
      <div v-for="task in recentTasks" :key="task.id" class="task-row">
        <div class="task-main">
          <span class="task-type">{{ taskTypeLabel(task.type) }}</span>
          <Badge :variant="taskStatus(task.status).variant" dot>{{ taskStatus(task.status).label }}</Badge>
          <span class="task-progress">{{ progressText(task) }}</span>
        </div>
        <div class="task-progress-bar" :aria-label="`进度 ${progressText(task)}`">
          <span :style="{ width: `${progressPercent(task)}%` }"></span>
        </div>
        <span v-if="task.error" class="task-error" :title="task.error">{{ task.error }}</span>
        <Button
          v-if="canRetry(task)"
          variant="secondary"
          size="sm"
          :loading="retryingId === task.id"
          @click="retryTask(task)"
        >
          <Icon name="refresh" :size="16" /> 重试
        </Button>
      </div>
    </div>
  </section>
</template>

<style scoped>
.task-strip {
  display: flex;
  align-items: flex-start;
  gap: var(--space-4);
  margin-bottom: var(--space-4);
  padding: var(--space-3) var(--space-4);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--bg-hover);
}
.task-strip-title {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  min-width: 82px;
  padding-top: 4px;
  color: var(--text-secondary);
  font-size: var(--text-sm);
  font-weight: var(--font-semibold);
}
.task-list {
  display: flex;
  flex: 1;
  flex-direction: column;
  min-width: 0;
}
.task-row {
  display: grid;
  grid-template-columns: minmax(210px, auto) minmax(80px, 160px) minmax(120px, 1fr) auto;
  align-items: center;
  gap: var(--space-3);
  min-height: 34px;
}
.task-row + .task-row {
  border-top: 1px solid var(--border-subtle);
}
.task-main {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  min-width: 0;
}
.task-type {
  overflow: hidden;
  color: var(--text-primary);
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  text-overflow: ellipsis;
  white-space: nowrap;
}
.task-progress {
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  white-space: nowrap;
}
.task-progress-bar {
  height: 4px;
  overflow: hidden;
  border-radius: var(--radius-full);
  background: var(--border-subtle);
}
.task-progress-bar span {
  display: block;
  height: 100%;
  border-radius: inherit;
  background: var(--accent);
  transition: width var(--duration-normal) var(--ease-out);
}
.task-error {
  overflow: hidden;
  color: var(--danger);
  font-size: var(--text-xs);
  text-overflow: ellipsis;
  white-space: nowrap;
}
@media (max-width: 900px) {
  .task-strip { flex-direction: column; gap: var(--space-2); }
  .task-strip-title { padding-top: 0; }
  .task-list { width: 100%; }
  .task-row { grid-template-columns: 1fr minmax(70px, 120px) auto; padding: var(--space-2) 0; }
  .task-error { grid-column: 1 / -1; }
}
@media (max-width: 560px) {
  .task-row { grid-template-columns: 1fr auto; }
  .task-progress-bar { grid-column: 1 / -1; }
}
</style>