<script setup>
/**
 * Admin 静态 IP 出口分类：一个分类 = 一组前置节点 + 一个 webshare 账号。
 *
 * 口径：
 *  - 套餐选「静态 IP」后绑一个分类；用户连该分类里的前置节点时，出站自动走住宅 IP。
 *  - 列表接口已返回 node_ids / node_count / provider_name，编辑直接用列表数据回填，不再拉详情。
 *  - 删除时后端若因「还有套餐绑着」拒绝，原样展示后端 msg（不自己编文案）。
 *  - 后端接口未合入时 404：页面降级为空列表 + 提示，不崩、不编数据。
 */
import { ref, computed, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const loading = ref(false)
const profiles = ref([])
const loadError = ref('')
// webshare 账号（provider）与节点全量，弹窗下拉/多选都从这里取
const providers = ref([])
const nodes = ref([])

const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : null)

/* ── 展示口径 ─────────────────────────────────────── */

const PROVIDER_STATUS = {
  success: { text: '正常', variant: 'success' },
  failed: { text: '连接失败', variant: 'danger' },
  pending: { text: '未测试', variant: 'warning' },
}

const providerStatus = (p) => PROVIDER_STATUS[p?.last_status] || { text: '未测试', variant: 'warning' }

/** 列表里的账号名：优先后端算好的 provider_name，缺了就按 provider_id 现查（账号被删时为「—」） */
const providerName = (p) => {
  if (p?.provider_name) return p.provider_name
  const hit = providers.value.find((x) => x.id === p?.provider_id)
  return hit?.name || '—'
}

const providerOf = (p) => providers.value.find((x) => x.id === p?.provider_id) || null

const nodeCount = (p) => {
  const n = num(p?.node_count)
  if (n != null) return n
  return Array.isArray(p?.node_ids) ? p.node_ids.length : 0
}

const nodeOf = (id) => nodes.value.find((n) => n.id === id) || null
const nodeName = (id) => nodeOf(id)?.name || `#${id}`

const statusVariant = (s) => ({ online: 'success', offline: 'danger', maintenance: 'warning' }[s] || 'info')

const trafficText = (p) => `${num(p?.traffic_gb) ?? 0} GB`
const durationText = (p) => `${num(p?.duration_days) ?? 0} 天`

/* ── 加载 ─────────────────────────────────────────── */

const loadProviders = async () => {
  try {
    const data = await adminApi.listWebshareProviders()
    providers.value = (data?.items ?? []).filter((p) => p.driver === 'webshare' || p.driver == null)
  } catch {
    // 账号接口挂了只让下拉空着，不连累分类列表
    providers.value = []
  }
}

const loadNodes = async () => {
  try {
    const data = await adminApi.listNodes({ all: 1 })
    nodes.value = Array.isArray(data) ? data : (data?.items ?? [])
  } catch {
    nodes.value = []
  }
}

const load = async () => {
  if (loading.value) return
  loading.value = true
  loadError.value = ''
  try {
    const data = await adminApi.listExitProfiles()
    // 后端返回数组（对齐 /regions）；也兼容包一层的形态
    profiles.value = Array.isArray(data) ? data : (data?.items ?? data?.profiles ?? [])
    await Promise.all([loadProviders(), loadNodes()])
  } catch (e) {
    profiles.value = []
    loadError.value = e?.message || '加载失败'
  } finally {
    loading.value = false
  }
}

/* ── 新建 / 编辑 ───────────────────────────────────── */

const formOpen = ref(false)
const saving = ref(false)

const emptyForm = () => ({
  id: null,
  name: '',
  provider_id: null,
  traffic_gb: 5,
  duration_days: 30,
  max_thread_count: 50,
  socks_host: 'p.webshare.io',
  socks_port: 1080,
  node_ids: [],
  enabled: true,
})

const form = ref(emptyForm())

const providerOptions = computed(() =>
  providers.value.map((p) => ({
    value: p.id,
    label: `${p.name}（${p.enabled === false ? '已停用' : providerStatus(p).text}）`,
  })),
)

const nodeOptions = computed(() => nodes.value)

const openCreate = () => {
  form.value = emptyForm()
  // socks 入口默认值跟着已选账号走（webshare 统一入口，账号不同也只换入口地址时手动改）
  formOpen.value = true
  loadProviders()
  loadNodes()
}

const openEdit = (p) => {
  form.value = {
    id: p.id,
    name: p.name || '',
    provider_id: p.provider_id ?? null,
    traffic_gb: num(p.traffic_gb) ?? 0,
    duration_days: num(p.duration_days) ?? 30,
    max_thread_count: num(p.max_thread_count) ?? 0,
    socks_host: p.socks_host || 'p.webshare.io',
    socks_port: num(p.socks_port) ?? 1080,
    node_ids: Array.isArray(p.node_ids) ? [...p.node_ids] : [],
    enabled: p.enabled !== false,
  }
  formOpen.value = true
  loadProviders()
  loadNodes()
}

