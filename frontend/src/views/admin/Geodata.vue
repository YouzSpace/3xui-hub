<script setup>
/**
 * Geo 数据：geoip.dat / geosite.dat 的更新与分发（issue #40）。
 *
 * 面板每周一 04:45 自动从上游检查更新并分发到节点；本页可手动检查更新、
 * 补推节点。节点侧 sha256 相同则跳过（不重载内核），失败保留旧文件。
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
const updating = ref(false)
const dispatching = ref(false)
const files = ref([])
const nodes = ref([])

const fmtSize = (b) => {
  if (!b) return '—'
  if (b >= 1024 * 1024) return (b / 1024 / 1024).toFixed(1) + ' MB'
  if (b >= 1024) return (b / 1024).toFixed(1) + ' KB'
  return b + ' B'
}
const fmtTime = (s) => {
  if (!s) return '—'
  const d = new Date(s)
  return isNaN(d.getTime()) ? s : d.toLocaleString()
}
const shortSha = (s) => (s ? s.slice(0, 16) + '…' : '—')

const statusVariant = (s) =>
  ({ success: 'success', failed: 'warning', pending: 'info', acknowledged: 'info' })[s] || 'info'
const statusLabel = (s) =>
  ({ success: '完成', failed: '失败', pending: '待执行', acknowledged: '执行中' })[s] || '未同步'

const load = async () => {
  loading.value = true
  try {
    const res = await adminApi.getGeodata()
    files.value = res?.files || []
    nodes.value = res?.nodes || []
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const update = async () => {
  updating.value = true
  try {
    const res = await adminApi.updateGeodata()
    const results = res?.results || {}
    const updatedCount = Object.values(results).filter((r) => r.status === 'updated').length
    const failed = Object.entries(results).filter(([, r]) => r.status === 'failed')
    if (failed.length) {
      toast.error(failed.map(([n, r]) => `${n}: ${r.message || '更新失败'}`).join('；'), 8000)
    } else {
      toast.success(updatedCount ? `已更新 ${updatedCount} 个文件，可分发到节点` : '已是最新，无需更新')
    }
    await load()
  } catch (e) {
    toast.error(e.message || '更新失败')
  } finally {
    updating.value = false
  }
}

const dispatch = async () => {
  if (!confirm('把面板当前 geo 文件分发到所有启用的 xray 节点？节点校验 sha256，有变化才重载内核。')) return
  dispatching.value = true
  try {
    const res = await adminApi.dispatchGeodata()
    toast.success(`已下发 ${res?.queued ?? 0} 个节点${res?.skipped ? `（跳过 ${res.skipped} 个已有在途任务的节点）` : ''}`)
    await load()
  } catch (e) {
    toast.error(e.message || '分发失败')
  } finally {
    dispatching.value = false
  }
}

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="gd">
    <div class="gd-head">
      <div>
        <h1 class="gd-title">Geo 数据</h1>
        <p class="gd-desc">
          xray 的 geoip.dat / geosite.dat 规则文件：面板每周一凌晨自动从上游检查更新并分发到节点，
          也可以在这里手动更新、补推节点。节点侧校验 sha256，相同则跳过（不重载内核），失败保留旧文件。
        </p>
      </div>
      <div class="gd-actions">
        <Button variant="secondary" size="sm" :loading="loading" @click="load">
          <Icon name="refresh" :size="14" /> 刷新
        </Button>
        <Button variant="secondary" size="sm" :loading="updating" :disabled="updating" @click="update">
          <Icon name="globe" :size="14" /> {{ updating ? '检查中…' : '检查并更新' }}
        </Button>
        <Button variant="primary" size="sm" :loading="dispatching" :disabled="dispatching" @click="dispatch">
          <Icon name="server" :size="14" /> 分发到节点
        </Button>
      </div>
    </div>

    <Card padding="false">
      <div class="gd-table-wrap">
        <table class="gd-table">
          <thead>
            <tr><th>文件</th><th>大小</th><th>更新时间</th><th>SHA256</th><th>状态</th></tr>
          </thead>
          <tbody>
            <tr v-for="f in files" :key="f.name">
              <td class="mono">{{ f.name }}</td>
              <td>{{ fmtSize(f.size) }}</td>
              <td>{{ fmtTime(f.updated_at) }}</td>
              <td class="mono gd-dim" :title="f.sha256 || ''">{{ shortSha(f.sha256) }}</td>
              <td>
                <Badge :variant="f.exists ? 'success' : 'warning'" dot>{{ f.exists ? '已就绪' : '未下载' }}</Badge>
              </td>
            </tr>
            <tr v-if="!loading && files.length === 0">
              <td colspan="5" class="gd-empty">暂无数据。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Card padding="false">
      <div class="gd-sub">
        <h2>节点同步状态</h2>
        <p class="gd-desc">最近一次分发的执行结果；节点离线时排队等待，上线后自动执行。</p>
      </div>
      <div class="gd-table-wrap">
        <table class="gd-table">
          <thead>
            <tr><th>节点</th><th>状态</th><th>结果</th><th>时间</th></tr>
          </thead>
          <tbody>
            <tr v-for="n in nodes" :key="n.node_id">
              <td>
                {{ n.node_name }}
                <span v-if="!n.enabled" class="gd-dim">（已停用）</span>
              </td>
              <td><Badge :variant="statusVariant(n.status)" dot>{{ statusLabel(n.status) }}</Badge></td>
              <td class="gd-dim gd-result">{{ n.result || '—' }}</td>
              <td class="gd-dim">{{ fmtTime(n.finished_at) }}</td>
            </tr>
            <tr v-if="!loading && nodes.length === 0">
              <td colspan="4" class="gd-empty">还没有 xray 节点。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>
  </div>
</template>

<style scoped>
.gd {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}
.gd-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-4);
  flex-wrap: wrap;
}
.gd-title {
  margin: 0 0 var(--space-1);
  font-size: var(--text-xl);
}
.gd-desc {
  margin: 0;
  color: var(--text-muted);
  font-size: var(--text-sm);
  max-width: 760px;
  line-height: 1.6;
}
.gd-actions {
  display: flex;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.gd-sub {
  padding: var(--space-4) var(--space-4) 0;
}
.gd-sub h2 {
  margin: 0 0 var(--space-1);
  font-size: var(--text-base);
  color: var(--text-primary);
}
.gd-table-wrap {
  overflow-x: auto;
}
.gd-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.gd-table th,
.gd-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
.gd-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.gd-dim {
  color: var(--text-muted);
  font-size: var(--text-xs);
}
.gd-result {
  max-width: 420px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.gd-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
</style>
