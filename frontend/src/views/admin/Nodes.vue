<script setup>
/**
 * Admin 节点管理：表格 + 创建/编辑 Modal + 测试连接 + 删除。
 * 列表分页：每页 50 条，页码走 ?page=（刷新/切页回来不丢页）。
 */
import { ref, computed, watch, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import Skeleton from '@/components/ui/Skeleton.vue'
import Pagination from '@/components/ui/Pagination.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { usePagedList } from '@/composables/usePagedList'

const toast = useToast()
const nodes = ref([])
const testingId = ref(null)

const { page, loading, loadError, pagination, hasPrev, hasNext, prev, next, fetchPage } = usePagedList()

const modalOpen = ref(false)
const editing = ref(null)
const saved = ref(false)
const panelUrl = ref('')
// 3x-ui 可绑定的协议：面板侧按协议分组下发入站（后端 probe-inbounds 返回的动态键）。
// 顺序即界面顺序；不在表里的协议（如 vmess/shadowsocks）照样能显示 —— 见 bindableProtocols。
const BIND_PROTOCOLS = ['vless', 'trojan', 'hysteria']
const BIND_LABELS = { vless: 'VLESS', trojan: 'Trojan', hysteria: 'Hysteria2' }
// 从 3x-ui 拉取的入站列表（按协议分组，{id,label}）
const inboundOptions = ref({ vless: [], trojan: [], hysteria: [] })
const emptyInbounds = () => ({ vless: [], trojan: [], hysteria: [] })
// 详情里的 inbounds 是 {协议: [id...]}（后端 present 的统一形状）：按三个绑定协议取，
// 缺键给空数组，老数据（单值）也兼容。
const bindableInbounds = (src) => {
  const out = emptyInbounds()
  Object.keys(out).forEach((proto) => {
    const v = src?.[proto]
    out[proto] = Array.isArray(v) ? v : v != null ? [v] : []
  })
  return out
}
const probing = ref(false)
// xray 节点详情（编辑态：安装命令 / 密钥前缀 / 注册状态）
const xrayInfo = ref(null)
const form = ref({
  name: '', driver_type: '3x-ui', host: '', host_v6: '', port: 443, scheme: 'https', web_base_path: '',
  username: 'admin', password: '', api_key: '',
  enabled: true, verify_ssl: false, inbounds: emptyInbounds(),
})

const driverTypeOptions = [
  { label: '3x-ui 面板', value: '3x-ui' },
  { label: 'xray 裸内核', value: 'xray' },
]
const isXrayForm = computed(() => form.value.driver_type === 'xray')

const schemeOptions = [
  { label: 'https', value: 'https' },
  { label: 'http', value: 'http' },
]

// 用当前表单凭据连 3x-ui 拉取入站列表，填到下拉框
const fetchInbounds = async () => {
  if (!form.value.host) {
    toast.error('请先填面板地址/主机')
    return
  }
  probing.value = true
  try {
    const data = await adminApi.probeInbounds({
      scheme: form.value.scheme,
      host: form.value.host,
      port: form.value.port,
      web_base_path: form.value.web_base_path,
      api_key: form.value.api_key || undefined,
      username: form.value.username || undefined,
      password: form.value.password || undefined,
      verify_ssl: form.value.verify_ssl,
    })
    // 后端按实际出现的协议分组返回（键是动态的），这里照单全收，再补齐三个固定键
    const next = emptyInbounds()
    Object.keys(data || {}).forEach((proto) => {
      next[proto] = Array.isArray(data[proto]) ? data[proto] : []
    })
    inboundOptions.value = next
    const summary = Object.keys(next)
      .filter((proto) => next[proto].length)
      .map((proto) => `${next[proto].length} 个 ${BIND_LABELS[proto] || proto}`)
    toast.success(summary.length ? `已拉取 ${summary.join('、')} 入站` : '该面板没有可绑定的入站')
  } catch (e) {
    toast.error(e.message || '拉取失败')
  } finally {
    probing.value = false
  }
}

// 切换入站多选
const toggleInbound = (proto, id) => {
  if (!Array.isArray(form.value.inbounds[proto])) form.value.inbounds[proto] = []
  const arr = form.value.inbounds[proto]
  const idx = arr.indexOf(id)
  if (idx >= 0) {
    arr.splice(idx, 1)
  } else {
    arr.push(id)
  }
  // 自动用第一个选中的入站备注作节点名称
  if (arr.length > 0) {
    const opt = inboundOptions.value[proto]?.find((o) => o.id === arr[0])
    if (opt && opt.remark) form.value.name = opt.remark
  }
}

// 解析面板地址（如 https://jp3.315461.xyz:40924/u6x3yohif7zOJsYtEY/）
// 自动拆出 scheme/host/port/web_base_path，拆不出的留空让用户手填。
const parsePanelUrl = () => {
  const raw = panelUrl.value.trim()
  if (!raw) return
  let url = raw
  if (!/^https?:\/\//i.test(url)) url = 'https://' + url
  try {
    const u = new URL(url)
    form.value.scheme = (u.protocol || 'https:').replace(':', '')
    if (u.hostname) form.value.host = u.hostname
    if (u.port) form.value.port = parseInt(u.port, 10)
    const path = u.pathname.replace(/\/+$/, '')
    form.value.web_base_path = path
  } catch (e) {
    toast.error('面板地址无法识别，请手动填写')
  }
}

const load = async () => {
  const res = await fetchPage((params, config) => adminApi.listNodes(params, config))
  if (res) nodes.value = res
}

// 有没有任何启用中的入站：xray 节点看 xray_inbounds、3x-ui 节点看 node_inbounds，
// 后端 present 已统一成 {协议: [id...]} 形状，这里只数长度（仪表盘统计也复用它）
const hasInbounds = (row) => Object.values(row?.inbounds || {}).some((arr) => Array.isArray(arr) && arr.length > 0)

watch(page, load)

const openCreate = () => {
  editing.value = null
  panelUrl.value = ''
  xrayInfo.value = null
  inboundOptions.value = emptyInbounds()
  form.value = {
    name: '', driver_type: '3x-ui', host: '', host_v6: '', port: 443, scheme: 'https', web_base_path: '',
    username: 'admin', password: '', api_key: '',
    enabled: true, verify_ssl: false, inbounds: emptyInbounds(),
  }
  modalOpen.value = true
}

const openEdit = async (n) => {
  editing.value = n
  panelUrl.value = `${n.scheme}://${n.host}:${n.port}${n.web_base_path || ''}`
  // 拉取节点详情，回填解密后的 api_key（密码不回填，留空即不改）
  let detail = n
  try {
    detail = await adminApi.getNode(n.id)
  } catch (e) {
    // 拉取失败则用列表数据
  }
  form.value = {
    name: detail.name, driver_type: detail.driver_type || '3x-ui',
    host: detail.host, host_v6: detail.host_v6 || '', port: detail.port, scheme: detail.scheme,
    web_base_path: detail.web_base_path,
    username: detail.username, password: '', api_key: detail.api_key || '',
    enabled: detail.enabled, verify_ssl: !!detail.verify_ssl,
    inbounds: bindableInbounds(detail.inbounds),
  }
  // 用当前 id 作占位项，点「拉取入站」后替换为真实备注
  const seed = (ids) => (Array.isArray(ids) ? ids : ids != null ? [ids] : []).map(id => ({ id, label: `当前入站 id:${id}（点拉取刷新名称）` }))
  const options = emptyInbounds()
  Object.keys(options).forEach((proto) => { options[proto] = seed(detail.inbounds?.[proto]) })
  inboundOptions.value = options
  xrayInfo.value = detail.driver_type === 'xray'
    ? {
        install_command: detail.install_command || '',
        node_secret_prefix: detail.node_secret_prefix || null,
        // 详情接口给全文（列表只给前 4 位）。密钥是 40 位定长串，挤在元信息行里看不全，
        // 所以单独占一行并配复制按钮。
        node_secret: detail.node_secret || null,
        reality_registered: !!detail.reality_registered,
        has_inbounds: hasInbounds(detail),
        xray_version: detail.xray_version || null,
        lag_seconds: detail.lag_seconds ?? 0,
        agent: detail.agent || null,
        ws_connected: !!detail.ws_connected,
        agent_version: detail.agent_version || null,
        agent_latest: detail.agent_latest || null,
        agent_upgrade_pending: !!detail.agent_upgrade_pending,
        // issue #94 R5：BBR 运行态（agent 心跳上报，未上报 = null）
        bbr: detail.bbr || null,
      }
    : null
  modalOpen.value = true
}

const save = async () => {
  const xray = isXrayForm.value
  if (!form.value.name || (!xray && !form.value.host)) {
    toast.error(xray ? '名称必填' : '名称和主机必填')
    return
  }
  try {
    // xray 节点只提交适用字段（面板连接/入站概念不适用）
    const payload = xray
      ? {
          name: form.value.name, driver_type: 'xray', host: form.value.host || '',
          // issue #88：IPv6 入口地址（可选）。空串提交 = 清空（后端存 driver_config.host_v6）。
          host_v6: (form.value.host_v6 || '').trim(),
          enabled: form.value.enabled,
        }
      : { ...form.value }
    // 编辑时空密码/api_key 不提交（保留原值）
    if (editing.value && !xray) {
      if (!payload.password) delete payload.password
      if (!payload.api_key) delete payload.api_key
    }
    // 后端会说明「是否需要后台同步/初始化」以及去哪儿看进度，优先用它的文案
    const res = editing.value
      ? await adminApi.updateNode(editing.value.id, payload)
      : await adminApi.createNode(payload)
    toast.success(res?.message || (editing.value ? '已更新' : '已创建'))
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    saved.value = true
    await load()
    if (!editing.value && xray && res?.id) {
      // 新建 xray 节点：直接进编辑态展示安装命令（一步复制）；按钮仍闪一下对勾
      setTimeout(() => { saved.value = false }, 900)
      await openEdit(res)
    } else {
      setTimeout(() => {
        saved.value = false
        modalOpen.value = false
      }, 900)
    }
  } catch (e) {
    toast.error(e.message || '保存失败')
  }
}

// 复制文本（非 https 场景走 execCommand 兜底）
const copyText = async (text) => {
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    toast.success('已复制')
  } catch (e) {
    const ta = document.createElement('textarea')
    ta.value = text
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    document.body.removeChild(ta)
    toast.success('已复制')
  }
}

const test = async (n) => {
  testingId.value = n.id
  try {
    const r = await adminApi.testNode(n.id)
    toast.success(n.driver_type === 'xray'
      ? `服务器在线（心跳正常${r.latency_ms ? ` · ${r.latency_ms}ms` : ''}）`
      : `连接正常 · ${r.latency_ms}ms`)
    await load()
  } catch (e) {
    toast.error(e.message || '连接失败')
    await load()
  } finally {
    testingId.value = null
  }
}

// agent 自升级：只入队指令，节点取走后自己下载→校验→替换→重启（不断流，见 agent/upgrade.go）
const upgradingAgent = ref(false)
const agentOutdated = computed(() => {
  const info = xrayInfo.value
  if (!info || !info.agent_latest) return false
  return info.agent_version !== info.agent_latest
})

const upgradeAgent = async () => {
  if (!editing.value) return
  upgradingAgent.value = true
  try {
    const res = await adminApi.upgradeNodeAgent(editing.value.id)
    toast.success(res?.message || '已下发升级指令')
    await openEdit(editing.value) // 重新拉详情：agent_upgrade_pending 变 true
  } catch (e) {
    toast.error(e.message || '下发失败')
  } finally {
    upgradingAgent.value = false
  }
}

// ── 系统调优：BBR 运行态 + 一键开启（issue #94 R4/R5）──
// 这是**系统 TCP BBR**（net.ipv4.tcp_congestion_control + net.core.default_qdisc），
// 对 vless/vmess/trojan/ss 有效；HY2 走 QUIC，它的 BBR 是入站表单里的「拥塞控制」，
// 两者是两层，别混。字段未上报（老 agent / 非 Linux）一律显示「未上报」，不猜。
const bbrText = computed(() => {
  const b = xrayInfo.value?.bbr || {}
  const text = (v) => (v == null || v === '' ? '未上报' : String(v))
  return {
    congestion: text(b.tcp_congestion_control),
    qdisc: text(b.default_qdisc),
    module: b.bbr_module == null ? '未上报' : b.bbr_module ? '已加载' : '未加载',
    conntrack: b.conntrack_max == null ? '未上报' : Number(b.conntrack_max).toLocaleString(),
  }
})

const bbrEnabled = computed(() => xrayInfo.value?.bbr?.tcp_congestion_control === 'bbr')

const enablingBBR = ref(false)
const enableBBR = async () => {
  if (!editing.value) return
  enablingBBR.value = true
  try {
    const res = await adminApi.sysctlApply(editing.value.id, { profile: 'bbr' })
    if (res?.pending) {
      toast.error(res.message || '节点未及时回执（可能离线），稍后重试')
      return
    }
    if (res?.error) {
      // agent 原文：可能是「参数未实际生效」「modprobe 失败」这类真问题，别改写
      toast.error(res.error)
      return
    }
    const applied = res?.result?.applied || {}
    const pairs = Object.keys(applied).map((k) => `${k}=${applied[k]}`)
    toast.success(pairs.length ? `已开启 BBR（实际生效：${pairs.join('，')}）` : '已开启 BBR')
    // BBR 状态是心跳上报的（30s 一次），这里先重新拉详情刷新其余字段
    await openEdit(editing.value)
  } catch (e) {
    toast.error(e.message || '下发失败')
  } finally {
    enablingBBR.value = false
  }
}

const remove = async (n) => {
  if (!confirm(`删除服务器 ${n.name}？`)) return
  try {
    const res = await adminApi.deleteNode(n.id)
    toast.success(res?.message || '已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

const statusVariant = (s) => ({ online: 'success', offline: 'danger', maintenance: 'warning' }[s] || 'info')

// 速率格式化（监控快照用）
const fmtSpeed = (v) => {
  v = Number(v) || 0
  if (v < 1024) return `${Math.round(v)} B/s`
  if (v < 1048576) return `${(v / 1024).toFixed(1)} KB/s`
  return `${(v / 1048576).toFixed(2)} MB/s`
}

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="an">
    <div class="an-head">
      <h1 class="an-title">节点服务器</h1>
      <Button variant="primary" size="sm" @click="openCreate"><Icon name="plus" :size="16" /> 新建服务器</Button>
    </div>

    <Card padding="false">
      <div class="an-table-wrap">
        <table class="an-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>主机</th>
              <th>状态</th>
              <th>延迟</th>
              <th>VLESS</th>
              <th>Trojan</th>
              <th>HY2</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="n in nodes" :key="n.id">
              <td>
                {{ n.name }}
                <Badge v-if="n.driver_type === 'xray'" variant="info">xray</Badge>
              </td>
              <td class="mono">{{ n.driver_type === 'xray' ? (n.host || '—') : `${n.host}:${n.port}` }}</td>
              <td>
                <Badge v-if="n.driver_type === 'xray' && !hasInbounds(n)" variant="warning" dot>未建入站</Badge>
                <Badge v-else :variant="statusVariant(n.status)" dot>{{ n.status }}</Badge>
                <span v-if="n.driver_type === 'xray' && n.lag_seconds > 60" class="an-muted"> 滞后 {{ n.lag_seconds }}s</span>
                <div v-if="n.driver_type === 'xray' && n.agent" class="an-metrics">
                  CPU {{ n.agent.cpu != null ? Math.round(n.agent.cpu) + '%' : '—' }} · 内存
                  {{ n.agent.mem != null ? Math.round(n.agent.mem) + '%' : '—' }} · 连接 {{ n.agent.conns ?? 0 }}
                  <!-- agent 版本：只有 xray 节点有；落后于面板 manifest 时标黄 -->
                  <span
                    :class="['an-agent-ver', { 'is-outdated': rowAgentOutdated(n) }]"
                    :title="rowAgentTitle(n)"
                  > · agent {{ n.agent_version || '未上报' }}</span>
                </div>
                <!-- 还没上报指标（agent 刚开始跑/刚上线）时，agent 版本仍要能看见 -->
                <div v-else-if="n.driver_type === 'xray'" class="an-metrics">
                  <span
                    :class="['an-agent-ver', { 'is-outdated': rowAgentOutdated(n) }]"
                    :title="rowAgentTitle(n)"
                  >agent {{ n.agent_version || '未上报' }}</span>
                </div>
              </td>
              <td class="mono">{{ n.latency > 0 ? `${n.latency}ms` : '—' }}</td>
              <td class="mono">{{ n.inbounds?.vless?.join(', ') || '—' }}</td>
              <td class="mono">{{ n.inbounds?.trojan?.join(', ') || '—' }}</td>
              <td class="mono">{{ n.inbounds?.hysteria?.join(', ') || '—' }}</td>
              <td>
                <div class="an-actions">
                  <Button variant="ghost" size="sm" :loading="testingId === n.id" @click="test(n)"><Icon name="activity" :size="14" /></Button>
                  <Button variant="ghost" size="sm" @click="openEdit(n)"><Icon name="edit" :size="14" /></Button>
                  <Button variant="ghost" size="sm" @click="remove(n)"><Icon name="trash" :size="14" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="8" class="an-empty">
                加载失败：{{ loadError }}
                <Button variant="secondary" size="sm" @click="load">重试</Button>
              </td>
            </tr>
            <tr v-else-if="!nodes.length">
              <td colspan="8" class="an-empty">
                <Skeleton v-if="loading" :rows="5" height="32px" />
                <template v-else>暂无服务器</template>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Pagination
      :page="pagination.current_page"
      :last-page="pagination.last_page"
      :total="pagination.total"
      :loading="loading"
      @prev="prev"
      @next="next"
    />

    <Modal v-model="modalOpen" :title="editing ? '编辑服务器' : '新建服务器'" width="560px">
      <div class="an-form">
        <div class="an-grid">
          <label class="an-field">
            <span>服务器类型</span>
            <Select v-model="form.driver_type" :options="driverTypeOptions" :disabled="!!editing" />
          </label>
          <label class="an-field">
            <span>名称</span>
            <Input v-model="form.name" :placeholder="isXrayForm ? '如 HK-01' : '选择入站后自动生成'" />
          </label>
        </div>

        <!-- ── xray 裸内核节点：一条安装命令装完，agent 自动注册 ── -->
        <template v-if="isXrayForm">
          <label class="an-field">
            <span>服务器地址（VPS 公网 IP / 域名，用户连接用）</span>
            <Input v-model="form.host" mono placeholder="1.2.3.4" />
          </label>
          <label class="an-field">
            <span>服务器 IPv6 地址（可选，绑 IPv6 的入站用它生成订阅）</span>
            <Input v-model="form.host_v6" mono placeholder="2602:fa4f::1 或 v6.example.com" />
          </label>
          <p class="an-hint">
            填独立的 IPv6 地址或只解析 AAAA 的域名，不要与上面的地址共用同一个域名
            （xray 会随机抽 IP）。入站的「地址族」选 IPv6 时，订阅链接改用这里填的地址。
          </p>

          <template v-if="editing && xrayInfo">
            <label class="an-field">
              <span>安装命令（在新 VPS 上以 root 粘贴执行，装完自动注册上线）</span>
              <textarea class="an-cmd" readonly rows="3" :value="xrayInfo.install_command"></textarea>
            </label>
            <div>
              <CopyButton :text="xrayInfo.install_command" label="复制安装命令" />
            </div>
            <div class="an-xray-meta">
              <span>内核 {{ xrayInfo.xray_version || '未上报' }}</span>
              <span>{{ xrayInfo.ws_connected ? 'WS 长连接' : 'HTTP 降级通道' }}</span>
              <span>{{ xrayInfo.has_inbounds ? '已配置入站' : '未建入站' }}</span>
              <span>agent {{ xrayInfo.agent_version || '未上报' }}</span>
              <span v-if="xrayInfo.lag_seconds > 60">配置滞后 {{ xrayInfo.lag_seconds }}s</span>
            </div>
            <!-- 密钥单独一行：它是 40 位定长串，和上面四项挤一行会被截成「VpM2…」看不全。
                 能复制完整值（仅详情接口返回全文；列表接口仍只给前 4 位）。 -->
            <label class="an-field">
              <span>节点密钥（安装命令里已带，需要单独拷贝时用这条）</span>
              <div class="an-secret">
                <Input :model-value="xrayInfo.node_secret || xrayInfo.node_secret_prefix || '—'" mono readonly />
                <CopyButton
                  v-if="xrayInfo.node_secret"
                  :text="xrayInfo.node_secret"
                  label="复制密钥"
                />
              </div>
            </label>
            <!-- agent 自升级：面板上这份二进制比节点新时才提示；点一下只入队指令，节点自己换并重启 -->
            <div v-if="agentOutdated" class="an-agent-upgrade">
              <span>面板上已有新版 agent {{ xrayInfo.agent_latest }}，升级不会中断用户连接</span>
              <Button variant="secondary" size="sm" :loading="upgradingAgent" @click="upgradeAgent">
                升级 agent
              </Button>
            </div>
            <p v-else-if="xrayInfo.agent_upgrade_pending" class="an-muted">
              已下发升级指令，等待服务器取走执行（服务器离线时会在上线后自动执行）。
            </p>
            <div v-if="xrayInfo.agent" class="an-xray-meta">
              <span>CPU {{ xrayInfo.agent.cpu != null ? xrayInfo.agent.cpu.toFixed(1) + '%' : '—' }}</span>
              <span>内存 {{ xrayInfo.agent.mem != null ? xrayInfo.agent.mem.toFixed(1) + '%' : '—' }}</span>
              <span>连接 {{ xrayInfo.agent.conns ?? 0 }}</span>
              <span>活跃用户 {{ xrayInfo.agent.active_users ?? 0 }}</span>
              <span>↑ {{ fmtSpeed(xrayInfo.agent.up_speed) }} · ↓ {{ fmtSpeed(xrayInfo.agent.down_speed) }}</span>
            </div>
            <!-- 系统 TCP BBR（issue #94 R4/R5）：状态来自心跳，按钮只下发 sysctl_apply。
                 HY2 的 QUIC BBR 在入站表单里，不在这里。 -->
            <div class="an-bbr">
              <div class="an-xray-meta">
                <span>拥塞控制 {{ bbrText.congestion }}</span>
                <span>队列规则 {{ bbrText.qdisc }}</span>
                <span>tcp_bbr 模块 {{ bbrText.module }}</span>
                <span>conntrack 上限 {{ bbrText.conntrack }}</span>
              </div>
              <Button
                variant="secondary"
                size="sm"
                :loading="enablingBBR"
                :disabled="enablingBBR"
                @click="enableBBR"
              >
                {{ bbrEnabled ? '重新应用 BBR' : '开启 BBR' }}
              </Button>
            </div>
            <p class="an-hint">
              开启 BBR 只影响系统 TCP（vless/vmess/trojan/ss）；HY2 走 QUIC，它的拥塞控制
              在入站表单的「拥塞控制」里选。参数写入节点
              /etc/sysctl.d/99-controlhub-tuning.conf，回执显示的是读回的实际生效值。
            </p>
          </template>
          <p v-else class="an-muted">保存后此处显示安装命令。新 VPS 粘贴执行即自动注册上线（GitHub 零依赖）。</p>
        </template>

        <!-- ── 3x-ui 面板节点 ── -->
        <template v-else>
          <label class="an-field an-panel-url">
            <span>面板地址（粘贴完整 URL 自动解析，失焦后填充下方字段）</span>
            <div class="an-panel-url-row">
              <Input v-model="panelUrl" mono placeholder="https://your-panel.com:port/path/" @blur="parsePanelUrl" />
              <Button variant="secondary" size="sm" @click="parsePanelUrl">解析</Button>
            </div>
          </label>

          <div class="an-grid">
            <label class="an-field">
              <span>主机</span>
              <Input v-model="form.host" placeholder="hk.example.com" />
            </label>
            <label class="an-field">
              <span>端口</span>
              <Input v-model="form.port" type="number" mono />
            </label>
            <label class="an-field">
              <span>协议</span>
              <Select v-model="form.scheme" :options="schemeOptions" />
            </label>
          </div>
          <label class="an-field">
            <span>webBasePath（面板路径前缀，如 /vNWY...，无则留空）</span>
            <Input v-model="form.web_base_path" mono placeholder="/your-path（无则留空）" />
          </label>
          <div class="an-grid">
            <label class="an-field">
              <span>面板用户名</span>
              <Input v-model="form.username" />
            </label>
            <label class="an-field">
              <span>面板密码{{ editing ? '（留空不改）' : '' }}</span>
              <Input v-model="form.password" type="password" />
            </label>
          </div>
          <label class="an-field">
            <span>API Token（Bearer，推荐）</span>
            <Input v-model="form.api_key" mono />
          </label>
          <div class="an-grid">
            <label v-for="proto in BIND_PROTOCOLS" :key="proto" class="an-field">
              <span>{{ BIND_LABELS[proto] }} 入站（可多选）</span>
              <div class="an-checkbox-list">
                <label v-for="o in inboundOptions[proto]" :key="o.id" class="an-checkbox-item">
                  <input type="checkbox" :checked="form.inbounds[proto].includes(o.id)" @change="toggleInbound(proto, o.id)" />
                  <span>{{ o.label }} [id:{{ o.id }}]</span>
                </label>
                <span v-if="!inboundOptions[proto].length" class="an-muted">请先拉取入站列表</span>
              </div>
            </label>
          </div>
          <Button variant="secondary" size="sm" :loading="probing" class="an-probe-btn" @click="fetchInbounds">
            <Icon name="refresh" :size="14" /> 拉取入站列表
          </Button>
          <label class="an-field an-field-row">
            <input v-model="form.verify_ssl" type="checkbox" />
            <span>校验 SSL 证书（自签/证书链不全时关闭）</span>
          </label>
        </template>

        <label class="an-field an-field-row">
          <input v-model="form.enabled" type="checkbox" />
          <span>启用</span>
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="modalOpen = false">取消</Button>
        <Button variant="primary" size="sm" :success="saved" @click="save">保存</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.an-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-4);
}
.an-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.an-table-wrap {
  overflow-x: auto;
}
.an-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.an-table th,
.an-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
.an-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.an-table tbody tr:hover {
  background: var(--bg-hover);
}
.an-actions {
  display: flex;
  gap: var(--space-1);
}
.an-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
.an-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.an-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-3);
}
/* 窄屏（手机）：表单两列并排会把控件压到只剩几十像素，一律单列。 */
@media (max-width: 767px) {
  .an-grid,
  .an-grid-3 {
    grid-template-columns: 1fr;
  }
}
.an-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.an-panel-url-row {
  display: flex;
  gap: var(--space-2);
}
.an-panel-url-row :deep(.ch-input) {
  flex: 1;
}
.an-field-row {
  flex-direction: row;
  align-items: center;
}

