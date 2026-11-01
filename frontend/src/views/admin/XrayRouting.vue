<script setup>
/**
 * xray 节点配置 · 路由管理。
 *
 * 内置基础规则（阻私有网段 / 阻 BT）由系统打头，这里管理自定义分流规则：
 * 按「域名 / IP / 端口 / 网络 / 协议 / 入站」把流量导向指定出站（或负载均衡器）。
 * 另含：负载均衡器（balancer）管理区、基础设置（domainStrategy / domainMatcher），
 * 以及路由测试器（输入域名或 IP，按渲染口径逐条模拟匹配）。
 */
import { ref, watch, computed } from 'vue'
import { useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import XrayNodePicker from '@/components/admin/XrayNodePicker.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const router = useRouter()
const nodeId = ref(null)
const rules = ref([])
const balancers = ref([])
const outboundTags = ref([])      // 规则目标候选：出站（含中转出站）+ balancer
const outboundOnlyTags = ref([])  // balancer selector / fallback 候选：纯出站
const inboundTags = ref([])       // 来源入站候选：{value,tag,port,protocol,label}
const settings = ref({ domainStrategy: 'AsIs', domainMatcher: '' })
const strategyOptions = ref([])
const domainStrategyOptions = ref([])
const domainMatcherOptions = ref([])
const loading = ref(false)

// 实际下发预览（含渲染层自动护栏；issue #37）
const previewOpen = ref(false)
const renderPreview = ref([])

const modalOpen = ref(false)
const editing = ref(null)
const saving = ref(false)
const saved = ref(false)

// 基础设置
const settingsSaving = ref(false)
const settingsSaved = ref(false)

// 负载均衡器
const balancerModalOpen = ref(false)
const editingBalancer = ref(null)
const balancerSaving = ref(false)
const balancerSaved = ref(false)
const emptyBalancer = () => ({
  tag: '',
  selectors: '',
  strategy: 'roundRobin',
  fallback_tag: '',
  remark: '',
  enabled: true,
})
const balancerForm = ref(emptyBalancer())

// 路由测试器
const testerOpen = ref(false)
const testerDomain = ref('')
const testerIp = ref('')
const testerBusy = ref(false)
const testerResult = ref(null)

// 出站延时（规则表「延时」列）：tag → {state, latencyMs, error}
//   state: idle | testing | ok | fail | skipped
// 按 tag 存而不是按规则行存：同一出站常被多条规则引用，测一次全列同时更新。
const latencyByTag = ref({})
const latencyAllBusy = ref(false)

const emptyForm = () => ({
  domains: '',
  ips: '',
  port: '',
  network: '',
  protocol: '',
  inbound_tag: '',
  outbound_tag: 'direct',
  remark: '',
  enabled: true,
})
const form = ref(emptyForm())

const networkOptions = [{ label: '不限', value: '' }, { label: 'TCP', value: 'tcp' }, { label: 'UDP', value: 'udp' }]
const protocolOptions = [
  { label: '不限', value: '' },
  { label: 'HTTP', value: 'http' },
  { label: 'TLS', value: 'tls' },
  { label: 'BitTorrent', value: 'bittorrent' },
]
const strategyLabels = {
  random: '随机（Random）',
  roundRobin: '轮询（Round robin）',
  leastPing: '最低延迟（Least ping，需观测器）',
  leastLoad: '最低负载（Least load，需观测器）',
}
const domainStrategyLabels = {
  AsIs: 'AsIs（不解析域名）',
  IPIfNonMatch: 'IPIfNonMatch（未命中则解析为 IP）',
  IPOnDemand: 'IPOnDemand（遇 IP 规则才解析）',
}
const domainMatcherLabels = { '': '默认（内核选择）', mph: 'mph（最小完美哈希）', hybrid: 'hybrid' }

const strategySelectOptions = computed(() =>
  (strategyOptions.value || []).map((v) => ({ label: strategyLabels[v] || v, value: v })),
)
const domainStrategySelectOptions = computed(() =>
  (domainStrategyOptions.value || []).map((v) => ({ label: domainStrategyLabels[v] || v, value: v })),
)
const domainMatcherSelectOptions = computed(() =>
  (domainMatcherOptions.value || []).map((v) => ({ label: domainMatcherLabels[v] ?? v, value: v })),
)
const balancerTagSet = computed(() => new Set(balancers.value.map((b) => b.tag)))
const fallbackOptions = computed(() => [
  { label: '（无）', value: '' },
  ...outboundOnlyTags.value.map((t) => ({ label: t.tag, value: t.tag })),
])

// 来源入站候选：显示「端口 · 协议 · tag」，value 仍为 xray tag
const inboundOptions = computed(() => [
  { label: '不限', value: '' },
  ...inboundTags.value.map((t) => ({ label: t.label || t.tag || t.value, value: t.value ?? t.tag })),
])

// 目标出站候选：只列本节点已有出站（含中转出站）+ balancer。
// 「本站服务器」中转出口由出站页创建，这里不生成 relay 候选，只提供快捷跳转入口。
const outboundOptions = computed(() =>
  outboundTags.value.map((t) => ({ label: `${t.tag}（${t.label}）`, value: t.tag })),
)

// 「＋ 新建中转出口」快捷入口：跳到出站页并预填「目标来源 = 本站服务器」。
// 预填用户当前在下拉里选中的服务器/入站——但下拉现在只有裸 tag（无服务器/入站信息），
// 拿不到就只带目标来源，不预填。语义仍是「出口由出站页创建」，此处仅快捷跳转。
const newRelayOutbound = () => {
  const q = { tab: 'outbounds', node: nodeId.value, relay: 1 }
  // 出站页现在是「Xray 高级配置」容器里的一个 Tab，走容器路径 + tab 参数；
  // relay=1 继续带上（出站页靠它决定是否展开中转出口）
  router.push({ path: '/admin/nodes/xray', query: q })
}

// 规则列表「目标出站」显示名：中转出站（后端带 relay 标记）显示名字，避免裸 tag 认不出是谁的服务器
const outboundLabel = (tag) => {
  const t = outboundTags.value.find((x) => x.tag === tag)
  return t && t.relay && t.label ? t.label : tag
}

// selector 当前命中的出站（前缀匹配，与 xray 口径一致）
const selectorHits = (sel) =>
  outboundOnlyTags.value.map((t) => t.tag).filter((t) => t.startsWith(sel))
const balancerTargets = (b) =>
  (b.selectors || []).flatMap((s) => selectorHits(s)).filter((v, i, a) => a.indexOf(v) === i)

const strategyLabel = (v) => strategyLabels[v] || v
const isBalancerTag = (tag) => balancerTagSet.value.has(tag)

// 目标出站是否 socks/http（xray 不支持 UDP；渲染层会自动注入 udp→direct 护栏，issue #37）
const outboundProtocol = (tag) => outboundTags.value.find((x) => x.tag === tag)?.protocol || ''
const isTcpOnlyTarget = (tag) => ['socks', 'http'].includes(outboundProtocol(tag))
// 规则会命中 UDP 且目标是 TCP-only 出站（命中时自动护栏生效，界面标注「UDP→直连」）
const udpGuardHit = (r) => isTcpOnlyTarget(r.outbound_tag) && (!r.network || r.network === 'udp')

const load = async () => {
  // 延时结果属于「这一次读取的这份出站定义」：切节点或出站被改过就该作废，
  // 否则旧延时会被当成本节点的实测值展示。
  latencyByTag.value = {}
  latencyAllBusy.value = false
  if (!nodeId.value) {
    rules.value = []
    balancers.value = []
    outboundTags.value = []
    outboundOnlyTags.value = []
    inboundTags.value = []
    renderPreview.value = []
    return
  }
  loading.value = true
  try {
    const res = await adminApi.listXrayRouting(nodeId.value)
    rules.value = res?.rules || []
    balancers.value = res?.balancers || []
    outboundTags.value = res?.outbound_tags || []
    outboundOnlyTags.value = res?.outbound_only_tags || res?.outbound_tags || []
    inboundTags.value = res?.inbounds || []
    renderPreview.value = res?.render_preview?.rules || []
    settings.value = res?.settings || { domainStrategy: 'AsIs', domainMatcher: '' }
    strategyOptions.value = res?.strategies || ['random', 'roundRobin', 'leastPing', 'leastLoad']
    domainStrategyOptions.value = res?.domain_strategies || ['AsIs', 'IPIfNonMatch', 'IPOnDemand']
    domainMatcherOptions.value = res?.domain_matchers || ['', 'mph', 'hybrid']
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}
watch(nodeId, load)

const saveSettings = async () => {
  if (!nodeId.value) return
  settingsSaving.value = true
  try {
    await adminApi.updateXrayRoutingSettings(nodeId.value, {
      domain_strategy: settings.value.domainStrategy || 'AsIs',
      domain_matcher: settings.value.domainMatcher || '',
    })
    toast.success('基础设置已保存（节点将秒级生效）')
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段）
    settingsSaved.value = true
    setTimeout(() => { settingsSaved.value = false }, 900)
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    settingsSaving.value = false
  }
}

const openCreate = () => {
  editing.value = null
  form.value = emptyForm()
  modalOpen.value = true
}

const openEdit = (r) => {
  editing.value = r
  form.value = {
    domains: (r.domains || []).join('\n'),
    ips: (r.ips || []).join('\n'),
    port: r.port || '',
    network: r.network || '',
    protocol: r.protocol || '',
    inbound_tag: r.inbound_tag || '',
    outbound_tag: r.outbound_tag || 'direct',
    remark: r.remark || '',
    enabled: r.enabled,
  }
  modalOpen.value = true
}

const splitLines = (s) =>
  String(s || '')
    .split(/[\n,，]+/)
    .map((x) => x.trim())
    .filter(Boolean)

const save = async () => {
  if (!nodeId.value) return
  saving.value = true
  try {
    const payload = {
      domains: splitLines(form.value.domains),
      ips: splitLines(form.value.ips),
      port: form.value.port || null,
      network: form.value.network || null,
      protocol: form.value.protocol || null,
      inbound_tag: form.value.inbound_tag || null,
      outbound_tag: form.value.outbound_tag,
      remark: form.value.remark || null,
      enabled: form.value.enabled,
    }
    if (editing.value) {
      const res = await adminApi.updateXrayRoutingRule(nodeId.value, editing.value.id, payload)
      toast.success('规则已更新（节点将秒级生效）')
      if (res?.warning) toast.info(res.warning, 8000)
    } else {
      const res = await adminApi.createXrayRoutingRule(nodeId.value, payload)
      toast.success('规则已创建（节点将秒级生效）')
      if (res?.warning) toast.info(res.warning, 8000)
    }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    saved.value = true
    setTimeout(() => {
      saved.value = false
      modalOpen.value = false
    }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const remove = async (r) => {
  if (!confirm(`删除规则「${r.remark || r.outbound_tag}」？`)) return
  try {
    await adminApi.deleteXrayRoutingRule(nodeId.value, r.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

// ── 负载均衡器 ──
const openBalancerCreate = () => {
  editingBalancer.value = null
  balancerForm.value = emptyBalancer()
  balancerModalOpen.value = true
}

const openBalancerEdit = (b) => {
  editingBalancer.value = b
  balancerForm.value = {
    tag: b.tag || '',
    selectors: (b.selectors || []).join('\n'),
    strategy: b.strategy || 'roundRobin',
    fallback_tag: b.fallback_tag || '',
    remark: b.remark || '',
    enabled: b.enabled,
  }
  balancerModalOpen.value = true
}

const saveBalancer = async () => {
  if (!nodeId.value) return
  const selectors = splitLines(balancerForm.value.selectors)
  if (!balancerForm.value.tag.trim()) {
    toast.error('请填写 tag')
    return
  }
  if (!selectors.length) {
    toast.error('至少需要一个 selector')
    return
  }
  balancerSaving.value = true
  try {
    const payload = {
      tag: balancerForm.value.tag.trim(),
      selectors,
      strategy: balancerForm.value.strategy,
      fallback_tag: balancerForm.value.fallback_tag || null,
      remark: balancerForm.value.remark || null,
      enabled: balancerForm.value.enabled,
    }
    if (editingBalancer.value) {
      await adminApi.updateXrayBalancer(nodeId.value, editingBalancer.value.id, payload)
      toast.success('负载均衡已更新（节点将秒级生效）')
    } else {
      await adminApi.createXrayBalancer(nodeId.value, payload)
      toast.success('负载均衡已创建（节点将秒级生效）')
    }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    balancerSaved.value = true
    setTimeout(() => {
      balancerSaved.value = false
      balancerModalOpen.value = false
    }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    balancerSaving.value = false
  }
}

const removeBalancer = async (b) => {
  if (!confirm(`删除负载均衡「${b.tag}」？引用它的路由规则将变为无效目标。`)) return
  try {
    await adminApi.deleteXrayBalancer(nodeId.value, b.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

const runTest = async () => {
  if (!testerDomain.value && !testerIp.value) {
    toast.error('请输入域名或 IP')
    return
  }
  testerBusy.value = true
  testerResult.value = null
  try {
    const res = await adminApi.testXrayRouting(nodeId.value, {
      domain: testerDomain.value || null,
      ip: testerIp.value || null,
    })
    testerResult.value = res
  } catch (e) {
    toast.error(e.message || '测试失败')
  } finally {
    testerBusy.value = false
  }
}

// ── 出站延时（规则表「延时」列）────────────────────────────────
//
// 只测「真实出站」的目标地址：内置 direct/blocked、负载均衡 tag、UDP 出站
// （wireguard/WARP）后端一律不测，回执里 skipped=true，界面显示「—」。
// 每行按该规则的 outbound_tag 取结果；balancer 规则的成员出站由后端各自测。

const latencyEntry = (tag) => latencyByTag.value[tag] || { state: 'idle' }

// 该行能不能测：内置直连/拦截永远不测（后端也不会下发给它们）。
const latencyTestable = (r) => {
  const tag = r.outbound_tag
  if (!tag || tag === 'direct' || tag === 'blocked') return false
  const st = latencyEntry(tag).state
  return st !== 'testing' && st !== 'skipped'
}

const latencyLabel = (r) => {
  const e = latencyEntry(r.outbound_tag)
  if (e.state === 'testing') return '…'
  if (e.state === 'ok' && e.latencyMs != null) return `${e.latencyMs}ms`
  if (e.state === 'fail') return '超时'
  return '—'
}

const latencyClass = (r) => `an-latency an-latency--${latencyEntry(r.outbound_tag).state}`

const latencyTitle = (r) => {
  const e = latencyEntry(r.outbound_tag)
  if (e.state === 'ok') return `TCP 握手 ${e.latencyMs}ms`
  if (e.state === 'skipped') return e.error || '该目标不支持延时测试'
  if (e.state === 'fail') return e.error || '不可达'
  if (e.state === 'testing') return '测试中…'
  return '点击测试该出站的连通延时'
}

// 把后端回执写回按 tag 的结果表；未返回的 tag 保持原样（不清空别的行）。
const applyLatencyResults = (results) => {
  if (!Array.isArray(results)) return
  const next = { ...latencyByTag.value }
  for (const row of results) {
    if (!row || !row.tag) continue
    if (row.skipped) {
      next[row.tag] = { state: 'skipped', error: row.error || '' }
    } else if (row.ok) {
      next[row.tag] = { state: 'ok', latencyMs: row.latency_ms }
    } else {
      next[row.tag] = { state: 'fail', error: row.error || '不可达' }
    }
  }
  latencyByTag.value = next
}

// 测一个 tag（行按钮）。server/port 由后端按 tag 补齐，前端不用知道。
const testLatency = async (r) => {
  const tag = r.outbound_tag
  if (!nodeId.value || !latencyTestable(r)) return
  latencyByTag.value = { ...latencyByTag.value, [tag]: { state: 'testing' } }
  try {
    const res = await adminApi.testXrayRoutingOutbounds(nodeId.value, {
      node_id: nodeId.value,
      targets: [{ tag }],
    })
    if (res?.pending) {
      toast.warning(res.message || '节点未及时回执，请稍后重试')
      latencyByTag.value = { ...latencyByTag.value, [tag]: { state: 'idle' } }
      return
    }
    if (res?.error) toast.warning(res.error, 6000)
    applyLatencyResults(res?.results)
  } catch (e) {
    toast.error(e.message || '延时测试失败')
    latencyByTag.value = { ...latencyByTag.value, [tag]: { state: 'idle' } }
  }
}

// 「全部测试」：一次下发本节点全部可测出站（后端按 node 汇总），回填整列。
const testAllLatency = async () => {
  if (!nodeId.value || latencyAllBusy.value) return
  const tags = [...new Set(rules.value.map((r) => r.outbound_tag))].filter(
    (t) => t && t !== 'direct' && t !== 'blocked',
  )
  if (!tags.length) {
    toast.info('当前没有可测试的出站')
    return
  }
  latencyAllBusy.value = true
  const next = { ...latencyByTag.value }
  tags.forEach((t) => { next[t] = { state: 'testing' } })
  latencyByTag.value = next
  try {
    // targets 传 tag 列表即可：server/port 由后端按出站定义补齐，
    // 也不会有前端拿到过期地址的问题。
    const res = await adminApi.testXrayRoutingOutbounds(nodeId.value, {
      node_id: nodeId.value,
      targets: tags.map((tag) => ({ tag })),
    })
    if (res?.pending) {
      toast.warning(res.message || '节点未及时回执，请稍后重试')
      const revert = { ...latencyByTag.value }
      tags.forEach((t) => { revert[t] = { state: 'idle' } })
      latencyByTag.value = revert
      return
    }
    if (res?.error) toast.warning(res.error, 6000)
    applyLatencyResults(res?.results)
  } catch (e) {
    toast.error(e.message || '延时测试失败')
    const revert = { ...latencyByTag.value }
    tags.forEach((t) => { revert[t] = { state: 'idle' } })
    latencyByTag.value = revert
  } finally {
    latencyAllBusy.value = false
  }
}

const conditionSummary = (r) => {
  const parts = []
  if (r.domains?.length) parts.push(`域名 ${r.domains.length} 条`)
  if (r.ips?.length) parts.push(`IP ${r.ips.length} 条`)
  if (r.port) parts.push(`端口 ${r.port}`)
  if (r.network) parts.push(r.network)
  if (r.protocol) parts.push(r.protocol)
  if (r.inbound_tag) parts.push(`入站 ${r.inbound_tag}`)
  return parts.join(' · ') || '—'
}
</script>

<template>
  <div class="an">
    <div class="an-head">
      <h1 class="an-title">xray 路由</h1>
      <div class="an-head-actions">
        <Button
          variant="secondary"
          size="sm"
          :disabled="!nodeId"
          :loading="latencyAllBusy"
          title="向节点下发 outbound_test，对每条规则的目标出站做真实 TCP 握手"
          @click="testAllLatency"
        >
          <Icon name="traffic" :size="16" /> 全部测试
        </Button>
        <Button variant="secondary" size="sm" :disabled="!nodeId" @click="testerOpen = true">
          <Icon name="search" :size="16" /> 路由测试
        </Button>

        <Button variant="primary" size="sm" :disabled="!nodeId" @click="openCreate">
          <Icon name="plus" :size="16" /> 新建规则
        </Button>
      </div>
    </div>
    <p class="an-desc">
      按「域名 / IP / 端口 / 协议」把流量导向指定出站。系统已内置「阻私有网段 / 阻 BT」基础规则，无需手动添加。
    </p>

    <div class="an-toolbar">
      <XrayNodePicker v-model="nodeId" />
    </div>

    <!-- 基础设置 -->
    <Card v-if="nodeId" class="an-block">
      <div class="an-block-head">
        <h3 class="an-subtitle">基础设置</h3>
        <Button variant="secondary" size="sm" :loading="settingsSaving" :success="settingsSaved" @click="saveSettings">保存</Button>
      </div>
      <div class="an-grid">
        <label class="an-field">
          <span>域名解析策略（domainStrategy）</span>
          <Select v-model="settings.domainStrategy" :options="domainStrategySelectOptions" />
        </label>
        <label class="an-field">
          <span>域名匹配器（domainMatcher）</span>
          <Select v-model="settings.domainMatcher" :options="domainMatcherSelectOptions" />
        </label>
      </div>
      <p class="an-muted-sm">
        IPIfNonMatch / IPOnDemand 会把域名解析为 IP 后再匹配 IP 规则（依赖节点 DNS）；不确定时保持 AsIs。
      </p>
    </Card>

    <!-- 负载均衡 -->
    <Card v-if="nodeId" class="an-block">
      <div class="an-block-head">
        <div>
          <h3 class="an-subtitle">负载均衡</h3>
          <p class="an-muted-sm an-mb0">
            按出站 tag 前缀（selector）把流量分摊到多个出站。leastPing / leastLoad 依赖内核观测器，系统已自动补齐。
          </p>
        </div>
        <Button variant="primary" size="sm" @click="openBalancerCreate">
          <Icon name="plus" :size="16" /> 新建负载均衡
        </Button>
      </div>
      <div class="an-table-wrap">
        <table class="an-table">
          <thead>
            <tr>
              <th>tag</th>
              <th>选择器</th>
              <th>命中出站</th>
              <th>策略</th>
              <th>回退</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="b in balancers" :key="b.id">
              <td><Badge variant="info">{{ b.tag }}</Badge></td>
              <td class="mono an-clip">{{ (b.selectors || []).join(', ') }}</td>
              <td class="mono an-clip">{{ balancerTargets(b).join(', ') || '—' }}</td>
              <td>{{ strategyLabel(b.strategy) }}</td>
              <td class="mono">{{ b.fallback_tag || '—' }}</td>
              <td><Badge :variant="b.enabled ? 'success' : 'warning'" dot>{{ b.enabled ? '启用' : '停用' }}</Badge></td>
              <td>
                <div class="an-actions">
                  <Button size="sm" variant="ghost" @click="openBalancerEdit(b)"><Icon name="edit" :size="16" /></Button>
                  <Button size="sm" variant="ghost" @click="removeBalancer(b)"><Icon name="trash" :size="16" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!loading && balancers.length === 0">
              <td colspan="7" class="an-empty">
                还没有负载均衡器。出站 tag 用统一前缀（如 hk-1 / hk-2）时，可用 selector「hk-」一键分摊。
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Card padding="false">
      <div class="an-table-wrap">
        <table class="an-table">
          <thead>
            <tr>
              <th>备注</th>
              <th>匹配条件</th>
              <th>域名</th>
              <th>IP</th>
              <th>目标出站</th>
              <th>延时</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in rules" :key="r.id">
              <td>{{ r.remark || '—' }}</td>
              <td class="an-muted-sm">{{ conditionSummary(r) }}</td>
              <td class="mono an-clip">{{ (r.domains || []).join(', ') || '—' }}</td>
              <td class="mono an-clip">{{ (r.ips || []).join(', ') || '—' }}</td>
              <td class="an-target-cell">
                <Badge variant="info">{{ outboundLabel(r.outbound_tag) }}</Badge>
                <Badge
                  v-if="udpGuardHit(r)"
                  variant="warning"
                  title="socks/http 出站不支持 UDP：该规则命中的 UDP 将自动回落直连"
                >UDP→直连</Badge>
              </td>
              <td>
                <button
                  type="button"
                  :class="latencyClass(r)"
                  :title="latencyTitle(r)"
                  :disabled="!nodeId || !latencyTestable(r)"
                  @click="testLatency(r)"
                >{{ latencyLabel(r) }}</button>
              </td>
              <td><Badge :variant="r.enabled ? 'success' : 'warning'" dot>{{ r.enabled ? '启用' : '停用' }}</Badge></td>
              <td>
                <div class="an-actions">
                  <Button size="sm" variant="ghost" @click="openEdit(r)"><Icon name="edit" :size="16" /></Button>
                  <Button size="sm" variant="ghost" @click="remove(r)"><Icon name="trash" :size="16" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!loading && rules.length === 0">
              <td colspan="8" class="an-empty">
                {{ nodeId ? '还没有自定义路由规则（默认全部走 direct 直连，私有网段/BT 已内置阻断）' : '请先在上方选择一个 xray 节点' }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 实际下发规则预览（含渲染层自动护栏；issue #37） -->
    <Card padding="false" class="an-preview">
      <button type="button" class="an-preview-toggle" @click="previewOpen = !previewOpen">
        <Icon :name="previewOpen ? 'chevron-up' : 'chevron-down'" :size="16" />
        实际下发规则预览（含自动护栏，共 {{ renderPreview.length }} 条）
      </button>
      <pre v-if="previewOpen" class="an-preview-body">{{ JSON.stringify(renderPreview, null, 2) }}</pre>
    </Card>

    <!-- 新建 / 编辑 -->
    <Modal v-model="modalOpen" :title="editing ? '编辑路由规则' : '新建路由规则'" width="560px">
      <div class="an-form">
        <label class="an-field">
          <span>域名（每行一条；支持 domain: / full: / keyword: / regexp: / geosite: 前缀）</span>
          <textarea v-model="form.domains" class="an-textarea" rows="3" placeholder="domain:example.com&#10;geosite:cn" />
        </label>
        <label class="an-field">
          <span>IP（每行一条；支持 geoip: 前缀与 CIDR）</span>
          <textarea v-model="form.ips" class="an-textarea" rows="2" placeholder="geoip:cn&#10;1.1.1.1/32" />
        </label>
        <div class="an-grid">
          <label class="an-field">
            <span>端口（空=不限；如 443 或 1000-2000）</span>
            <Input v-model="form.port" placeholder="可空" />
          </label>
          <label class="an-field">
            <span>网络</span>
            <Select v-model="form.network" :options="networkOptions" />
          </label>
        </div>
        <div class="an-grid">
          <label class="an-field">
            <span>协议（按嗅探结果）</span>
            <Select v-model="form.protocol" :options="protocolOptions" />
          </label>
          <label class="an-field">
            <span>来源入站（可空）</span>
            <Select v-model="form.inbound_tag" :options="inboundOptions" />
          </label>
        </div>
        <div class="an-grid">
          <label class="an-field">
            <span>目标出站 / 负载均衡</span>
            <Select v-model="form.outbound_tag" :options="outboundOptions" />
          </label>
          <label class="an-field">
            <span>备注</span>
            <Input v-model="form.remark" placeholder="如：国内直连" />
          </label>
        </div>
        <button type="button" class="an-relay-entry" @click="newRelayOutbound">
          <Icon name="plus" :size="14" /> 新建中转出口（本站服务器）
        </button>
        <label class="an-field-row">
          <input v-model="form.enabled" type="checkbox" />
          <span>启用</span>
        </label>
      </div>
      <template #footer>
        <Button variant="ghost" @click="modalOpen = false">取消</Button>
        <Button variant="primary" :loading="saving" :success="saved" @click="save">{{ editing ? '保存' : '创建' }}</Button>
      </template>
    </Modal>

    <!-- 新建 / 编辑 负载均衡 -->
    <Modal v-model="balancerModalOpen" :title="editingBalancer ? '编辑负载均衡' : '新建负载均衡'" width="560px">
      <div class="an-form">
        <div class="an-grid">
          <label class="an-field">
            <span>tag（唯一，路由规则用它引用）</span>
            <Input v-model="balancerForm.tag" placeholder="如 bal-hk" />
          </label>
          <label class="an-field">
            <span>策略</span>
            <Select v-model="balancerForm.strategy" :options="strategySelectOptions" />
          </label>
        </div>
        <label class="an-field">
          <span>选择器 selector（每行一条；xray 为前缀匹配，如 hk- 命中 hk-1/hk-2）</span>
          <textarea v-model="balancerForm.selectors" class="an-textarea" rows="2" placeholder="hk-" />
        </label>
        <div v-if="splitLines(balancerForm.selectors).length" class="an-hintbox">
          <div v-for="sel in splitLines(balancerForm.selectors)" :key="sel" class="an-muted-sm">
            「{{ sel }}」→ {{ selectorHits(sel).join(', ') || '无匹配出站（保存会被拒绝）' }}
          </div>
        </div>
        <div class="an-grid">
          <label class="an-field">
            <span>回退出站（可空；目标不可用时走它）</span>
            <Select v-model="balancerForm.fallback_tag" :options="fallbackOptions" />
          </label>
          <label class="an-field">
            <span>备注</span>
            <Input v-model="balancerForm.remark" placeholder="如：香港三线" />
          </label>
        </div>
        <label class="an-field-row">
          <input v-model="balancerForm.enabled" type="checkbox" />
          <span>启用</span>
        </label>
      </div>
      <template #footer>
        <Button variant="ghost" @click="balancerModalOpen = false">取消</Button>
        <Button variant="primary" :loading="balancerSaving" :success="balancerSaved" @click="saveBalancer">{{ editingBalancer ? '保存' : '创建' }}</Button>
      </template>
    </Modal>

    <!-- 路由测试器 -->
    <Modal v-model="testerOpen" title="路由测试（模拟匹配）" width="560px">
      <div class="an-form">
        <div class="an-grid">
          <label class="an-field">
            <span>域名</span>
            <Input v-model="testerDomain" placeholder="www.example.com" />
          </label>
          <label class="an-field">
            <span>IP</span>
            <Input v-model="testerIp" placeholder="1.2.3.4" />
          </label>
        </div>
        <Button variant="secondary" size="sm" :loading="testerBusy" @click="runTest">开始测试</Button>

        <div v-if="testerResult" class="an-result">
          <div class="an-result-line">
            最终出站：<Badge variant="info">{{ testerResult.outbound }}</Badge>
            <span class="an-muted-sm">
              （{{ testerResult.matched ? '命中规则' : '默认出口' }} · 共检查 {{ testerResult.rules_checked }} 条规则）
            </span>
          </div>
          <template v-if="testerResult.matched">
            <div class="an-result-line">
              命中规则 #{{ testerResult.matched.index }}：
              <span class="mono">{{ testerResult.matched.is_balancer ? 'balancerTag ' : 'outboundTag ' }}{{ testerResult.matched.outbound }}</span>
              <Badge v-if="testerResult.matched.is_balancer" variant="info">负载均衡</Badge>
            </div>
            <div v-for="(n, i) in testerResult.matched.notes" :key="i" class="an-muted-sm">· {{ n }}</div>
            <div v-if="testerResult.matched.is_balancer" class="an-muted-sm">注：负载均衡的最终出口由内核按策略实时选举，面板无法预知。</div>
          </template>
          <div v-else class="an-muted-sm">
            未命中任何规则，走默认出口
            <span class="mono">{{ testerResult.default_outbound || 'direct' }}</span>。
          </div>
          <div class="an-muted-sm">注：geoip:/geosite: 类规则需节点内核的 geo 数据，面板侧无法精确判定（已在上方提示）。</div>
        </div>
      </div>
      <template #footer>
        <Button variant="ghost" @click="testerOpen = false">关闭</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.an-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-2);
}
.an-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.an-head-actions {
  display: flex;
  gap: var(--space-2);
}
.an-desc {
  margin: 0 0 var(--space-3);
  color: var(--text-muted);
  font-size: var(--text-sm);
}
.an-toolbar {
  display: flex;
  align-items: center;
  margin-bottom: var(--space-3);
}
.an-block {
  margin-bottom: var(--space-4);
}
.an-block-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
  margin-bottom: var(--space-3);
}
.an-subtitle {
  margin: 0 0 var(--space-1);
  font-size: var(--text-md);
  font-weight: var(--font-semibold);
}
.an-mb0 {
  margin-bottom: 0;
}
.an-hintbox {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-2) var(--space-3);
  background: var(--bg-input);
  border-radius: var(--radius-md);
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
.an-clip {
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.an-actions {
  display: flex;
  gap: var(--space-1);
}
.an-target-cell {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  flex-wrap: wrap;
}
/* 延时列：本身就是按钮（点一下测这条），所以按「可点的小胶囊」做，
   而不是再塞一个图标按钮把列宽撑开。 */
.an-latency {
  min-width: 56px;
  padding: 2px 8px;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--bg-input);
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: 1.6;
  cursor: pointer;
  transition: border-color 0.15s var(--ease-out, ease), color 0.15s var(--ease-out, ease);
}
.an-latency:hover:not(:disabled) {
  border-color: var(--accent);
  color: var(--accent);
}
.an-latency:disabled {
  cursor: default;
  opacity: 0.6;
}
.an-latency--ok {
  border-color: var(--success);
  color: var(--success);
}
.an-latency--fail {
  border-color: var(--danger);
  color: var(--danger);
}
.an-latency--testing {
  border-style: dashed;
  color: var(--text-muted);
}
.an-preview {
  margin-top: var(--space-3);
}
.an-preview-toggle {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  width: 100%;
  padding: var(--space-3) var(--space-4);
  border: 0;
  background: none;
  color: var(--text-muted);
  font-size: var(--text-sm);
  cursor: pointer;
  text-align: left;
}
.an-preview-toggle:hover {
  color: var(--text-primary);
}
.an-preview-body {
  margin: 0;
  padding: 0 var(--space-4) var(--space-4);
  max-height: 320px;
  overflow: auto;
  font-size: var(--text-xs);
  line-height: 1.6;
  color: var(--text-muted);
  white-space: pre;
}
.an-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
  white-space: normal;
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
.an-field-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--text-secondary);
  cursor: pointer;
}
.an-textarea {
  width: 100%;
  padding: var(--space-2);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--bg-input);
  color: var(--text-primary);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  resize: vertical;
}
.an-muted-sm {
  color: var(--text-muted);
  font-size: var(--text-xs);
  white-space: normal;
}
.an-relay-entry {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  align-self: flex-start;
  padding: var(--space-2) var(--space-3);
  background: var(--bg-input);
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-md);
  color: var(--accent);
  font-size: var(--text-xs);
  cursor: pointer;
  transition: border-color 0.15s var(--ease-out, ease);
}
.an-relay-entry:hover {
  border-color: var(--accent);
}
.an-result {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-3);
  background: var(--bg-input);
  border-radius: var(--radius-md);
  font-size: var(--text-sm);
}
.an-result-line {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.mono {
  font-family: var(--font-mono);
}

/* 表单栅格对齐：标签统一占两行高度，长标签换行时不会把输入框挤下去 */
.an-grid { align-items: start; }
.an-grid > .an-field > span:first-child { display: block; min-height: 2.6em; line-height: 1.35; }
</style>
