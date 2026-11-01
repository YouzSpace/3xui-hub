<script setup>
/**
 * xray 节点配置 · 出站管理。
 *
 * 出站 = 节点流量的出口（直连 / 中转服务器 / WARP）。
 * - 结构化表单覆盖常用协议；「高级 JSON」兜底未覆盖参数
 * - URI 一键导入（vless/vmess/trojan/ss/socks/http 分享链接）
 * - 连通测试：下发指令由节点侧 TCP 探活（同步等结果）
 * - 流量列：agent 上报的出站快照（覆盖写）
 * - 页签「订阅」：出站订阅（服务器池）—— 拉取外部订阅 → 解析成出站 → 注入节点
 */
import { ref, computed, watch, onMounted } from 'vue'
import { useRoute } from 'vue-router'
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
const route = useRoute()
const nodeId = ref(null)
const outbounds = ref([])
const relayTargets = ref([])   // 「目标来源 = 本站服务器」候选：{node_id,inbound_id,label,protocol,supported,...}
const loading = ref(false)
const testingId = ref(null)

const modalOpen = ref(false)
const editing = ref(null)
const saving = ref(false)
const saved = ref(false)
const advancedOpen = ref(false)

const importOpen = ref(false)
const importLink = ref('')
const importPreview = ref(null)
const importing = ref(false)

// ── 订阅（服务器池）──
const tab = ref('outbounds') // outbounds | subscriptions
const subs = ref([])
const subsLoading = ref(false)
const subModalOpen = ref(false)
const subEditing = ref(null)
const subSaving = ref(false)
const subSaved = ref(false)
const refreshingId = ref(null)
const emptySubForm = () => ({
  remark: '',
  url: '',
  tag_prefix: '',
  update_interval: 600,
  prepend: false,
  enabled: true,
})
const subForm = ref(emptySubForm())

const protocolOptions = [
  { label: 'VMess', value: 'vmess' },
  { label: 'VLESS', value: 'vless' },
  { label: 'Trojan', value: 'trojan' },
  { label: 'Shadowsocks', value: 'shadowsocks' },
  { label: 'SOCKS5', value: 'socks' },
  { label: 'HTTP 代理', value: 'http' },
  { label: 'Freedom（直连）', value: 'freedom' },
  { label: 'Blackhole（拦截）', value: 'blackhole' },
  { label: 'WireGuard', value: 'wireguard' },
  { label: 'Hysteria2（QUIC 出口）', value: 'hysteria' },
  { label: 'DNS（域名解析出口）', value: 'dns' },
  { label: 'WARP（Cloudflare，自动注册）', value: 'warp' },
]
const NETWORK_OPTIONS_ALL = [
  { label: 'TCP', value: 'tcp' },
  { label: 'WebSocket', value: 'ws' },
  { label: 'gRPC', value: 'grpc' },
  { label: 'xhttp', value: 'xhttp' },
  { label: 'HTTPUpgrade', value: 'httpupgrade' },
]
const REALITY_NETWORKS = ['tcp', 'xhttp', 'grpc']
// dns 出站的查询传输（写进 settings.network，和传输层 network 无关）
const dnsNetworkOptions = [
  { label: 'UDP（常用，53）', value: 'udp' },
  { label: 'TCP（53）', value: 'tcp' },
]
// 传输随安全层收敛：Reality 仅 tcp/xhttp/grpc（xray-core：REALITY only supports RAW, XHTTP and gRPC）
const networkOptions = computed(() =>
  form.value.security === 'reality'
    ? NETWORK_OPTIONS_ALL.filter((o) => REALITY_NETWORKS.includes(o.value))
    : NETWORK_OPTIONS_ALL,
)
const tcpHeaderOptions = [
  { label: '无', value: 'none' },
  { label: 'HTTP 伪装', value: 'http' },
]
// 安全层随协议收敛：Reality 仅 VLESS（xray-core 限制，vmess/trojan/ss 只有 none/tls）
const securityOptions = computed(() =>
  form.value.protocol === 'vless'
    ? [
        { label: '无', value: 'none' },
        { label: 'TLS', value: 'tls' },
        { label: 'Reality', value: 'reality' },
      ]
    : [
        { label: '无', value: 'none' },
        { label: 'TLS', value: 'tls' },
      ],
)
const ssMethods = ['aes-256-gcm', 'aes-128-gcm', 'chacha20-poly1305', 'xchacha20-poly1305', '2022-blake3-aes-256-gcm', '2022-blake3-aes-128-gcm'].map((m) => ({ label: m, value: m }))

const emptyForm = () => ({
  target_source: 'manual', // manual（手工填服务器）| relay（目标为本站服务器 → 自动中转出站）
  relay_target: '',        // 本站服务器候选：值 = "node_id:inbound_id"
  tag: '',
  protocol: 'vmess',
  address: '',
  port: 443,
  user_id: '',
  password: '',
  method: 'aes-256-gcm',
  username: '',
  flow: '',
  encryption: 'auto',
  network: 'tcp',
  security: 'none',
  dns_network: 'udp', // dns 出站的查询传输（xray settings.network）；与传输层 network 不是一回事
  ws_path: '/',
  ws_host: '',
  httpupgrade_path: '/',
  httpupgrade_host: '',
  tcp_header_type: 'none',
  tcp_header_path: '/',
  tcp_header_host: '',
  xhttp_path: '/',
  xhttp_host: '',
  xhttp_mode: 'auto',
  xhttp_padding_bytes: '100-1000',
  xhttp_padding_obfs_mode: false,
  xhttp_padding_key: '',
  xhttp_padding_header: '',
  xhttp_padding_placement: '',
  xhttp_padding_method: '',
  xhttp_session_id_placement: '',
  xhttp_session_id_key: '',
  xhttp_session_id_table: '',
  xhttp_session_id_length: '',
  xhttp_seq_placement: '',
  xhttp_seq_key: '',
  xhttp_uplink_data_placement: '',
  xhttp_uplink_data_key: '',
  xhttp_uplink_chunk_size: '',
  xhttp_sc_max_each_post_bytes: '',
  xhttp_sc_max_buffered_posts: '',
  xhttp_sc_min_posts_interval_ms: '',
  xhttp_sc_stream_up_server_secs: '',
  xhttp_server_max_header_bytes: '',
  xhttp_uplink_http_method: '',
  xhttp_no_sse_header: false,
  xhttp_no_grpc_header: false,
  xhttp_headers: '',
  xhttp_xmux_enabled: false,
  xhttp_xmux_max_concurrency: '',
  xhttp_xmux_max_connections: '',
  xhttp_xmux_c_max_reuse_times: '',
  xhttp_xmux_h_max_request_times: '',
  xhttp_xmux_h_max_reusable_secs: '',
  xhttp_xmux_h_keep_alive_period: '',
  tls_server_name: '',
  reality_server_name: '',
  reality_public_key: '',
  reality_short_id: '',
  reality_fingerprint: 'chrome',
  reality_spider_x: '',
  dialer_proxy: '',
  remark: '',
  enabled: true,
  advanced_settings: '',
  advanced_stream: '',
})
const form = ref(emptyForm())

// 表单内「从链接导入」：回车 → 解析 → 填充表单（后端 import 端点已就绪）
const formLink = ref('')
const formImporting = ref(false)

const isServerProto = computed(() => ['vmess', 'vless', 'trojan', 'shadowsocks', 'socks', 'http', 'hysteria', 'dns'].includes(form.value.protocol))
const isUuidProto = computed(() => ['vmess', 'vless'].includes(form.value.protocol))
// hysteria 出站：地址/端口 + auth（明文密码），安全层固定 TLS，没有 vless 那一套 transport
const isHyOut = computed(() => form.value.protocol === 'hysteria')
// dns 出站：只有「查什么、走 udp 还是 tcp」，没有传输层/安全层（settings = {network, address, port}）
const isDnsOut = computed(() => form.value.protocol === 'dns')
const isWireguard = computed(() => form.value.protocol === 'wireguard')
// WARP：协议选 WARP → 后端走 WarpService 自动注册并生成 wireguard 出站，表单无需填服务器
const isWarp = computed(() => form.value.protocol === 'warp')