.an-probe-btn {
  align-self: flex-start;
}
.an-checkbox-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  max-height: 200px;
  overflow-y: auto;
  padding: var(--space-2);
  background: var(--bg-input);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
}
.an-checkbox-item {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-xs);
  cursor: pointer;
}
.an-checkbox-item input[type="checkbox"] {
  cursor: pointer;
}
.an-muted {
  color: var(--text-muted);
  font-size: var(--text-xs);
}
.an-metrics {
  margin-top: 2px;
  font-size: var(--text-xs);
  color: var(--text-muted);
  white-space: nowrap;
}
.an-cmd {
  width: 100%;
  padding: var(--space-2);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--text-secondary);
  background: var(--bg-input);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  resize: vertical;
}
/* 密钥行：输入框占满剩余宽度，复制按钮固定宽度不换行 */
.an-secret {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.an-secret > :first-child {
  flex: 1;
  min-width: 0;
}
.an-xray-meta {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  color: var(--text-muted);
  font-size: var(--text-xs);
}
/* agent 有新版的提示行：文字 + 按钮并排，随内容换行 */
.an-agent-upgrade {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-3);
  color: var(--text-secondary);
  font-size: var(--text-xs);
}
/* 系统调优（BBR）：四项状态 + 「开启 BBR」按钮同排，窄屏换行 */
.an-bbr {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}
.mono {
  font-family: var(--font-mono);
}

/* 表单栅格对齐：标签统一占两行高度，长标签换行时不会把输入框挤下去 */
.an-grid { align-items: start; }
.an-grid > .an-field > span:first-child { display: block; min-height: 2.6em; line-height: 1.35; }
</style>