const save = async () => {
  if (saving.value) return
  const name = String(form.value.name || '').trim()
  if (!name) {
    toast.error('分类名不能为空')
    return
  }
  if (form.value.provider_id == null) {
    toast.error('请选择 webshare 账号')
    return
  }
  const trafficGb = Number(form.value.traffic_gb)
  if (!Number.isFinite(trafficGb) || trafficGb < 0) {
    toast.error('每个用户流量需为不小于 0 的数字')
    return
  }
  const days = Number(form.value.duration_days)
  if (!Number.isInteger(days) || days <= 0) {
    toast.error('有效期天数需为正整数')
    return
  }
  const threads = Number(form.value.max_thread_count)
  if (!Number.isInteger(threads) || threads < 0) {
    toast.error('并发数需为不小于 0 的整数')
    return
  }
  const port = Number(form.value.socks_port)
  if (!Number.isInteger(port) || port <= 0 || port > 65535) {
    toast.error('端口需为 1-65535 的整数')
    return
  }

  const payload = {
    name,
    provider_id: Number(form.value.provider_id),
    traffic_gb: trafficGb,
    duration_days: days,
    max_thread_count: threads,
    socks_host: String(form.value.socks_host || '').trim(),
    socks_port: port,
    node_ids: [...form.value.node_ids],
    enabled: !!form.value.enabled,
  }

  saving.value = true
  try {
    if (form.value.id) await adminApi.updateExitProfile(form.value.id, payload)
    else await adminApi.createExitProfile(payload)
    toast.success(form.value.id ? '已保存' : '已新建出口分类')
    formOpen.value = false
    await load()
  } catch (e) {
    toast.error(e?.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const removeProfile = async (p) => {
  if (!confirm(`删除出口分类「${p.name}」？`)) return
  try {
    await adminApi.deleteExitProfile(p.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    // 有套餐绑定时后端拒绝，msg 形如「该分类还被 N 个套餐使用」—— 原样展示
    toast.error(e?.message || '删除失败')
  }
}

// 布局是 keep-alive：onMounted 覆盖首屏，onActivated 只在切回来时刷新
let firstActivate = true
onMounted(load)
onActivated(() => {
  if (firstActivate) {
    firstActivate = false
    return
  }
  load()
})
</script>

<template>
  <div class="ep">
    <div class="ep-head">
      <h1 class="ep-title">静态 IP 出口</h1>
      <div class="ep-head-actions">
        <Button variant="secondary" size="sm" :loading="loading" @click="load">
          <Icon name="refresh" :size="16" /> 刷新
        </Button>
        <Button variant="primary" size="sm" @click="openCreate">
          <Icon name="plus" :size="16" /> 新建出口分类
        </Button>
      </div>
    </div>

    <p class="ep-hint">
      一个出口分类 = 一组<strong>前置节点</strong> + 一个 <strong>Webshare 账号</strong>。套餐绑了分类后，
      用户连这些前置节点时出站自动走住宅 IP，每个用户按分类里的流量 / 有效期开号。
    </p>

    <Card :padding="false">
      <div class="ep-table-wrap">
        <table class="ep-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>Webshare 账号</th>
              <th>前置节点</th>
              <th>流量 / 时长</th>
              <th>状态</th>
              <th class="ep-col-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in profiles" :key="p.id">
              <td class="ep-name">{{ p.name }}</td>
              <td>
                <span v-if="providerOf(p)" class="ep-provider">
                  {{ providerName(p) }}
                  <Badge :variant="providerStatus(providerOf(p)).variant" dot>
                    {{ providerOf(p).enabled === false ? '已停用' : providerStatus(providerOf(p)).text }}
                  </Badge>
                </span>
                <span v-else class="ep-muted">{{ providerName(p) }}</span>
              </td>
              <td class="mono" :title="(p.node_ids || []).map(nodeName).join('、')">
                {{ nodeCount(p) }} 台
              </td>
              <td>
                <span class="mono">{{ trafficText(p) }} / {{ durationText(p) }}</span>
                <span class="ep-sub mono">并发 {{ num(p.max_thread_count) ?? 0 }}</span>
              </td>
              <td>
                <Badge :variant="p.enabled !== false ? 'success' : 'warning'" dot>
                  {{ p.enabled !== false ? '已启用' : '已停用' }}
                </Badge>
              </td>
              <td class="ep-col-act">
                <div class="ep-actions">
                  <Button variant="ghost" size="sm" title="编辑" @click="openEdit(p)">
                    <Icon name="edit" :size="14" />
                  </Button>
                  <Button variant="ghost" size="sm" title="删除" @click="removeProfile(p)">
                    <Icon name="trash" :size="14" />
                  </Button>
                </div>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="6" class="ep-empty">
                加载失败：{{ loadError }}
                <Button variant="secondary" size="sm" @click="load">重试</Button>
              </td>
            </tr>
            <tr v-else-if="!profiles.length">
              <td colspan="6" class="ep-empty">
                {{ loading ? '加载中…' : '还没有出口分类' }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Modal v-model="formOpen" :title="form.id ? '编辑出口分类' : '新建出口分类'" width="560px">
      <div class="ep-form">
        <label class="ep-field">
          <span>分类名称</span>
          <Input v-model="form.name" placeholder="如：香港住宅出口" />
        </label>

        <label class="ep-field">
          <span>Webshare 账号</span>
          <Select v-model="form.provider_id" :options="providerOptions" placeholder="选择账号" />
          <span v-if="!providerOptions.length" class="ep-hint-text">
            还没有 Webshare 账号，先去「上游 · Webshare」页接入。
          </span>
        </label>

        <div class="ep-grid">
          <label class="ep-field">
            <span>每个用户流量（GB）</span>
            <Input v-model="form.traffic_gb" type="number" min="0" step="1" mono placeholder="5" />
          </label>
          <label class="ep-field">
            <span>有效期（天）</span>
            <Input v-model="form.duration_days" type="number" min="1" step="1" mono placeholder="30" />
          </label>
          <label class="ep-field">
            <span>并发数</span>
            <Input v-model="form.max_thread_count" type="number" min="0" step="1" mono placeholder="50" />
          </label>
        </div>

        <div class="ep-grid">
          <label class="ep-field">
            <span>SOCKS 地址</span>
            <Input v-model="form.socks_host" mono placeholder="p.webshare.io" />
          </label>
          <label class="ep-field">
            <span>端口</span>
            <Input v-model="form.socks_port" type="number" min="1" max="65535" step="1" mono placeholder="1080" />
          </label>
        </div>

        <div class="ep-nodes">
          <span class="ep-nodes-title">前置节点（用户连这些节点时走住宅出口）</span>
          <p class="ep-hint-text">
            {{ form.node_ids.length ? `已选 ${form.node_ids.length} 台` : '不选 = 该分类暂时没有可用入口' }}
          </p>
          <div v-if="nodeOptions.length" class="ep-node-list">
            <label v-for="n in nodeOptions" :key="n.id" class="ep-node-row">
              <input type="checkbox" :value="n.id" v-model="form.node_ids" />
              <span>{{ n.name }}</span>
              <Badge :variant="statusVariant(n.status)" dot>{{ n.status || '未知' }}</Badge>
            </label>
          </div>
          <p v-else class="ep-hint-text">还没有节点。</p>
        </div>

        <label class="ep-check">
          <input type="checkbox" v-model="form.enabled" />
          <span>启用（停用后不能绑新套餐）</span>
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="formOpen = false">取消</Button>
        <Button variant="primary" size="sm" :loading="saving" @click="save">保存</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.ep {
  max-width: 1200px;
}
.ep-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-4);
}
.ep-head-actions {
  display: flex;
  gap: var(--space-2);
}
.ep-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.ep-hint {
  margin: 0 0 var(--space-4);
  color: var(--text-secondary);
  font-size: var(--text-sm);
}
.ep-table-wrap {
  overflow-x: auto;
}
.ep-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.ep-table th,
.ep-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  vertical-align: middle;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
.ep-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.ep-table tbody tr:last-child td {
  border-bottom: none;
}
.ep-table tbody tr:hover {
  background: var(--bg-hover);
}
.ep-name {
  font-weight: var(--font-medium);
}
.ep-provider {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
}
/* 流量 / 时长下面那行小字（并发） */
.ep-sub {
  display: block;
  color: var(--text-muted);
  font-size: var(--text-xs);
}
.ep-col-act {
  text-align: right;
}
.ep-actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-1);
}
.ep-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
.ep-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.ep-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.ep-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: var(--space-3);
}
.ep-grid + .ep-grid {
  grid-template-columns: repeat(2, minmax(0, 1fr));
}
.ep-nodes {
  border-top: 1px solid var(--border-subtle);
  padding-top: var(--space-3);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.ep-nodes-title {
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  color: var(--text-primary);
}
.ep-node-list {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-1) var(--space-3);
  max-height: 200px;
  overflow-y: auto;
}
.ep-node-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--text-secondary);
  cursor: pointer;
}
.ep-node-row input[type='checkbox'] {
  cursor: pointer;
}
.ep-check {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--text-secondary);
  cursor: pointer;
}
.ep-hint-text {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.ep-muted {
  color: var(--text-muted);
}
.mono {
  font-family: var(--font-mono);
}

@media (max-width: 768px) {
  .ep-head {
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-2);
  }
  .ep-grid,
  .ep-grid + .ep-grid,
  .ep-node-list {
    grid-template-columns: 1fr;
  }
}
</style>