// 协议切换时收敛：Reality 仅 VLESS（切走后残留非法组合会被后端拒绝）
watch(
  () => form.value.protocol,
  () => {
    if (form.value.protocol !== 'vless' && form.value.security === 'reality') form.value.security = 'none'
    // hysteria 出站强制 tls（QUIC 走 TLS1.3，没有传输层可选）
    if (isHyOut.value) {
      form.value.network = 'hysteria'
      form.value.security = 'tls'
    } else if (form.value.network === 'hysteria') {
      // 从 hysteria 切走：把残留的 hysteria 传输收回 tcp，否则后端会直接拒（只有 hysteria 能用它）
      form.value.network = 'tcp'
    }
    // dns 出站没有传输层：端口从表单默认 443 挪到 DNS 的 53（没手改过才挪）
    if (isDnsOut.value && Number(form.value.port) === 443) form.value.port = 53
  },
)
// 安全层切换时收敛：Reality 时传输收回到合法面（tcp/xhttp/grpc）
watch(
  () => form.value.security,
  () => {
    if (form.value.security === 'reality' && !REALITY_NETWORKS.includes(form.value.network)) form.value.network = 'tcp'
  },
)
// 「目标来源 = 本站服务器」：保存时后端自动中转（读对方面板 → 对方建承接凭据 → 本地建中转出站）
const isRelaySource = computed(() => form.value.target_source === 'relay')
const targetSourceOptions = [
  { label: '自己填写（手填地址、端口、凭据）', value: 'manual' },
  { label: '本站服务器（从已接入的服务器里选，自动配好出口）', value: 'relay' },
]
// 本站服务器候选：value = "node_id:inbound_id"，label 如「香港01 · 443 · vless-reality · vless」
const relayTargetOptions = computed(() =>
  relayTargets.value.map((t) => ({
    label: t.label,
    value: `${t.node_id}:${t.inbound_id}`,
    disabled: t.supported === false,
  })),
)
const relayTargetHint = computed(() => {
  if (!isRelaySource.value) return ''
  const t = relayTargets.value.find((x) => `${x.node_id}:${x.inbound_id}` === form.value.relay_target)
  if (t && t.supported === false) return `目标入站协议（${t.protocol}）暂不支持自动中转`
  return '保存后自动在对方服务器建好承接凭据，并在本服务器生成中转出口（到「路由」里直接选它即可）'
})
const wgForm = ref({ wg_secret_key: '', wg_address: '', wg_peer_public_key: '', wg_peer_endpoint: '', wg_reserved: '0,0,0' })

// 「借道出站」候选 = 同节点其它出站（含 WARP）；排除自身与 freedom/blackhole（无出口）。
// 若当前编辑值不在候选里（如目标已停用），仍并入下拉并标注，避免静默改配置。
const dialerCandidates = computed(() => {
  const other = outbounds.value.filter(
    (o) => !['freedom', 'blackhole'].includes(o.protocol) && (!editing.value || o.id !== editing.value.id),
  )
  const opts = other.map((o) => ({ label: o.remark ? `${o.tag}（${o.remark}）` : o.tag, value: o.tag }))
  const cur = form.value.dialer_proxy
  if (cur && !opts.some((o) => o.value === cur)) {
    opts.unshift({ label: `${cur}（当前值，可能已停用）`, value: cur })
  }
  return opts
})

const formatBytes = (n) => {
  n = Number(n) || 0
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let i = -1
  do {
    n /= 1024
    i++
  } while (n >= 1024 && i < units.length - 1)
  return `${n.toFixed(2)} ${units[i]}`
}

