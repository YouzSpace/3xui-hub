<script setup>
/**
 * 配置模板面板（#地区功能-20）。
 *
 * 模板 = 一套与具体节点无关的标准入站 / 出站 / 路由片段，可以一次套用到多台 xray 节点。
 * 三条与后端一致的约定：
 *  1. JSON 列（settings / stream_settings）提交前先 JSON.parse 校验，格式错就当场提示、不发请求
 *     （后端也会 422，但没必要让用户白等一次往返）；
 *  2. 路由模板的 domains / ips 用逗号分隔输入，提交时拆成数组（后端按 JSON 数组存）；
 *  3. 3x-ui 节点没有这套表的对应概念，套用时整台跳过 —— 所以节点多选只列 driver_type=xray 的节点。
 */
import { ref, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Icon from '@/components/ui/Icon.vue'
import Modal from '@/components/ui/Modal.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const inboundProtocolOptions = ['vless', 'vmess', 'trojan', 'shadowsocks', 'hysteria2'].map((p) => ({ label: p, value: p }))
const outboundProtocolOptions = ['freedom', 'blackhole', 'socks', 'http'].map((p) => ({ label: p, value: p }))

// 套用结果状态 → 展示（与后端 status 取值一致）
const STATUS = {
  applied: { label: '成功', variant: 'success' },
  skipped: { label: '跳过', variant: 'warning' },
  failed: { label: '失败', variant: 'danger' },
}
const statusOf = (s) => STATUS[s] || { label: s || '未知', variant: 'info' }

// ---- 列表 ----

const loading = ref(false)
const templates = ref({ inbounds: [], outbounds: [], routing: [] })

const load = async () => {
  loading.value = true
  try {
    const res = await adminApi.listXrayTemplates()
    templates.value = {
      inbounds: Array.isArray(res?.inbounds) ? res.inbounds : [],
      outbounds: Array.isArray(res?.outbounds) ? res.outbounds : [],
      routing: Array.isArray(res?.routing) ? res.routing : [],
    }
  } catch (e) {
    toast.error(e.message || '加载配置模板失败')
  } finally {
    loading.value = false
  }
}

// ---- 新建 / 编辑 ----

const modalOpen = ref(false)
const editingKind = ref('inbound')
const editingId = ref(null)
const saving = ref(false)
const form = ref(emptyForm('inbound'))

function emptyForm(kind) {
  if (kind === 'inbound') {
    return { name: '', tag: '', protocol: 'vless', port: 443, listen: '0.0.0.0', settingsText: '', streamText: '', enabled: true }
  }
  if (kind === 'outbound') {
    return { name: '', tag: 'direct', protocol: 'freedom', settingsText: '', streamText: '', enabled: true }
  }
  return { name: '', domains: '', ips: '', port: '', network: '', outbound_tag: 'direct', remark: '', enabled: true }
}

/** JSON 列 → 文本域内容（后端返回的是已解析的对象，空值为 null）。 */
const prettyJson = (v) => (v === null || v === undefined || v === '' ? '' : JSON.stringify(v, null, 2))

const openCreate = (kind) => {
  editingKind.value = kind
  editingId.value = null
  form.value = emptyForm(kind)
  modalOpen.value = true
}

const openEdit = (kind, row) => {
  editingKind.value = kind
  editingId.value = row.id
  if (kind === 'routing') {
    form.value = {
      name: row.name || '',
      domains: (row.domains || []).join(', '),
      ips: (row.ips || []).join(', '),
      port: row.port || '',
      network: row.network || '',
      outbound_tag: row.outbound_tag || '',
      remark: row.remark || '',
      enabled: row.enabled !== false,
    }
  } else {
    form.value = {
      name: row.name || '',
      tag: row.tag || '',
      protocol: row.protocol || '',
      port: kind === 'inbound' ? (row.port ?? 443) : 443,
      listen: row.listen || '0.0.0.0',
      settingsText: prettyJson(row.settings),
      streamText: prettyJson(row.stream_settings),
      enabled: row.enabled !== false,
    }
  }
  modalOpen.value = true
}

/**
 * 解析 JSON 文本域：空 → 该列不配（后端落 NULL）；不是对象/数组 → 抛错。
 * 抛出的错直接给用户看，调用方负责「提示 + 不提交」。
 */
const parseJsonInput = (text, label) => {
  const raw = String(text ?? '').trim()
  if (raw === '') return null
  let parsed
  try {
    parsed = JSON.parse(raw)
  } catch (e) {
    throw new Error(`${label} 不是合法的 JSON（${e.message}）`)
  }
  if (parsed === null || typeof parsed !== 'object') {
    throw new Error(`${label} 必须是 JSON 对象或数组`)
  }
  return parsed
}

/** 逗号（中英文）/ 空白分隔 → 数组；空项丢掉。 */
const splitList = (text) =>
  String(text ?? '')
    .split(/[,，\s]+/)
    .map((s) => s.trim())
    .filter(Boolean)

const save = async () => {
  const kind = editingKind.value
  const f = form.value
  const name = String(f.name || '').trim()
  if (!name) {
    toast.error('模板名称必填')
    return
  }

  let payload
  if (kind === 'routing') {
    const outboundTag = String(f.outbound_tag || '').trim()
    if (!outboundTag) {
      toast.error('出站 tag 必填')
      return
    }
    payload = {
      name,
      domains: splitList(f.domains),
      ips: splitList(f.ips),
      port: String(f.port || '').trim() || null,
      network: String(f.network || '').trim() || null,
      outbound_tag: outboundTag,
      remark: String(f.remark || '').trim() || null,
      enabled: !!f.enabled,
    }
  } else {
    let settings = null
    let stream = null
    try {
      settings = parseJsonInput(f.settingsText, 'settings')
      stream = parseJsonInput(f.streamText, 'stream_settings')
    } catch (e) {
      // JSON 格式错：只提示，不提交
      toast.error(e.message)
      return
    }
    const tag = String(f.tag || '').trim()
    if (!tag) {
      toast.error('tag 必填')
      return
    }
    payload = { name, tag, protocol: f.protocol, settings, stream_settings: stream, enabled: !!f.enabled }
    if (kind === 'inbound') {
      const port = Number(f.port)
      if (!Number.isInteger(port) || port < 1 || port > 65535) {
        toast.error('端口必须在 1-65535 之间')
        return
      }
      payload.port = port
      payload.listen = String(f.listen || '').trim() || '0.0.0.0'
    }
  }

  saving.value = true
  try {
    if (editingId.value) await adminApi.updateXrayTemplate(kind, editingId.value, payload)
    else await adminApi.createXrayTemplate(kind, payload)
    toast.success(editingId.value ? '已保存' : '已创建')
    modalOpen.value = false
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const remove = async (kind, row) => {
  if (!confirm(`删除模板「${row.name}」？已套用过的节点不受影响（套用是拷贝，不是引用）`)) return
  try {
    await adminApi.deleteXrayTemplate(kind, row.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

// ---- 套用到节点 ----

const nodesLoading = ref(false)
const nodes = ref([])
const selectedNodeIds = ref([])
const overwriteCustom = ref(false)
const applying = ref(false)
const applyResult = ref(null)

const loadNodes = async () => {
  nodesLoading.value = true
  try {
    const res = await adminApi.listNodes({ all: 1 })
    const list = Array.isArray(res) ? res : []
    // 只有 xray 节点有模板的落点；3x-ui 节点套用时会被整台跳过，干脆不列出来
    nodes.value = list.filter((n) => n.driver_type === 'xray')
  } catch (e) {
    toast.error(e.message || '加载节点失败')
  } finally {
    nodesLoading.value = false
  }
}

const toggleNode = (id) => {
  const i = selectedNodeIds.value.indexOf(id)
  if (i >= 0) selectedNodeIds.value.splice(i, 1)
  else selectedNodeIds.value.push(id)
}

const apply = async () => {
  if (!selectedNodeIds.value.length) {
    toast.error('请先勾选要套用的节点')
    return
  }
  applying.value = true
  applyResult.value = null
  try {
    const res = await adminApi.applyXrayTemplates({
      node_ids: [...selectedNodeIds.value],
      overwrite_custom: !!overwriteCustom.value,
    })
    applyResult.value = {
      results: Array.isArray(res?.results) ? res.results : [],
      applied: res?.applied || 0,
      skipped: res?.skipped || 0,
      failed: res?.failed || 0,
    }
    const { applied, skipped, failed } = applyResult.value
    if (failed) toast.error(`套用完成：成功 ${applied}，跳过 ${skipped}，失败 ${failed}`)
    else toast.success(`套用完成：成功 ${applied}，跳过 ${skipped}`)
  } catch (e) {
    toast.error(e.message || '套用失败')
  } finally {
    applying.value = false
  }
}

onMounted(() => {
  load()
  loadNodes()
})
</script>

<template>
  <div class="xt">
    <p class="xt-desc">
      配置模板是「与节点无关的标准配置片段」：维护一套入站 / 出站 / 路由，一次套用到多台 xray 节点。
      套用是拷贝不是引用 —— 之后单独改某台节点不影响模板，改模板也不会回写已套用的节点。
    </p>

    <!-- 入站模板 -->
    <Card class="xt-block">
      <div class="xt-block-head">
        <div>
          <h3 class="xt-subtitle">入站模板</h3>
          <p class="xt-muted-sm xt-mb0">套用时按 tag 对齐节点入站：模板里有的增改，模板里没有的删掉。</p>
        </div>
        <Button variant="primary" size="sm" @click="openCreate('inbound')">
          <Icon name="plus" :size="16" /> 新建入站模板
        </Button>
      </div>
      <div class="xt-table-wrap">
        <table class="xt-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>tag</th>
              <th>协议</th>
              <th>端口 / 监听</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in templates.inbounds" :key="t.id">
              <td>{{ t.name }}</td>
              <td class="xt-mono">{{ t.tag }}</td>
              <td><Badge variant="info">{{ t.protocol }}</Badge></td>
              <td class="xt-mono">{{ t.port }} / {{ t.listen || '—' }}</td>
              <td><Badge :variant="t.enabled ? 'success' : 'warning'" dot>{{ t.enabled ? '启用' : '停用' }}</Badge></td>
              <td>
                <div class="xt-actions">
                  <Button size="sm" variant="ghost" @click="openEdit('inbound', t)"><Icon name="edit" :size="16" /></Button>
                  <Button size="sm" variant="ghost" @click="remove('inbound', t)"><Icon name="trash" :size="16" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!loading && templates.inbounds.length === 0">
              <td colspan="6" class="xt-empty">还没有入站模板。建一条之后，新节点可以一键套用同一份标准入站。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 出站模板 -->
    <Card class="xt-block">
      <div class="xt-block-head">
        <div>
          <h3 class="xt-subtitle">出站模板</h3>
          <p class="xt-muted-sm xt-mb0">套用时节点的出站全量按模板重写（模板外的出站会被删掉）。</p>
        </div>
        <Button variant="primary" size="sm" @click="openCreate('outbound')">
          <Icon name="plus" :size="16" /> 新建出站模板
        </Button>
      </div>
      <div class="xt-table-wrap">
        <table class="xt-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>tag</th>
              <th>协议</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in templates.outbounds" :key="t.id">
              <td>{{ t.name }}</td>
              <td class="xt-mono">{{ t.tag }}</td>
              <td><Badge variant="info">{{ t.protocol }}</Badge></td>
              <td><Badge :variant="t.enabled ? 'success' : 'warning'" dot>{{ t.enabled ? '启用' : '停用' }}</Badge></td>
              <td>
                <div class="xt-actions">
                  <Button size="sm" variant="ghost" @click="openEdit('outbound', t)"><Icon name="edit" :size="16" /></Button>
                  <Button size="sm" variant="ghost" @click="remove('outbound', t)"><Icon name="trash" :size="16" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!loading && templates.outbounds.length === 0">
              <td colspan="5" class="xt-empty">还没有出站模板。至少留一条 direct（freedom）出站，路由才有兜底目标。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 路由模板 -->
    <Card class="xt-block">
      <div class="xt-block-head">
        <div>
          <h3 class="xt-subtitle">路由模板</h3>
          <p class="xt-muted-sm xt-mb0">套用时节点的路由规则全量按模板重写，按入站 / 域名 / IP 分流到模板里的出站 tag。</p>
        </div>
        <Button variant="primary" size="sm" @click="openCreate('routing')">
          <Icon name="plus" :size="16" /> 新建路由模板
        </Button>
      </div>
      <div class="xt-table-wrap">
        <table class="xt-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>域名</th>
              <th>IP</th>
              <th>端口 / 网络</th>
              <th>目标出站</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in templates.routing" :key="t.id">
              <td>{{ t.name }}</td>
              <td class="xt-mono xt-clip">{{ (t.domains || []).join(', ') || '—' }}</td>
              <td class="xt-mono xt-clip">{{ (t.ips || []).join(', ') || '—' }}</td>
              <td class="xt-mono">{{ [t.port, t.network].filter(Boolean).join(' / ') || '不限' }}</td>
              <td><Badge>{{ t.outbound_tag }}</Badge></td>
              <td><Badge :variant="t.enabled ? 'success' : 'warning'" dot>{{ t.enabled ? '启用' : '停用' }}</Badge></td>
              <td>
                <div class="xt-actions">
                  <Button size="sm" variant="ghost" @click="openEdit('routing', t)"><Icon name="edit" :size="16" /></Button>
                  <Button size="sm" variant="ghost" @click="remove('routing', t)"><Icon name="trash" :size="16" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!loading && templates.routing.length === 0">
              <td colspan="7" class="xt-empty">还没有路由模板。常见的两条：私有网段 / BT 走 blackhole，国内域名走 direct。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 套用到节点 -->
    <Card class="xt-block">
      <div class="xt-block-head">
        <div>
          <h3 class="xt-subtitle">套用到节点</h3>
          <p class="xt-muted-sm xt-mb0">
            只列 xray 节点：3x-ui 节点的入站 / 出站 / 路由在面板侧，没有模板的对应表，套用时整台跳过。
            单独改过配置的节点默认也跳过，勾上「覆盖」才会重写。
          </p>
        </div>
      </div>

      <div v-if="nodesLoading" class="xt-empty">加载节点…</div>
      <div v-else-if="nodes.length === 0" class="xt-empty">还没有 xray 节点（去「节点管理」新建）</div>
      <template v-else>
        <div class="xt-nodes">
          <label v-for="n in nodes" :key="n.id" class="xt-node">
            <input
              type="checkbox"
              :checked="selectedNodeIds.includes(n.id)"
              @change="toggleNode(n.id)"
            />
            <span class="xt-node-name">{{ n.name }}</span>
            <Badge variant="info">xray</Badge>
          </label>
        </div>

        <div class="xt-apply-actions">
          <label class="xt-field-row">
            <input v-model="overwriteCustom" type="checkbox" />
            <span>覆盖单独配置过的节点</span>
          </label>
          <Button variant="primary" size="sm" :loading="applying" @click="apply">
            <Icon name="check" :size="16" /> 套用（已选 {{ selectedNodeIds.length }} 台）
          </Button>
        </div>

        <div v-if="applyResult" class="xt-result">
          <div class="xt-result-summary">
            成功 {{ applyResult.applied }} · 跳过 {{ applyResult.skipped }} · 失败 {{ applyResult.failed }}
          </div>
          <div v-for="r in applyResult.results" :key="r.node_id" class="xt-result-row">
            <span class="xt-result-name">{{ r.node_name || `#${r.node_id}` }}</span>
            <Badge :variant="statusOf(r.status).variant" dot>{{ statusOf(r.status).label }}</Badge>
            <span class="xt-muted-sm">{{ r.message || '—' }}</span>
          </div>
        </div>
      </template>
    </Card>

    <!-- 新建 / 编辑 -->
    <Modal
      v-model="modalOpen"
      :title="`${editingId ? '编辑' : '新建'}${editingKind === 'inbound' ? '入站' : editingKind === 'outbound' ? '出站' : '路由'}模板`"
      width="640px"
    >
      <div class="xt-form">
        <div class="xt-grid">
          <label class="xt-field">
            <span>名称</span>
            <Input v-model="form.name" placeholder="如：标准 vless-reality 443" />
          </label>
          <label v-if="editingKind !== 'routing'" class="xt-field">
            <span>tag（套用时按它对上节点里的条目）</span>
            <Input v-model="form.tag" mono placeholder="如：vless-reality" />
          </label>
          <label v-else class="xt-field">
            <span>目标出站 tag（必须是出站模板里的 tag）</span>
            <Input v-model="form.outbound_tag" mono placeholder="如：direct" />
          </label>
        </div>

        <!-- 入站 / 出站 -->
        <template v-if="editingKind !== 'routing'">
          <div class="xt-grid">
            <label class="xt-field">
              <span>协议</span>
              <Select
                v-model="form.protocol"
                :options="editingKind === 'inbound' ? inboundProtocolOptions : outboundProtocolOptions"
              />
            </label>
            <label v-if="editingKind === 'inbound'" class="xt-field">
              <span>端口</span>
              <Input v-model="form.port" type="number" />
            </label>
          </div>
          <label v-if="editingKind === 'inbound'" class="xt-field">
            <span>listen（监听地址）</span>
            <Input v-model="form.listen" mono placeholder="0.0.0.0" />
          </label>
          <label class="xt-field">
            <span>settings（JSON；空 = 不配）</span>
            <textarea v-model="form.settingsText" class="xt-textarea" rows="4" placeholder='{ "clients": [], "decryption": "none" }' />
          </label>
          <label class="xt-field">
            <span>stream_settings（JSON；空 = 不配）</span>
            <textarea v-model="form.streamText" class="xt-textarea" rows="4" placeholder='{ "network": "tcp", "security": "none" }' />
          </label>
        </template>

        <!-- 路由 -->
        <template v-else>
          <label class="xt-field">
            <span>域名（逗号分隔；支持 domain: / full: / keyword: / geosite: 前缀）</span>
            <textarea v-model="form.domains" class="xt-textarea" rows="3" placeholder="geosite:cn, domain:example.com" />
          </label>
          <label class="xt-field">
            <span>IP（逗号分隔；支持 geoip: 前缀与 CIDR）</span>
            <textarea v-model="form.ips" class="xt-textarea" rows="2" placeholder="geoip:cn, 10.0.0.0/8" />
          </label>
          <div class="xt-grid">
            <label class="xt-field">
              <span>端口（空 = 不限；如 443 或 1000-2000）</span>
              <Input v-model="form.port" placeholder="可空" />
            </label>
            <label class="xt-field">
              <span>网络（空 = 不限；tcp / udp）</span>
              <Input v-model="form.network" placeholder="可空" />
            </label>
          </div>
          <label class="xt-field">
            <span>备注</span>
            <Input v-model="form.remark" placeholder="如：国内直连" />
          </label>
        </template>

        <label class="xt-field-row">
          <input v-model="form.enabled" type="checkbox" />
          <span>启用</span>
        </label>
      </div>
      <template #footer>
        <Button variant="ghost" @click="modalOpen = false">取消</Button>
        <Button variant="primary" :loading="saving" @click="save">{{ editingId ? '保存' : '创建' }}</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.xt {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}
.xt-desc {
  margin: 0;
  color: var(--text-muted);
  font-size: var(--text-sm);
  line-height: 1.6;
}
.xt-block-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
  margin-bottom: var(--space-3);
}
.xt-subtitle {
  margin: 0 0 var(--space-1);
  font-size: var(--text-base);
  font-weight: var(--font-semibold);
}
.xt-muted-sm {
  color: var(--text-muted);
  font-size: var(--text-xs);
  line-height: 1.6;
}
.xt-mb0 {
  margin: 0;
}
.xt-table-wrap {
  overflow-x: auto;
}
.xt-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.xt-table th,
.xt-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
.xt-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.xt-table tbody tr:hover {
  background: var(--bg-hover);
}
.xt-clip {
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.xt-actions {
  display: flex;
  gap: var(--space-1);
}
.xt-empty {
  color: var(--text-muted);
  font-size: var(--text-xs);
  text-align: center;
  padding: var(--space-5) var(--space-3);
  white-space: normal;
}
.xt-mono {
  font-family: var(--font-mono);
}
.xt-nodes {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2) var(--space-4);
  margin-bottom: var(--space-3);
}
.xt-node {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  cursor: pointer;
}
.xt-node-name {
  color: var(--text-primary);
}
.xt-apply-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  padding-top: var(--space-3);
  border-top: 1px solid var(--border-subtle);
}
.xt-result {
  margin-top: var(--space-3);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.xt-result-summary {
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.xt-result-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  flex-wrap: wrap;
}
.xt-result-name {
  min-width: 120px;
  color: var(--text-primary);
}
.xt-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.xt-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-3);
}
@media (max-width: 767px) {
  .xt-grid {
    grid-template-columns: 1fr;
  }
  .xt-block-head,
  .xt-apply-actions {
    flex-direction: column;
    align-items: stretch;
  }
}
.xt-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.xt-field-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.xt-textarea {
  width: 100%;
  padding: var(--space-2);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--bg-input);
  color: var(--text-primary);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: 1.6;
  resize: vertical;
  box-sizing: border-box;
}
.xt-textarea:focus {
  outline: none;
  border-color: var(--accent);
}
</style>