const load = async () => {
  if (!nodeId.value) {
    outbounds.value = []
    relayTargets.value = []
    return
  }
  loading.value = true
  try {
    const res = await adminApi.listXrayOutbounds(nodeId.value)
    outbounds.value = res?.outbounds || []
    relayTargets.value = res?.relay_targets || []
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}
watch(nodeId, () => {
  load()
  if (tab.value === 'subscriptions') loadSubs()
})

const openCreate = () => {
  editing.value = null
  form.value = emptyForm()
  wgForm.value = { wg_secret_key: '', wg_address: '', wg_peer_public_key: '', wg_peer_endpoint: '', wg_reserved: '0,0,0' }
  formLink.value = ''
  advancedOpen.value = false
  modalOpen.value = true
}

const openEdit = (ob) => {
  editing.value = ob
  const s = ob.settings || {}
  const st = ob.stream_settings || {}
  const first = s.vnext?.[0] || s.servers?.[0] || {}
  const user = first.users?.[0] || {}
  form.value = {
    ...emptyForm(),
    tag: ob.tag,
    protocol: ob.protocol,
    address: ob.address || first.address || '',
    port: ob.port || first.port || 443,
    // dns 出站的查询传输存在 settings.network（udp/tcp），与传输层 network 不是同一个东西
    dns_network: ob.protocol === 'dns' && s.network === 'tcp' ? 'tcp' : 'udp',
    user_id: user.id || '',
    password: first.password || user.pass || st.hysteriaSettings?.auth || '',
    method: first.method || 'aes-256-gcm',
    username: user.user || '',
    flow: user.flow || '',
    encryption: user.security || 'auto',
    network: st.network || 'tcp',
    security: st.security || 'none',
    ws_path: st.wsSettings?.path || '/',
    ws_host: st.wsSettings?.headers?.Host || '',
    httpupgrade_path: st.httpupgradeSettings?.path || '/',
    httpupgrade_host: st.httpupgradeSettings?.host || '',
    tcp_header_type: st.tcpSettings?.header?.type || 'none',
    tcp_header_path: st.tcpSettings?.header?.request?.path?.[0] || '/',
    tcp_header_host:
      st.tcpSettings?.header?.response?.headers?.Host?.[0] ||
      st.tcpSettings?.header?.request?.headers?.Host?.[0] ||
      '',
    xhttp_path: st.xhttpSettings?.path || '/',
    xhttp_host: st.xhttpSettings?.host || '',
    xhttp_mode: st.xhttpSettings?.mode || 'auto',
    xhttp_padding_bytes: st.xhttpSettings?.xPaddingBytes || '',
    xhttp_padding_obfs_mode: !!st.xhttpSettings?.xPaddingObfsMode,
    xhttp_padding_key: st.xhttpSettings?.xPaddingKey || '',
    xhttp_padding_header: st.xhttpSettings?.xPaddingHeader || '',
    xhttp_padding_placement: st.xhttpSettings?.xPaddingPlacement || '',
    xhttp_padding_method: st.xhttpSettings?.xPaddingMethod || '',
    xhttp_session_id_placement: st.xhttpSettings?.sessionIDPlacement || '',
    xhttp_session_id_key: st.xhttpSettings?.sessionIDKey || '',
    xhttp_session_id_table: st.xhttpSettings?.sessionIDTable || '',
    xhttp_session_id_length: st.xhttpSettings?.sessionIDLength || '',
    xhttp_seq_placement: st.xhttpSettings?.seqPlacement || '',
    xhttp_seq_key: st.xhttpSettings?.seqKey || '',
    xhttp_uplink_data_placement: st.xhttpSettings?.uplinkDataPlacement || '',
    xhttp_uplink_data_key: st.xhttpSettings?.uplinkDataKey || '',
    xhttp_uplink_chunk_size: st.xhttpSettings?.uplinkChunkSize ?? '',
    xhttp_sc_max_each_post_bytes: st.xhttpSettings?.scMaxEachPostBytes || '',
    xhttp_sc_max_buffered_posts: st.xhttpSettings?.scMaxBufferedPosts ?? '',
    xhttp_sc_min_posts_interval_ms: st.xhttpSettings?.scMinPostsIntervalMs || '',
    xhttp_sc_stream_up_server_secs: st.xhttpSettings?.scStreamUpServerSecs || '',
    xhttp_server_max_header_bytes: st.xhttpSettings?.serverMaxHeaderBytes ?? '',
    xhttp_uplink_http_method: st.xhttpSettings?.uplinkHTTPMethod || '',
    xhttp_no_sse_header: !!st.xhttpSettings?.noSSEHeader,
    xhttp_no_grpc_header: !!st.xhttpSettings?.noGRPCHeader,
    xhttp_headers: Object.entries(st.xhttpSettings?.headers || {})
      .map(([k, v]) => `${k}: ${Array.isArray(v) ? v[0] || '' : v}`)
      .join('\n'),
    xhttp_xmux_enabled: !!(st.xhttpSettings?.xmux && Object.keys(st.xhttpSettings.xmux).length),
    xhttp_xmux_max_concurrency: st.xhttpSettings?.xmux?.maxConcurrency ?? '',
    xhttp_xmux_max_connections: st.xhttpSettings?.xmux?.maxConnections ?? '',
    xhttp_xmux_c_max_reuse_times: st.xhttpSettings?.xmux?.cMaxReuseTimes ?? '',
    xhttp_xmux_h_max_request_times: st.xhttpSettings?.xmux?.hMaxRequestTimes ?? '',
    xhttp_xmux_h_max_reusable_secs: st.xhttpSettings?.xmux?.hMaxReusableSecs ?? '',
    xhttp_xmux_h_keep_alive_period: st.xhttpSettings?.xmux?.hKeepAlivePeriod ?? '',
    tls_server_name: st.tlsSettings?.serverName || '',
    reality_server_name: st.realitySettings?.serverName || '',
    reality_public_key: st.realitySettings?.publicKey || '',
    reality_short_id: st.realitySettings?.shortId || '',
    reality_fingerprint: st.realitySettings?.fingerprint || 'chrome',
    reality_spider_x: st.realitySettings?.spiderX || '',
    dialer_proxy: ob.dialer_proxy || st.sockopt?.dialerProxy || '',
    remark: ob.remark || '',
    enabled: ob.enabled,
  }
  if (ob.protocol === 'wireguard') {
    wgForm.value = {
      wg_secret_key: '',
      wg_address: (s.address || []).join(', '),
      wg_peer_public_key: s.peers?.[0]?.publicKey || '',
      wg_peer_endpoint: s.peers?.[0]?.endpoint || '',
      wg_reserved: (s.reserved || []).join(','),
    }
  }
  advancedOpen.value = false
  modalOpen.value = true
}

/**
 * 从路由页「＋ 新建中转出口」跳转而来（?relay=1）：直接打开新建弹窗并预填
 * 「目标来源 = 本站服务器」。候选的服务器/入站在跳转时拿不到（路由下拉只有裸 tag），
 * 故只预填来源，让用户在此页选择具体目标。
 */
const applyRelayPrefill = () => {
  if (String(route.query.relay || '') !== '1') return
  openCreate()
  form.value.target_source = 'relay'
}
onMounted(applyRelayPrefill)

// 表单内导入：解析分享链接并回填结构化表单（凭据/传输/目标），可再手动微调。
const applyLink = async () => {
  const link = formLink.value.trim()
  if (!link) return
  formImporting.value = true
  try {
    const res = await adminApi.importXrayOutbound(nodeId.value, link)
    const p = res?.parsed
    if (!p) {
      toast.error('链接未解析出内容')
      return
    }
    const s = p.settings || {}
    const st = p.stream_settings || {}
    const first = s.vnext?.[0] || s.servers?.[0] || {}
    const user = first.users?.[0] || {}
    const curTag = form.value.tag
    form.value = {
      ...emptyForm(),
      tag: p.tag || curTag || '',
      protocol: p.protocol,
      address: first.address || '',
      port: first.port || 443,
      user_id: user.id || '',
      password: first.password || user.pass || '',
      method: first.method || 'aes-256-gcm',
      username: user.user || '',
      flow: user.flow || '',
      encryption: user.security || 'auto',
      network: st.network || 'tcp',
      security: st.security || 'none',
      ws_path: st.wsSettings?.path || '/',
      ws_host: st.wsSettings?.headers?.Host || '',
      httpupgrade_path: st.httpupgradeSettings?.path || '/',
      httpupgrade_host: st.httpupgradeSettings?.host || '',
      tcp_header_type: st.tcpSettings?.header?.type || 'none',
      tcp_header_path: st.tcpSettings?.header?.request?.path?.[0] || '/',
      tcp_header_host:
        st.tcpSettings?.header?.response?.headers?.Host?.[0] ||
        st.tcpSettings?.header?.request?.headers?.Host?.[0] ||
        '',
      xhttp_path: st.xhttpSettings?.path || '/',
      xhttp_host: st.xhttpSettings?.host || '',
      xhttp_mode: st.xhttpSettings?.mode || 'auto',
      xhttp_padding_bytes: st.xhttpSettings?.xPaddingBytes || '',
      xhttp_padding_obfs_mode: !!st.xhttpSettings?.xPaddingObfsMode,
      xhttp_padding_key: st.xhttpSettings?.xPaddingKey || '',
      xhttp_padding_header: st.xhttpSettings?.xPaddingHeader || '',
      xhttp_padding_placement: st.xhttpSettings?.xPaddingPlacement || '',
      xhttp_padding_method: st.xhttpSettings?.xPaddingMethod || '',
      xhttp_session_id_placement: st.xhttpSettings?.sessionIDPlacement || '',
      xhttp_session_id_key: st.xhttpSettings?.sessionIDKey || '',
      xhttp_session_id_table: st.xhttpSettings?.sessionIDTable || '',
      xhttp_session_id_length: st.xhttpSettings?.sessionIDLength || '',
      xhttp_seq_placement: st.xhttpSettings?.seqPlacement || '',
      xhttp_seq_key: st.xhttpSettings?.seqKey || '',
      xhttp_uplink_data_placement: st.xhttpSettings?.uplinkDataPlacement || '',
      xhttp_uplink_data_key: st.xhttpSettings?.uplinkDataKey || '',
      xhttp_uplink_chunk_size: st.xhttpSettings?.uplinkChunkSize ?? '',
      xhttp_sc_max_each_post_bytes: st.xhttpSettings?.scMaxEachPostBytes || '',
      xhttp_sc_max_buffered_posts: st.xhttpSettings?.scMaxBufferedPosts ?? '',
      xhttp_sc_min_posts_interval_ms: st.xhttpSettings?.scMinPostsIntervalMs || '',
      xhttp_sc_stream_up_server_secs: st.xhttpSettings?.scStreamUpServerSecs || '',
      xhttp_server_max_header_bytes: st.xhttpSettings?.serverMaxHeaderBytes ?? '',
      xhttp_uplink_http_method: st.xhttpSettings?.uplinkHTTPMethod || '',
      xhttp_no_sse_header: !!st.xhttpSettings?.noSSEHeader,
      xhttp_no_grpc_header: !!st.xhttpSettings?.noGRPCHeader,
      xhttp_headers: Object.entries(st.xhttpSettings?.headers || {})
        .map(([k, v]) => `${k}: ${Array.isArray(v) ? v[0] || '' : v}`)
        .join('\n'),
      xhttp_xmux_enabled: !!(st.xhttpSettings?.xmux && Object.keys(st.xhttpSettings.xmux).length),
      xhttp_xmux_max_concurrency: st.xhttpSettings?.xmux?.maxConcurrency ?? '',
      xhttp_xmux_max_connections: st.xhttpSettings?.xmux?.maxConnections ?? '',
      xhttp_xmux_c_max_reuse_times: st.xhttpSettings?.xmux?.cMaxReuseTimes ?? '',
      xhttp_xmux_h_max_request_times: st.xhttpSettings?.xmux?.hMaxRequestTimes ?? '',
      xhttp_xmux_h_max_reusable_secs: st.xhttpSettings?.xmux?.hMaxReusableSecs ?? '',
      xhttp_xmux_h_keep_alive_period: st.xhttpSettings?.xmux?.hKeepAlivePeriod ?? '',
      tls_server_name: st.tlsSettings?.serverName || '',
      reality_server_name: st.realitySettings?.serverName || '',
      reality_public_key: st.realitySettings?.publicKey || '',
      reality_short_id: st.realitySettings?.shortId || '',
      reality_fingerprint: st.realitySettings?.fingerprint || 'chrome',
      reality_spider_x: st.realitySettings?.spiderX || '',
      remark: form.value.remark || '',
      enabled: true,
    }
    formLink.value = ''
    toast.success('已解析链接，请核对后保存')
  } catch (e) {
    toast.error(e.message || '链接解析失败')
  } finally {
    formImporting.value = false
  }
}

const save = async () => {
  if (!nodeId.value) return
  // WARP：无需 tag/服务器，后端自动注册 WARP 并生成 wireguard 出站（tag=warp）
  if (isWarp.value) {
    saving.value = true
    try {
      await adminApi.createXrayOutbound(nodeId.value, { protocol: 'warp' })
      toast.success('WARP 已注册并应用（节点将秒级生效）')
      // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
      saved.value = true
      setTimeout(() => {
        saved.value = false
        modalOpen.value = false
      }, 900)
      await load()
    } catch (e) {
      toast.error(e.message || 'WARP 创建失败')
    } finally {
      saving.value = false
    }
    return
  }
  // 目标来源「本站服务器」：后端自动中转（对方建承接凭据 + 本地建中转出站），无需手填 tag/凭据
  if (isRelaySource.value) {
    const [tn, ti] = String(form.value.relay_target || '').split(':').map((x) => Number(x))
    if (!tn || !ti) {
      toast.error('请选择服务器与入站')
      return
    }
    saving.value = true
    try {
      await adminApi.createXrayOutbound(nodeId.value, {
        target_source: 'relay',
        target_node_id: tn,
        target_inbound_id: ti,
        tag: form.value.tag || null,       // 留空 → 后端用默认「服务器名-端口-入站名」
        remark: form.value.remark || null, // 留空 → 备注为空
      })
      toast.success('中转出口已创建（已在对方服务器建好承接凭据，秒级生效）')
      // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
      saved.value = true
      setTimeout(() => {
        saved.value = false
        modalOpen.value = false
      }, 900)
      await load()
    } catch (e) {
      toast.error(e.message || '中转出站创建失败')
    } finally {
      saving.value = false
    }
    return
  }
  if (!form.value.tag) {
    toast.error('请填标识 tag')
    return
  }
  saving.value = true
  try {
    const payload = {
      tag: form.value.tag,
      protocol: form.value.protocol,
      remark: form.value.remark || null,
      enabled: form.value.enabled,
      network: form.value.network,
      security: form.value.security,
      ws_path: form.value.ws_path,
      ws_host: form.value.ws_host,
      httpupgrade_path: form.value.httpupgrade_path,
      httpupgrade_host: form.value.httpupgrade_host,
      tcp_header_type: form.value.tcp_header_type,
      tcp_header_path: form.value.tcp_header_path,
      tcp_header_host: form.value.tcp_header_host,
      xhttp_path: form.value.xhttp_path,
      xhttp_host: form.value.xhttp_host,
      xhttp_mode: form.value.xhttp_mode,
      // xhttp 高级字段全量提交（编辑能清空；空值后端不落库）
      xhttp_padding_bytes: form.value.xhttp_padding_bytes,
      xhttp_padding_obfs_mode: form.value.xhttp_padding_obfs_mode,
      xhttp_padding_key: form.value.xhttp_padding_key,
      xhttp_padding_header: form.value.xhttp_padding_header,
      xhttp_padding_placement: form.value.xhttp_padding_placement,
      xhttp_padding_method: form.value.xhttp_padding_method,
      xhttp_session_id_placement: form.value.xhttp_session_id_placement,
      xhttp_session_id_key: form.value.xhttp_session_id_key,
      xhttp_session_id_table: form.value.xhttp_session_id_table,
      xhttp_session_id_length: form.value.xhttp_session_id_length,
      xhttp_seq_placement: form.value.xhttp_seq_placement,
      xhttp_seq_key: form.value.xhttp_seq_key,
      xhttp_uplink_data_placement: form.value.xhttp_uplink_data_placement,
      xhttp_uplink_data_key: form.value.xhttp_uplink_data_key,
      xhttp_uplink_chunk_size: form.value.xhttp_uplink_chunk_size,
      xhttp_sc_max_each_post_bytes: form.value.xhttp_sc_max_each_post_bytes,
      xhttp_sc_max_buffered_posts: form.value.xhttp_sc_max_buffered_posts,
      xhttp_sc_min_posts_interval_ms: form.value.xhttp_sc_min_posts_interval_ms,
      xhttp_sc_stream_up_server_secs: form.value.xhttp_sc_stream_up_server_secs,
      xhttp_server_max_header_bytes: form.value.xhttp_server_max_header_bytes,
      xhttp_uplink_http_method: form.value.xhttp_uplink_http_method,
      xhttp_no_sse_header: form.value.xhttp_no_sse_header,
      xhttp_no_grpc_header: form.value.xhttp_no_grpc_header,
      xhttp_headers: form.value.xhttp_headers,
      xhttp_xmux_max_concurrency: form.value.xhttp_xmux_max_concurrency,
      xhttp_xmux_max_connections: form.value.xhttp_xmux_max_connections,
      xhttp_xmux_c_max_reuse_times: form.value.xhttp_xmux_c_max_reuse_times,
      xhttp_xmux_h_max_request_times: form.value.xhttp_xmux_h_max_request_times,
      xhttp_xmux_h_max_reusable_secs: form.value.xhttp_xmux_h_max_reusable_secs,
      xhttp_xmux_h_keep_alive_period: form.value.xhttp_xmux_h_keep_alive_period,
      tls_server_name: form.value.tls_server_name,
      reality_server_name: form.value.reality_server_name,
      reality_public_key: form.value.reality_public_key,
      reality_short_id: form.value.reality_short_id,
      reality_fingerprint: form.value.reality_fingerprint,
      reality_spider_x: form.value.reality_spider_x,
      dialer_proxy: form.value.dialer_proxy || null,
    }
    if (isServerProto.value) {
      payload.address = form.value.address
      payload.port = Number(form.value.port)
    }
    if (isDnsOut.value) payload.dns_network = form.value.dns_network
    if (isUuidProto.value) {
      payload.user_id = form.value.user_id
      payload.encryption = form.value.encryption
      payload.flow = form.value.flow
    }
    if (['trojan', 'shadowsocks', 'socks', 'http', 'hysteria'].includes(form.value.protocol)) {
      payload.password = form.value.password
    }
    if (form.value.protocol === 'shadowsocks') payload.method = form.value.method
    if (['socks', 'http'].includes(form.value.protocol)) payload.username = form.value.username
    if (isWireguard.value) Object.assign(payload, wgForm.value)
    if (advancedOpen.value) {
      if (form.value.advanced_settings) payload.advanced_settings = form.value.advanced_settings
      if (form.value.advanced_stream) payload.advanced_stream = form.value.advanced_stream
    }

    if (editing.value) {
      await adminApi.updateXrayOutbound(nodeId.value, editing.value.id, payload)
      toast.success('出站已更新（节点将秒级生效）')
    } else {
      await adminApi.createXrayOutbound(nodeId.value, payload)
      toast.success('出站已创建（节点将秒级生效）')
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

const remove = async (ob) => {
  const msg = ob.relay_target_node_id
    ? `删除中转出口 ${ob.tag}？将同时清理目标服务器上的承接凭据；引用它的路由规则会随之失效。`
    : `删除出站 ${ob.tag}？引用它的路由规则会随之失效`
  if (!confirm(msg)) return
  try {
    await adminApi.deleteXrayOutbound(nodeId.value, ob.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

const test = async (ob) => {
  testingId.value = ob.id
  try {
    const res = await adminApi.testXrayOutbound(nodeId.value, ob.id)
    if (res?.pending) {
      toast.warning(res.message || '节点未及时回执，请稍后重试')
    } else if (res?.success) {
      toast.success(res.output || '目标可达')
    } else {
      toast.error(res?.output || '目标不可达')
    }
  } catch (e) {
    toast.error(e.message || '测试失败')
  } finally {
    testingId.value = null
  }
}

// 复制文本到剪贴板（非安全上下文走 execCommand 兜底，沿用 Nodes.vue 口径）
const copyText = async (text) => {
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
  } catch (e) {
    const ta = document.createElement('textarea')
    ta.value = text
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    document.body.removeChild(ta)
  }
}

/**
 * 「复制链接」：取该出站的客户端配置链接 → 剪贴板。
 * 只读操作（后端 GET，不写配置）。wireguard 等无标准格式时后端返回 code!=0，
 * 捕获后提示原因（按钮本身已置灰，此处兜底提示）。
 */
const copyingId = ref(null)
const copyLink = async (ob) => {
  if (ob.link_supported === false) {
    toast.warning('该出站类型没有标准分享链接（如 WireGuard）')
    return
  }
  copyingId.value = ob.id
  try {
    const res = await adminApi.shareXrayOutboundLink(nodeId.value, ob.id)
    await copyText(res?.link)
    toast.success('出站链接已复制，可到 3x 面板「出站导入」粘贴使用')
  } catch (e) {
    toast.error(e.message || '该出站暂不支持导出链接')
  } finally {
    copyingId.value = null
  }
}

const doImport = async () => {
  if (!importLink.value) return
  importing.value = true
  try {
    const res = await adminApi.importXrayOutbound(nodeId.value, importLink.value)
    importPreview.value = res?.parsed || null
  } catch (e) {
    importPreview.value = null
    toast.error(e.message || '链接解析失败')
  } finally {
    importing.value = false
  }
}

const confirmImport = async () => {
  if (!importPreview.value) return
  const p = importPreview.value
  try {
    await adminApi.createXrayOutbound(nodeId.value, {
      tag: p.tag,
      protocol: p.protocol,
      network: p.stream_settings?.network || 'tcp',
      security: p.stream_settings?.security || 'none',
      advanced_settings: JSON.stringify(p.settings || {}),
      advanced_stream: JSON.stringify(p.stream_settings || {}),
      enabled: true,
    })
    toast.success('已从链接导入出站（节点将秒级生效）')
    importOpen.value = false
    importLink.value = ''
    importPreview.value = null
    await load()
  } catch (e) {
    toast.error(e.message || '导入失败')
  }
}

const protocolLabel = { vmess: 'VMess', vless: 'VLESS', trojan: 'Trojan', shadowsocks: 'SS', socks: 'SOCKS5', http: 'HTTP', freedom: 'Freedom', blackhole: 'Blackhole', wireguard: 'WireGuard', hysteria: 'HY2', dns: 'DNS', warp: 'WARP' }

// 「目标」列：中转出站优先显示用户填的备注，无则显示标识 tag（tag 已可读，如「香港01-51177-联通优化」）
const targetLabel = (ob) =>
  ob.relay_target_node_id
    ? (ob.remark || [ob.relay_target_node_name, ob.relay_target_port].filter(Boolean).join(' · ') || ob.tag)
    : (ob.address ? `${ob.address}:${ob.port}` : (ob.remark || '—'))
const protocolVariant = (p) => ({ vless: 'vless', trojan: 'trojan' }[p] || 'info')

// ── 订阅（服务器池）──
const loadSubs = async () => {
  if (!nodeId.value) {
    subs.value = []
    return
  }
  subsLoading.value = true
  try {
    const res = await adminApi.listXrayOutboundSubscriptions(nodeId.value)
    subs.value = res?.items || []
  } catch (e) {
    toast.error(e.message || '订阅加载失败')
  } finally {
    subsLoading.value = false
  }
}
watch(tab, (t) => { if (t === 'subscriptions') loadSubs() })

const openSubCreate = () => {
  subEditing.value = null
  subForm.value = emptySubForm()
  subModalOpen.value = true
}
const openSubEdit = (s) => {
  subEditing.value = s
  subForm.value = {
    remark: s.remark || '',
    url: s.url || '',
    tag_prefix: s.tag_prefix || '',
    update_interval: s.update_interval || 600,
    prepend: !!s.prepend,
    enabled: !!s.enabled,
  }
  subModalOpen.value = true
}
const saveSub = async () => {
  if (!subForm.value.url) {
    toast.error('请填写订阅地址')
    return
  }
  subSaving.value = true
  try {
    if (subEditing.value) {
      await adminApi.updateXrayOutboundSubscription(nodeId.value, subEditing.value.id, subForm.value)
      toast.success('订阅已更新（节点将秒级生效）')
    } else {
      await adminApi.createXrayOutboundSubscription(nodeId.value, subForm.value)
      toast.success('订阅已创建并拉取')
    }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    subSaved.value = true
    setTimeout(() => {
      subSaved.value = false
      subModalOpen.value = false
    }, 900)
    await loadSubs()
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    subSaving.value = false
  }
}
const refreshSub = async (s) => {
  refreshingId.value = s.id
  try {
    const res = await adminApi.refreshXrayOutboundSubscription(nodeId.value, s.id)
    if (res?.last_status === 'success') toast.success(`拉取成功（${res.node_count} 个出站）`)
    else toast.error(res?.last_error || '拉取失败（已保留上次成功缓存）')
    await loadSubs()
    await load()
  } catch (e) {
    toast.error(e.message || '刷新失败')
  } finally {
    refreshingId.value = null
  }
}
const removeSub = async (s) => {
  if (!confirm(`删除订阅「${s.remark || s.id}」？其注入的出站会随之失效`)) return
  try {
    await adminApi.deleteXrayOutboundSubscription(nodeId.value, s.id)
    // 撤销：按原订阅字段重建（订阅是简单 URL 数据，可恢复）
    toast.success(`已删除「${s.remark || s.id}」`, 4000, async () => {
      try {
        await adminApi.createXrayOutboundSubscription(nodeId.value, {
          remark: s.remark || '',
          url: s.url || '',
          tag_prefix: s.tag_prefix || '',
          update_interval: s.update_interval || 600,
          prepend: !!s.prepend,
          enabled: s.enabled !== false,
        })
        await loadSubs()
        await load()
        toast.success('已恢复')
      } catch (e) {
        toast.error(e.message || '恢复失败')
      }
    })
    await loadSubs()
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}
</script>

<template>
  <div class="an">
    <div class="an-head">
      <h1 class="an-title">xray 出站</h1>
      <div class="an-head-actions">
        <template v-if="tab === 'outbounds'">
          <Button variant="secondary" size="sm" :disabled="!nodeId" @click="importOpen = true">
            <Icon name="package" :size="16" /> 从链接导入
          </Button>
          <Button variant="primary" size="sm" :disabled="!nodeId" @click="openCreate">
            <Icon name="plus" :size="16" /> 新建出站
          </Button>
        </template>
        <template v-else>
          <Button variant="primary" size="sm" :disabled="!nodeId" @click="openSubCreate">
            <Icon name="plus" :size="16" /> 新建订阅
          </Button>
        </template>
      </div>
    </div>
    <p class="an-desc">
      <template v-if="tab === 'outbounds'">
        出站 = 流量出口。直连节点用内置 direct 即可；需要「中转 / 换出口 IP」时建自定义出站，再配合「路由」把流量导过去。
      </template>
      <template v-else>
        出站订阅 = 服务器池。填一条外部订阅地址，后台定时拉取并解析成出站注入本节点；同一服务器跨刷新保持同一标识（tag），路由/负载均衡引用不被打断。
      </template>
    </p>

    <div class="an-toolbar">
      <XrayNodePicker v-model="nodeId" />
      <div class="an-tabs">
        <button class="an-tab" :class="{ active: tab === 'outbounds' }" @click="tab = 'outbounds'">自定义出站</button>
        <button class="an-tab" :class="{ active: tab === 'subscriptions' }" @click="tab = 'subscriptions'">出站订阅</button>
      </div>
    </div>

    <Card v-if="tab === 'outbounds'" padding="false">
      <div class="an-table-wrap">
        <table class="an-table">
          <thead>
            <tr>
              <th>标识</th>
              <th>协议</th>
              <th>目标</th>
              <th>传输</th>
              <th>流量（上传 / 下载）</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="ob in outbounds" :key="ob.id">
              <td class="mono">{{ ob.tag }}</td>
              <td><Badge :variant="protocolVariant(ob.protocol)">{{ protocolLabel[ob.protocol] || ob.protocol }}</Badge></td>
              <td class="mono">{{ targetLabel(ob) }}</td>
              <td class="mono">{{ ob.network ? `${ob.network}/${ob.security}` : '—' }}</td>
              <td class="mono">{{ formatBytes(ob.upload) }} / {{ formatBytes(ob.download) }}</td>
              <td><Badge :variant="ob.enabled ? 'success' : 'warning'" dot>{{ ob.enabled ? '启用' : '停用' }}</Badge></td>
              <td>
                <div class="an-actions">
                  <Button size="sm" variant="secondary" :loading="testingId === ob.id" @click="test(ob)">测试</Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    :disabled="ob.link_supported === false"
                    :loading="copyingId === ob.id"
                    :title="ob.link_supported === false ? '该出站类型没有标准分享链接（如 WireGuard）' : '复制客户端配置链接（可粘贴到 3x 出站导入）'"
                    @click="copyLink(ob)"
                  >
                    <Icon name="copy" :size="16" /> 复制链接
                  </Button>
                  <Button size="sm" variant="ghost" @click="openEdit(ob)"><Icon name="edit" :size="16" /></Button>
                  <Button size="sm" variant="ghost" @click="remove(ob)"><Icon name="trash" :size="16" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!loading && outbounds.length === 0">
              <td colspan="7" class="an-empty">{{ nodeId ? '还没有自定义出站（内置 direct 直连已可用）' : '请先在上方选择一个 xray 节点' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 订阅（服务器池）-->
    <Card v-if="tab === 'subscriptions'" padding="false">
      <div class="an-table-wrap">
        <table class="an-table">
          <thead>
            <tr>
              <th>备注</th>
              <th>订阅地址</th>
              <th>前缀</th>
              <th>出站数</th>
              <th>间隔</th>
              <th>注入</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="s in subs" :key="s.id">
              <td>{{ s.remark || '—' }}</td>
              <td class="mono">{{ s.url_masked }}</td>
              <td class="mono">{{ s.tag_prefix || `sub${s.id}-` }}</td>
              <td class="mono">{{ s.outbound_count }}</td>
              <td class="mono">{{ s.update_interval }}s</td>
              <td><Badge :variant="s.prepend ? 'warning' : 'info'">{{ s.prepend ? '前置' : '后置' }}</Badge></td>
              <td>
                <Badge :variant="s.last_status === 'success' ? 'success' : (s.last_status === 'failed' ? 'danger' : 'warning')" dot>
                  {{ s.last_status === 'success' ? '正常' : (s.last_status === 'failed' ? '失败' : '待拉取') }}
                </Badge>
                <Badge v-if="!s.enabled" variant="warning" class="an-badge-gap">已停用</Badge>
              </td>
              <td>
                <div class="an-actions">
                  <Button size="sm" variant="secondary" :loading="refreshingId === s.id" @click="refreshSub(s)">刷新</Button>
                  <Button size="sm" variant="ghost" @click="openSubEdit(s)"><Icon name="edit" :size="16" /></Button>
                  <Button size="sm" variant="ghost" @click="removeSub(s)"><Icon name="trash" :size="16" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!subsLoading && subs.length === 0">
              <td colspan="8" class="an-empty">{{ nodeId ? '还没有出站订阅（点右上「新建订阅」接入服务器池）' : '请先在上方选择一个 xray 节点' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="subs.some((s) => s.last_error)" class="an-suberr">
        <p v-for="s in subs.filter((x) => x.last_error)" :key="s.id">
          <b>{{ s.remark || s.id }}</b>：{{ s.last_error }}
        </p>
      </div>
    </Card>

    <template v-if="tab === 'outbounds'">
    <!-- 新建 / 编辑 -->
    <Modal v-model="modalOpen" :title="editing ? `编辑出站 ${editing.tag}` : (isRelaySource ? '新建中转出口（本站服务器）' : '新建出站')" width="560px">
      <div class="an-form">
        <!-- 目标来源：仅新建时可选；编辑走结构化表单（中转出站编辑意义有限，引导到列表重删） -->
        <template v-if="!editing">
          <label class="an-field">
            <span>目标来源</span>
            <Select v-model="form.target_source" :options="targetSourceOptions" />
          </label>
          <template v-if="isRelaySource">
            <label class="an-field">
              <span>目标服务器 · 入站（自动读取对方面板凭据/传输参数）</span>
              <Select v-model="form.relay_target" :options="relayTargetOptions" placeholder="选择服务器与入站" />
            </label>
            <p v-if="relayTargetHint" class="an-warp-hint">{{ relayTargetHint }}</p>
            <!-- 标识（tag）：留空用默认「服务器名-端口-入站名」；重名直接报错，不自动加后缀 -->
            <div class="an-grid">
              <label class="an-field">
                <span>标识（留空自动生成，可改；允许中文）</span>
                <Input v-model="form.tag" placeholder="如：香港01-51177-联通优化" />
              </label>
              <label class="an-field">
                <span>备注（可选，列表展示用）</span>
                <Input v-model="form.remark" placeholder="如：香港中转" />
              </label>
            </div>
          </template>
        </template>

        <template v-if="!isRelaySource">
        <label class="an-field">
          <span>粘贴分享链接解析（vless/ vmess/ trojan/ ss/ socks/ http，回车导入并回填下方表单）</span>
          <Input
            v-model="formLink"
            :disabled="formImporting"
            placeholder="vmess://.... 或 vless://....（可选）"
            @keyup.enter="applyLink"
          />
        </label>

        <div class="an-grid">
          <label v-if="!isWarp" class="an-field">
            <span>标识 tag（同节点内唯一，路由按它引用）</span>
            <Input v-model="form.tag" placeholder="如 relay-hk" />
          </label>
          <label class="an-field">
            <span>协议</span>
            <Select v-model="form.protocol" :options="protocolOptions" />
          </label>
        </div>

        <p v-if="isWarp" class="an-warp-hint">
          保存时面板会自动向 Cloudflare 注册 WARP 设备并生成 wireguard 出站（tag=warp，秒级下发节点）。
          换 IP / 查看状态请到 WARP 面板。
        </p>

        <template v-if="isServerProto">
          <div class="an-grid">
            <label class="an-field">
              <span>服务器地址</span>
              <Input v-model="form.address" placeholder="1.2.3.4 / host.example.com" />
            </label>
            <label class="an-field">
              <span>端口</span>
              <Input v-model.number="form.port" type="number" placeholder="443" />
            </label>
          </div>
          <div v-if="isUuidProto" class="an-grid">
            <label class="an-field">
              <span>用户 ID（UUID）</span>
              <Input v-model="form.user_id" placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" />
            </label>
            <label class="an-field">
              <span>加密（VMess security）</span>
              <Input v-model="form.encryption" placeholder="auto" />
            </label>
          </div>
          <div v-if="form.protocol === 'vless'" class="an-grid">
            <label class="an-field">
              <span>flow（可选）</span>
              <Input v-model="form.flow" placeholder="xtls-rprx-vision" />
            </label>
          </div>
          <div v-if="form.protocol === 'shadowsocks'" class="an-grid">
            <label class="an-field">
              <span>加密方式</span>
              <Select v-model="form.method" :options="ssMethods" />
            </label>
            <label class="an-field">
              <span>密码</span>
              <Input v-model="form.password" type="password" placeholder="连接凭据" />
            </label>
          </div>
          <div v-if="['trojan', 'socks', 'http'].includes(form.protocol)" class="an-grid">
            <label v-if="['socks', 'http'].includes(form.protocol)" class="an-field">
              <span>用户名（可选）</span>
              <Input v-model="form.username" />
            </label>
            <label class="an-field">
              <span>密码</span>
              <Input v-model="form.password" type="password" placeholder="连接凭据" />
            </label>
          </div>
          <div v-if="isHyOut" class="an-grid">
            <label class="an-field">
              <span>auth（对端 hysteria2 的认证串）</span>
              <Input v-model="form.password" type="password" placeholder="对端面板里该用户的凭据" />
            </label>
            <label class="an-field">
              <span>TLS serverName</span>
              <Input v-model="form.tls_server_name" placeholder="对端证书域名" />
            </label>
          </div>
          <p v-if="isHyOut" class="an-hint">
            Hysteria2 出口固定走 QUIC + TLS（没有 vless 那套传输/安全层选择）。地址、端口、auth 之外
            只需要对端证书的 serverName；对端是自签证书时，可在「高级」里用 advanced_stream 补
            <code>tlsSettings.allowInsecure</code>。
          </p>

          <div v-if="isDnsOut" class="an-grid">
            <label class="an-field">
              <span>查询传输</span>
              <Select v-model="form.dns_network" :options="dnsNetworkOptions" />
            </label>
          </div>
          <p v-if="isDnsOut" class="an-hint">
            DNS 出口：把匹配到的域名查询直接发给这里填的解析服务器（xray 的 dns 出站）。
            地址填解析服务器 IP/域名，端口默认 53；没有传输层与安全层选项。
          </p>

          <div v-if="!isHyOut && !isDnsOut" class="an-grid">
            <label class="an-field">
              <span>传输</span>
              <Select v-model="form.network" :options="networkOptions" />
            </label>
            <label class="an-field">
              <span>安全层</span>
              <Select v-model="form.security" :options="securityOptions" />
            </label>
          </div>
          <div v-if="!isDnsOut && form.network === 'tcp'" class="an-grid">
            <label class="an-field">
              <span>TCP Header 伪装</span>
              <Select v-model="form.tcp_header_type" :options="tcpHeaderOptions" />
            </label>
            <template v-if="form.tcp_header_type === 'http'">
              <label class="an-field">
                <span>伪装路径</span>
                <Input v-model="form.tcp_header_path" placeholder="/" />
              </label>
              <label class="an-field">
                <span>伪装 Host（可选）</span>
                <Input v-model="form.tcp_header_host" placeholder="如 www.bing.com" />
              </label>
            </template>
          </div>
          <div v-if="!isDnsOut && form.network === 'ws'" class="an-grid">
            <label class="an-field">
              <span>WS 路径</span>
              <Input v-model="form.ws_path" placeholder="/" />
            </label>
            <label class="an-field">
              <span>WS Host / SNI</span>
              <Input v-model="form.ws_host" placeholder="可选" />
            </label>
          </div>
          <div v-if="!isDnsOut && form.network === 'httpupgrade'" class="an-grid">
            <label class="an-field">
              <span>HTTPUpgrade 路径</span>
              <Input v-model="form.httpupgrade_path" placeholder="/" />
            </label>
            <label class="an-field">
              <span>HTTPUpgrade Host（可选）</span>
              <Input v-model="form.httpupgrade_host" placeholder="留空不校验" />
            </label>
          </div>
          <div v-if="!isDnsOut && form.network === 'xhttp'" class="an-grid">
            <label class="an-field">
              <span>xhttp 路径</span>
              <Input v-model="form.xhttp_path" placeholder="/" />
            </label>
            <label class="an-field">
              <span>xhttp Host（可选）</span>
              <Input v-model="form.xhttp_host" placeholder="留空不校验" />
            </label>
            <label class="an-field">
              <span>xhttp 模式</span>
              <Input v-model="form.xhttp_mode" placeholder="auto / packet-up / stream-up / stream-one" />
            </label>
          </div>
          <template v-if="!isDnsOut && form.network === 'xhttp'">
            <p class="an-hint">xhttp 调优（留空 = 内核默认、不落库；勿填 1000000 / 30 等内核默认值会形成指纹）</p>
            <div class="an-grid">
              <label class="an-field">
                <span>Padding 字节</span>
                <Input v-model="form.xhttp_padding_bytes" placeholder="如 100-1000" />
              </label>
              <label class="an-field">
                <span>最大上传大小(字节)</span>
                <Input v-model="form.xhttp_sc_max_each_post_bytes" placeholder="留空 = 默认" />
              </label>
              <label class="an-field">
                <span>最大缓冲上传</span>
                <Input v-model="form.xhttp_sc_max_buffered_posts" type="number" placeholder="0 = 默认" />
              </label>
              <label class="an-field">
                <span>最小上传间隔 (ms)</span>
                <Input v-model="form.xhttp_sc_min_posts_interval_ms" placeholder="如 50-150" />
              </label>
              <label class="an-field">
                <span>服务器最大头字节</span>
                <Input v-model="form.xhttp_server_max_header_bytes" type="number" placeholder="0 = 默认" />
              </label>
              <label class="an-field">
                <span>Stream-Up 服务端秒数</span>
                <Input v-model="form.xhttp_sc_stream_up_server_secs" placeholder="如 20-80" />
              </label>
              <label class="an-field">
                <span>上行 HTTP 方法</span>
                <Input v-model="form.xhttp_uplink_http_method" placeholder="POST / PUT / GET" />
              </label>
            </div>
            <div class="an-grid">
              <label class="an-field-row">
                <input v-model="form.xhttp_no_sse_header" type="checkbox" />
                <span>noSSEHeader（禁用 SSE 头）</span>
              </label>
              <label class="an-field-row">
                <input v-model="form.xhttp_padding_obfs_mode" type="checkbox" />
                <span>Padding 混淆模式</span>
              </label>
            </div>
            <template v-if="form.xhttp_padding_obfs_mode">
              <div class="an-grid">
                <label class="an-field">
                  <span>Padding Key</span>
                  <Input v-model="form.xhttp_padding_key" placeholder="x_padding" />
                </label>
                <label class="an-field">
                  <span>Padding Header</span>
                  <Input v-model="form.xhttp_padding_header" placeholder="X-Padding" />
                </label>
                <label class="an-field">
                  <span>Padding Placement</span>
                  <Input v-model="form.xhttp_padding_placement" placeholder="queryInHeader / header / cookie / query" />
                </label>
                <label class="an-field">
                  <span>Padding Method</span>
                  <Input v-model="form.xhttp_padding_method" placeholder="repeat-x / tokenish" />
                </label>
              </div>
            </template>
            <p class="an-hint">会话/序号伪装（留空 = 默认 path 放置）</p>
            <div class="an-grid">
              <label class="an-field">
                <span>sessionID 放置</span>
                <Input v-model="form.xhttp_session_id_placement" placeholder="path / header / cookie / query" />
              </label>
              <label class="an-field">
                <span>sessionID Key</span>
                <Input v-model="form.xhttp_session_id_key" placeholder="x_session" />
              </label>
              <label class="an-field">
                <span>sessionID Table</span>
                <Input v-model="form.xhttp_session_id_table" placeholder="Base62 / hex / 自定义字符集" />
              </label>
              <label class="an-field">
                <span>sessionID 长度</span>
                <Input v-model="form.xhttp_session_id_length" placeholder="如 8-16" />
              </label>
              <label class="an-field">
                <span>seq 放置</span>
                <Input v-model="form.xhttp_seq_placement" placeholder="path / header / cookie / query" />
              </label>
              <label class="an-field">
                <span>seq Key</span>
                <Input v-model="form.xhttp_seq_key" placeholder="x_seq" />
              </label>
              <label class="an-field">
                <span>uplinkData 放置</span>
                <Input v-model="form.xhttp_uplink_data_placement" placeholder="auto / body / header / cookie" />
              </label>
              <label class="an-field">
                <span>uplinkData Key</span>
                <Input v-model="form.xhttp_uplink_data_key" placeholder="x_data" />
              </label>
            </div>
            <label class="an-field">
              <span>请求头（每行一个，Key: Value）</span>
              <textarea v-model="form.xhttp_headers" class="an-textarea" rows="3" placeholder="X-Custom: value" />
            </label>
            <label class="an-field-row">
              <input v-model="form.xhttp_xmux_enabled" type="checkbox" />
              <span>连接复用（XMUX）</span>
            </label>
            <div v-if="form.xhttp_xmux_enabled" class="an-grid">
              <label class="an-field">
                <span>maxConcurrency</span>
                <Input v-model="form.xhttp_xmux_max_concurrency" placeholder="16-32" />
              </label>
              <label class="an-field">
                <span>maxConnections</span>
                <Input v-model="form.xhttp_xmux_max_connections" placeholder="0（与 maxConcurrency 互斥）" />
              </label>
              <label class="an-field">
                <span>cMaxReuseTimes</span>
                <Input v-model="form.xhttp_xmux_c_max_reuse_times" placeholder="0" />
              </label>
              <label class="an-field">
                <span>hMaxRequestTimes</span>
                <Input v-model="form.xhttp_xmux_h_max_request_times" placeholder="600-900" />
              </label>
              <label class="an-field">
                <span>hMaxReusableSecs</span>
                <Input v-model="form.xhttp_xmux_h_max_reusable_secs" placeholder="1800-3000" />
              </label>
              <label class="an-field">
                <span>hKeepAlivePeriod</span>
                <Input v-model="form.xhttp_xmux_h_keep_alive_period" placeholder="0" />
              </label>
            </div>
          </template>
          <label v-if="form.security === 'tls'" class="an-field">
            <span>TLS serverName</span>
            <Input v-model="form.tls_server_name" placeholder="对端证书域名" />
          </label>
          <template v-if="form.security === 'reality'">
            <div class="an-grid">
              <label class="an-field">
                <span>Reality 公钥 publicKey</span>
                <Input v-model="form.reality_public_key" placeholder="对端 publicKey" />
              </label>
              <label class="an-field">
                <span>Reality serverName</span>
                <Input v-model="form.reality_server_name" placeholder="对端 SNI（如 www.amd.com）" />
              </label>
            </div>
            <div class="an-grid">
              <label class="an-field">
                <span>shortId（可选）</span>
                <Input v-model="form.reality_short_id" placeholder="留空则不带" />
              </label>
              <label class="an-field">
                <span>指纹 fingerprint</span>
                <Input v-model="form.reality_fingerprint" placeholder="chrome" />
              </label>
            </div>
            <label class="an-field">
              <span>spiderX（可选）</span>
              <Input v-model="form.reality_spider_x" placeholder="/" />
            </label>
          </template>
        </template>

        <template v-if="isWireguard">
          <label class="an-field">
            <span>WireGuard 私钥{{ editing && editing.has_secret_key ? '（不改留空）' : '' }}</span>
            <Input v-model="wgForm.wg_secret_key" placeholder="base64 私钥" />
          </label>
          <div class="an-grid">
            <label class="an-field">
              <span>接口地址（逗号分隔）</span>
              <Input v-model="wgForm.wg_address" placeholder="172.16.0.2/32" />
            </label>
            <label class="an-field">
              <span>Peer 公钥</span>
              <Input v-model="wgForm.wg_peer_public_key" placeholder="对端 publicKey" />
            </label>
          </div>
          <div class="an-grid">
            <label class="an-field">
              <span>Peer 端点</span>
              <Input v-model="wgForm.wg_peer_endpoint" placeholder="1.2.3.4:2408" />
            </label>
            <label class="an-field">
              <span>reserved（逗号分隔，可选）</span>
              <Input v-model="wgForm.wg_reserved" placeholder="0,0,0" />
            </label>
          </div>
        </template>

        <div v-if="!isWarp" class="an-grid">
          <label class="an-field">
            <span>备注（可选）</span>
            <Input v-model="form.remark" placeholder="如：香港中转" />
          </label>
          <label class="an-field-row">
            <input v-model="form.enabled" type="checkbox" />
            <span>启用</span>
          </label>
        </div>

        <label v-if="!isWireguard && !isWarp" class="an-field">
          <span>借道出站（可选：本出站的连接先经同节点另一出站拨出，实现「中转链」）</span>
          <Select
            v-model="form.dialer_proxy"
            :options="[{ label: '不借道（直连本出站服务器）', value: '' }, ...dialerCandidates]"
            placeholder="选择借道出站"
          />
        </label>

        <div class="an-advanced">
          <button class="an-advanced-toggle" type="button" @click="advancedOpen = !advancedOpen">
            <Icon :name="advancedOpen ? 'chevron-down' : 'chevron-right'" :size="14" />
            高级：直接编辑 JSON（UI 未覆盖的参数走这里）
          </button>
          <template v-if="advancedOpen">
            <label class="an-field">
              <span>settings JSON</span>
              <textarea v-model="form.advanced_settings" class="an-textarea" rows="3" placeholder='{"vnext": [...]}（非空时覆盖上方表单）' />
            </label>
            <label class="an-field">
              <span>streamSettings JSON</span>
              <textarea v-model="form.advanced_stream" class="an-textarea" rows="3" placeholder='{"network":"ws", ...}' />
            </label>
          </template>
        </div>
        </template>
      </div>
      <template #footer>
        <Button variant="ghost" @click="modalOpen = false">取消</Button>
        <Button variant="primary" :loading="saving" :success="saved" @click="save">{{ editing ? '保存' : (isRelaySource ? '创建中转出口' : '创建') }}</Button>
      </template>
    </Modal>

    <!-- 从链接导入 -->
    <Modal v-model="importOpen" title="从分享链接导入出站" width="520px">
      <div class="an-form">
        <label class="an-field">
          <span>粘贴链接（vless:// vmess:// trojan:// ss:// socks:// http://）</span>
          <textarea v-model="importLink" class="an-textarea" rows="3" placeholder="vmess://.... / vless://...." />
        </label>
        <Button variant="secondary" size="sm" :loading="importing" @click="doImport">解析</Button>
        <div v-if="importPreview" class="an-preview">
          <div class="an-preview-row"><span>协议</span><b>{{ protocolLabel[importPreview.protocol] || importPreview.protocol }}</b></div>
          <div class="an-preview-row"><span>标识</span><b class="mono">{{ importPreview.tag }}</b></div>
          <div class="an-preview-row">
            <span>目标</span>
            <b class="mono">
              {{ importPreview.settings?.vnext?.[0]?.address || importPreview.settings?.servers?.[0]?.address || '—' }}:{{
                importPreview.settings?.vnext?.[0]?.port || importPreview.settings?.servers?.[0]?.port || '—'
              }}
            </b>
          </div>
          <div class="an-preview-row">
            <span>传输</span><b class="mono">{{ importPreview.stream_settings?.network || 'tcp' }} / {{ importPreview.stream_settings?.security || 'none' }}</b>
          </div>
        </div>
      </div>
      <template #footer>
        <Button variant="ghost" @click="importOpen = false">取消</Button>
        <Button variant="primary" :disabled="!importPreview" @click="confirmImport">导入</Button>
      </template>
    </Modal>
    </template>

    <!-- 订阅 新建 / 编辑 -->
    <Modal v-model="subModalOpen" :title="subEditing ? '编辑订阅' : '新建出站订阅'" width="560px">
      <div class="an-form">
        <label class="an-field">
          <span>订阅地址（仅 http/https；指向内网/保留地址会被拒绝）</span>
          <Input v-model="subForm.url" placeholder="https://example.com/sub?token=..." />
        </label>
        <div class="an-grid">
          <label class="an-field">
            <span>备注（可选）</span>
            <Input v-model="subForm.remark" placeholder="如：机场A" />
          </label>
          <label class="an-field">
            <span>标识前缀（留空用 sub&lt;id&gt;-）</span>
            <Input v-model="subForm.tag_prefix" placeholder="如 pool-" />
          </label>
        </div>
        <div class="an-grid">
          <label class="an-field">
            <span>刷新间隔（秒，≥60）</span>
            <Input v-model.number="subForm.update_interval" type="number" placeholder="600" />
          </label>
          <label class="an-field-row">
            <input v-model="subForm.prepend" type="checkbox" />
            <span>前置注入（排在自定义出站之前）</span>
          </label>
        </div>
        <label class="an-field-row">
          <input v-model="subForm.enabled" type="checkbox" />
          <span>启用</span>
        </label>
      </div>
      <template #footer>
        <Button variant="ghost" @click="subModalOpen = false">取消</Button>
        <Button variant="primary" :loading="subSaving" :success="subSaved" @click="saveSub">保存</Button>
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
  gap: var(--space-4);
  margin-bottom: var(--space-3);
}
.an-tabs {
  display: flex;
  gap: var(--space-1);
}
.an-tab {
  background: none;
  border: none;
  border-bottom: 2px solid transparent;
  padding: var(--space-2) var(--space-3);
  color: var(--text-muted);
  font-size: var(--text-sm);
  cursor: pointer;
}
.an-tab.active {
  color: var(--text-primary);
  border-bottom-color: var(--accent, var(--color-primary, #6366f1));
}
.an-badge-gap {
  margin-left: var(--space-1);
}
.an-suberr {
  padding: var(--space-3) var(--space-4);
  border-top: 1px solid var(--border-subtle);
  color: var(--text-muted);
  font-size: var(--text-xs);
}
.an-suberr p {
  margin: 0 0 var(--space-1);
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
.an-field-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--text-secondary);
  cursor: pointer;
}
.an-advanced-toggle {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  background: none;
  border: none;
  color: var(--text-muted);
  font-size: var(--text-xs);
  cursor: pointer;
  padding: 0;
}
.an-advanced {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
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
.an-preview {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-3);
  background: var(--bg-input);
  border-radius: var(--radius-md);
  font-size: var(--text-sm);
}
.an-warp-hint {
  margin: 0;
  padding: var(--space-2) var(--space-3);
  background: var(--bg-input);
  border-radius: var(--radius-md);
  color: var(--text-muted);
  font-size: var(--text-xs);
}
.an-preview-row {
  display: flex;
  justify-content: space-between;
  gap: var(--space-2);
}
.an-preview-row span {
  color: var(--text-muted);
}
.mono {
  font-family: var(--font-mono);
}

/* 表单栅格对齐：标签统一占两行高度，长标签换行时不会把输入框挤下去 */
.an-grid { align-items: start; }
.an-grid > .an-field > span:first-child { display: block; min-height: 2.6em; line-height: 1.35; }
</style>
