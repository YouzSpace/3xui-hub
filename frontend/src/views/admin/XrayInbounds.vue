<script setup>
/**
 * xray 节点配置 · 入站管理。
 *
 * 协议面板自建（vless / vmess / trojan / shadowsocks），
 * 传输 tcp/ws/grpc，安全 none/tls/reality（reality 仅 VLESS）。
 * Reality 密钥由面板生成（与 xray x25519 同格式），私钥只进 config 下发。
 */
import { ref, computed, watch } from 'vue'
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
const nodeId = ref(null)
const inbounds = ref([])
const loading = ref(false)
// 端口跳跃的节点级能力（issue #94）：agent ≥ 1.0.7 才认那四个 action。
const hopSupported = ref(false)
const hopMinAgentVersion = ref('1.0.7')
// 多选删除用的勾选集合（只存 id，切节点清空）
const selectedIds = ref([])

const modalOpen = ref(false)
const editing = ref(null)
const saving = ref(false)
const saved = ref(false)
const generating = ref(false)

// 与后端白名单完全一致（handler/admin/xray_inbound.go：in:vless,vmess,trojan,shadowsocks,hysteria,socks,http,tunnel,wireguard）。
// 多一项会让表单提交被后端 422 打回，少一项则协议建不出来。
const protocolOptions = [
  { label: 'VLESS（支持 Reality）', value: 'vless' },
  { label: 'VMess', value: 'vmess' },
  { label: 'Trojan', value: 'trojan' },
  { label: 'Shadowsocks', value: 'shadowsocks' },
  { label: 'Hysteria2（QUIC）', value: 'hysteria' },
  { label: 'SOCKS5 代理', value: 'socks' },
  { label: 'HTTP 代理', value: 'http' },
  { label: 'Tunnel（dokodemo-door 透明转发）', value: 'tunnel' },
  { label: 'WireGuard（UDP）', value: 'wireguard' },
]
const NETWORK_ALL = [
  { label: 'TCP', value: 'tcp' },
  { label: 'WebSocket', value: 'ws' },
  { label: 'gRPC', value: 'grpc' },
  { label: 'xhttp', value: 'xhttp' },
  { label: 'HTTPUpgrade', value: 'httpupgrade' },
]
// REALITY 支持面 = tcp / xhttp / grpc（xray-core 25+；ws/httpupgrade 无 TLS 层不搭 reality）
const REALITY_NETWORKS = ['tcp', 'xhttp', 'grpc']
const tcpHeaderOptions = [
  { label: '无', value: 'none' },
  { label: 'HTTP 伪装', value: 'http' },
]
// masquerade：string 直接回固定文本，file/proxy 走别的形态（内核支持，面板先只放两种常用）
const hyMasqueradeOptions = [
  { label: '不伪装（内核默认）', value: '' },
  { label: 'string（固定文本）', value: 'string' },
  { label: 'file（固定目录）', value: 'file' },
  { label: 'proxy（反代到 URL）', value: 'proxy' },
]
// HY2 的 QUIC 拥塞控制（内核自带，与系统 TCP BBR 是两回事）
const hyCongestionOptions = [
  { label: '内核默认（bbr）', value: '' },
  { label: 'reno', value: 'reno' },
  { label: 'bbr', value: 'bbr' },
  { label: 'brutal', value: 'brutal' },
  { label: 'force-brutal', value: 'force-brutal' },
]
const ssMethodOptions = [
  { label: 'aes-256-gcm', value: 'aes-256-gcm' },
  { label: 'aes-128-gcm', value: 'aes-128-gcm' },
  { label: 'chacha20-poly1305', value: 'chacha20-poly1305' },
  { label: 'xchacha20-poly1305', value: 'xchacha20-poly1305' },
  { label: '2022-blake3-aes-256-gcm', value: '2022-blake3-aes-256-gcm' },
  { label: '2022-blake3-aes-128-gcm', value: '2022-blake3-aes-128-gcm' },
]
// 嗅探协议（issue #39；与 xray SniffingConfig.destOverride 口径一致）
const sniffDestOptions = [
  { label: 'HTTP', value: 'http' },
  { label: 'TLS', value: 'tls' },
  { label: 'QUIC', value: 'quic' },
  { label: 'FakeDNS（需配合 FakeDNS 使用）', value: 'fakedns' },
]
// uTLS 指纹（3x-ui UTLS_FINGERPRINT 全集，前四个最常用）
const fingerprintOptions = [
  { label: 'chrome', value: 'chrome' },
  { label: 'firefox', value: 'firefox' },
  { label: 'safari', value: 'safari' },
  { label: 'ios', value: 'ios' },
  { label: 'android', value: 'android' },
  { label: 'edge', value: 'edge' },
  { label: '360', value: '360' },
  { label: 'qq', value: 'qq' },
  { label: 'random', value: 'random' },
  { label: 'randomized', value: 'randomized' },
  { label: 'randomizednoalpn', value: 'randomizednoalpn' },
  { label: 'unsafe', value: 'unsafe' },
]

const emptyLimitFallback = () => ({ after_bytes: 0, bytes_per_sec: 0, burst_bytes_per_sec: 0 })

const emptyForm = () => ({
  protocol: 'vless',
  tag: '',
  remark: '',
  port: 443,
  listen: '0.0.0.0',
  network: 'tcp',
  security: 'reality',
  dest: 'www.cloudflare.com:443',
  server_names: 'www.cloudflare.com',
  short_ids: '',
  private_key: '',
  public_key: '',
  // reality 高级参数（服务端 + 客户端段）
  show: false,
  xver: 0,
  min_client_ver: '',
  max_client_ver: '',
  max_timediff: 0,
  mldsa65_seed: '',
  mldsa65_verify: '',
  master_key_log: '',
  fingerprint: 'chrome',
  spider_x: '/',
  limit_fallback_upload: emptyLimitFallback(),
  limit_fallback_download: emptyLimitFallback(),
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
  tls_certificate: '',
  tls_key: '',
  ss_method: 'aes-256-gcm',
  ss_password: '',
  // socks / http（传统代理）：单组账号密码，密码编辑时留空 = 不改
  auth_user: '',
  auth_pass: '',
  // tunnel（dokodemo-door）：转发目标 + L4 网络（tcp / udp / tcp,udp）
  tunnel_address: '',
  tunnel_port: 0,
  tunnel_network: 'tcp,udp',
  // wireguard：服务端私钥由面板生成且不回显，表单只填 MTU 与对端公钥清单
  wg_mtu: 1420,
  wg_peers: '',
  // hysteria2（主端口就是上面的 port）：跳跃区间/伪装/拥塞控制/证书引用
  hy_hop_ports: '',
  hy_hop_interval: '',
  // 默认 60（内核默认值）：留 0 会被后端 min 校验拦下（历史 bug），这里直接给合法默认值
  hy_udp_idle_timeout: 60,
  hy_congestion: '',
  hy_masquerade_type: '',
  hy_masquerade_url: '',
  hy_masquerade_dir: '',
  hy_masquerade_content: '',
  hy_masquerade_status_code: 0,
  // hy2 高级 TLS 开关 + QUIC 调优（0/不勾 = 不写，走内核默认）
  hy_reject_unknown_sni: false,
  hy_session_resumption: false,
  hy_brutal_up: 0,
  hy_brutal_down: 0,
  hy_init_stream_window: 0,
  hy_max_stream_window: 0,
  hy_init_conn_window: 0,
  hy_max_conn_window: 0,
  // salamander 混淆密码（issue #94）：加密存独立列，编辑时留空=不改、勾「清除」=清掉
  hy_salamander_password: '',
  hy_salamander_clear: false,
  tls_cert_id: null,
  // 嗅探（issue #39）：默认值与后端现状一致（enabled + http/tls/quic）
  sniffing_enabled: true,
  sniffing_dest: ['http', 'tls', 'quic'],
  sniffing_route_only: false,
  enabled: true,
})
const form = ref(emptyForm())
// 高级参数折叠面板（新建/编辑时不打扰）
const advOpen = ref(false)

const isReality = computed(() => form.value.protocol === 'vless' && form.value.security === 'reality')

// ── 地址族（issue #88）─────────────────────────────────────────
// listen 一直是字段（后端 schema 里就有），但以前前端固定传 0.0.0.0，用户无从选择：
// 一台机器有 v6 时入站只能绑 v4，v6 用户拿不到入口。这里把它暴露成「地址族」下拉。
//
// 老数据兼容：listen 是这两个值之外的内容（历史手工值 / 具体 IP / unix socket）
// 时，额外给一个「保留原值」选项 —— 未动的老入站提交后仍是原值，不会被悄悄改写成 0.0.0.0。
const ADDRESS_FAMILIES = [
  { label: 'IPv4（0.0.0.0，默认）', value: '0.0.0.0' },
  { label: 'IPv6（::，同时接受 IPv4-mapped）', value: '::' },
]
const listenOptions = computed(() => {
  const cur = form.value.listen
  const known = ADDRESS_FAMILIES.some((o) => o.value === cur)
  if (known || !cur) return ADDRESS_FAMILIES
  return [...ADDRESS_FAMILIES, { label: `保留原值（${cur}）`, value: cur }]
})
const listenFamily = computed({
  get: () => form.value.listen,
  set: (v) => { form.value.listen = v },
})
const isTls = computed(() => form.value.security === 'tls')
const isWs = computed(() => form.value.network === 'ws')
const isXhttp = computed(() => form.value.network === 'xhttp')
const isHttpupgrade = computed(() => form.value.network === 'httpupgrade')
const isSs = computed(() => form.value.protocol === 'shadowsocks')
// SS-2022（2022-blake3-aes-*）：服务端密钥必须是 base64 固定长度（aes-256 32 字节 /
// aes-128 16 字节），留空或长度不对后端会自动生成一把合法的。所以这一档用明文展示：
// 它本来就明文出现在每个用户的订阅链接里（ss://method:服务端PSK:用户PSK），
// 生成后的值管理员要能看到/复制，否则没法核对。
const ssIs2022 = computed(() => String(form.value.ss_method || '').startsWith('2022-blake3-'))
// 传统代理（socks / http）：单组账号密码，没有用户概念
const isSocks = computed(() => form.value.protocol === 'socks')
const isHttp = computed(() => form.value.protocol === 'http')
const isProxyProto = computed(() => isSocks.value || isHttp.value)
// tunnel（dokodemo-door）：把流量原样转给固定目标，L4 网络在 settings.network
const isTunnel = computed(() => form.value.protocol === 'tunnel')
// wireguard：UDP 监听、自带加密，服务端私钥由面板生成
const isWg = computed(() => form.value.protocol === 'wireguard')
// 传输下拉：wireguard / tunnel 没有可选的传输层（wireguard 固定 UDP，tunnel 走 L4 直转）
const hideTransport = computed(() => isWg.value || isTunnel.value)
// 安全层下拉：上面两种 + 传统代理都是「无附加安全层」
const hideSecurity = computed(() => hideTransport.value || isProxyProto.value)
// hysteria2：QUIC 传输，强制 TLS（跳过了 TLS 就没有加密层），证书走证书库引用
const isHy = computed(() => form.value.protocol === 'hysteria')
// brutal 带宽只在这两种拥塞算法下有意义（后端同样只在这两种下写入 quicParams）
const isHyBrutal = computed(
  () => isHy.value && ['brutal', 'force-brutal'].includes(form.value.hy_congestion),
)

// 传输选项：Reality 下仅保留受支持项（tcp/xhttp/grpc）；hy2 固定 hysteria（QUIC）
const networkOptions = computed(() => {
  if (isHy.value) return [{ label: 'Hysteria（QUIC）', value: 'hysteria' }]
  if (isReality.value) return NETWORK_ALL.filter((o) => REALITY_NETWORKS.includes(o.value))
  return NETWORK_ALL
})

// tunnel 的 L4 网络（写内核 settings.network；值里的逗号是内核口径，不是列表分隔）
const tunnelNetworkOptions = [
  { label: 'TCP + UDP（推荐）', value: 'tcp,udp' },
  { label: '仅 TCP', value: 'tcp' },
  { label: '仅 UDP', value: 'udp' },
]

// 安全层选项随协议变化（reality 仅 vless；ss 固定无；hy2 固定 tls）
const securityOptions = computed(() => {
  if (isHy.value) return [{ label: 'TLS（HY2 强制）', value: 'tls' }]
  if (isSs.value) return [{ label: '无（SS 自带加密）', value: 'none' }]
  if (form.value.protocol === 'vless') {
    return [
      { label: 'Reality（推荐）', value: 'reality' },
      { label: 'TLS', value: 'tls' },
      { label: '无', value: 'none' },
    ]
  }
  return [
    { label: 'TLS', value: 'tls' },
    { label: '无', value: 'none' },
  ]
})

// Reality 仅 tcp/xhttp/grpc；ss 固定 tcp+none；hy2 固定 hysteria+tls；
// socks/http/tunnel/wireguard 没有安全层也没有用户 —— 切换协议/安全时自动收敛，
// 避免残留非法组合被后端打回
watch(
  () => [form.value.protocol, form.value.security],
  () => {
    if (isReality.value && !REALITY_NETWORKS.includes(form.value.network)) form.value.network = 'tcp'
    if (isSs.value) {
      form.value.network = 'tcp'
      form.value.security = 'none'
    }
    if (isHy.value) {
      form.value.network = 'hysteria'
      form.value.security = 'tls'
    }
    if (isWg.value || isTunnel.value) {
      form.value.network = 'tcp'
      form.value.security = 'none'
    }
    if (isProxyProto.value) form.value.security = 'none'
  },
)

const load = async () => {
  if (!nodeId.value) {
    inbounds.value = []
    selectedIds.value = []
    return
  }
  loading.value = true
  try {
    const res = await adminApi.listXrayInbounds(nodeId.value)
    inbounds.value = res?.inbounds || []
    hopSupported.value = res?.hop_supported !== false
    if (res?.hop_min_agent_version) hopMinAgentVersion.value = res.hop_min_agent_version
    // 已经被别处删掉的码不能留在勾选里（否则批量删除会带上不存在的 id）
    const alive = new Set(inbounds.value.map((ib) => ib.id))
    selectedIds.value = selectedIds.value.filter((id) => alive.has(id))
  } catch (e) {
    toast.error(e.message || '加载失败')
    inbounds.value = []
    selectedIds.value = []
    hopSupported.value = false
  } finally {
    loading.value = false
  }
}
// 切节点要清空勾选：跨节点的 id 没有意义；节点 host 也要重取（HY2 证书按它做域名匹配）
watch(nodeId, () => { selectedIds.value = []; load(); loadNodeHost() })

/* ── 多选 / 批量删除（对齐 3x-ui 入站列表的 checkbox + bulk delete） ── */

const selectedCount = computed(() => selectedIds.value.length)
const allSelected = computed(() => inbounds.value.length > 0 && selectedIds.value.length === inbounds.value.length)

const isSelected = (ib) => selectedIds.value.includes(ib.id)

const toggleSelect = (ib) => {
  selectedIds.value = isSelected(ib)
    ? selectedIds.value.filter((id) => id !== ib.id)
    : [...selectedIds.value, ib.id]
}

const toggleSelectAll = () => {
  selectedIds.value = allSelected.value ? [] : inbounds.value.map((ib) => ib.id)
}

const batchRemoving = ref(false)

const removeSelected = async () => {
  const picked = inbounds.value.filter((ib) => selectedIds.value.includes(ib.id))
  if (!picked.length) return
  const list = picked.map((ib) => `${ib.tag}（端口 ${ib.port}）`).join('\n')
  if (!confirm(`确定删除选中的 ${picked.length} 个入站？\n${list}\n删除后不可恢复。`)) return

  batchRemoving.value = true
  try {
    const res = await adminApi.batchDeleteXrayInbounds(nodeId.value, picked.map((ib) => ib.id))
    const done = res?.deleted?.length || 0
    const failed = res?.failed?.length || 0
    if (failed) toast.error(`已删除 ${done} 个，${failed} 个失败（可能已被别处删除）`)
    else toast.success(`已删除 ${done} 个入站`)
    selectedIds.value = []
    await load()
  } catch (e) {
    toast.error(e.message || '批量删除失败')
  } finally {
    batchRemoving.value = false
  }
}

// ── TLS 证书库（HY2 的证书引用源）──────────────────────────────
// HY2 强制 TLS：入站只存证书库 id，私钥留在库里（不进 stream_settings）。
// 只在这里读取列表；新增/删除走证书库页面，避免把私钥表单塞进入站弹窗。
const certificates = ref([])

// 当前节点的主机名：HY2 的证书是按「入站 SNI → 节点域名」匹配的（后端 autoPickHysteriaCert
// 就是这两级），前端要按同一个域名给证书条目做匹配标注与自动填充。
// 节点选择器只回 id，这里按 id 再取一次拿 host。
const nodeHost = ref('')

// 与后端 nodeHostName 同口径：去 scheme / 路径 / 端口 / IPv6 方括号，转小写裸主机名。
const bareHost = (raw) => {
  let h = String(raw || '').trim()
  if (!h) return ''
  const scheme = h.indexOf('//')
  if (scheme >= 0) h = h.slice(scheme + 2)
  const cut = h.search(/[/?]/)
  if (cut >= 0) h = h.slice(0, cut)
  if (h.startsWith('[')) {
    const end = h.indexOf(']')
    if (end > 0) h = h.slice(1, end)
  } else if (h.split(':').length === 2) {
    h = h.slice(0, h.lastIndexOf(':'))
  }
  return h.toLowerCase()
}

const loadNodeHost = async () => {
  nodeHost.value = ''
  if (!nodeId.value) return
  try {
    const res = await adminApi.getNode(nodeId.value)
    nodeHost.value = bareHost(res?.host)
  } catch {
    // 拿不到节点域名时不做域名匹配标注：宁可少标，也不误标成「不匹配」
    nodeHost.value = ''
  }
}

// 与后端 certDomainMatches 同口径：完全一致，或 *.example.com 覆盖 a.example.com 与 example.com 本身
const certCoversHost = (certDomain, host) => {
  const d = String(certDomain || '').trim().toLowerCase()
  const h = String(host || '').trim().toLowerCase()
  if (!d || !h) return false
  if (d === h) return true
  if (d.startsWith('*.')) {
    const base = d.slice(2)
    return h === base || h.endsWith(`.${base}`)
  }
  return false
}

// 到期判定：not_after 缺失或解析不出来按「不知道」处理，不标已过期
const certExpired = (c) => {
  if (!c?.not_after) return false
  const t = Date.parse(c.not_after)
  return Number.isFinite(t) && t <= Date.now()
}

// source 是后端派生字段：local-dir = 本机证书目录自动刷新，manual = 手工导入/续签后需重导
const certSourceLabel = (source) => {
  if (source === 'local-dir') return '本机目录'
  if (source === 'manual') return '手工导入'
  return ''
}

// 要匹配的目标域名：优先表单里的 SNI（HY2 握手就按它），其次节点 host。
const certTargetHost = computed(() => {
  const sni = String(form.value.tls_server_name || '').trim().toLowerCase()
  return sni || nodeHost.value
})

// 证书能否覆盖目标域名。目标域名或证书域名任一未知时返回 true：
// 判断不了就不拦人（后端同样在两边都空时才兜底挑到期最晚的一张）。
const certFits = (c) => {
  const target = certTargetHost.value
  const dom = String(c?.domain || '').trim()
  if (!target || !dom) return true
  return certCoversHost(dom, target)
}

const certOptions = computed(() => [
  { label: '不引用（内核用其它方式提供证书）', value: null },
  ...certificates.value.map((c) => {
    const tags = []
    if (c.domain) tags.push(c.domain)
    const source = certSourceLabel(c.source)
    if (source) tags.push(source)
    if (c.not_after) tags.push(`${String(c.not_after).slice(0, 10)} 到期`)
    if (certExpired(c)) tags.push('已过期')
    // 域名覆盖不到的条目置灰：选了客户端按 SNI 握手必失败
    // （后端自动挑证书同样拒绝拿别的域名凑数，线上真踩过「入站看着正常、实际连不上」）
    const fits = certFits(c)
    if (!fits) tags.push('域名不匹配')
    return {
      label: `${c.name}${tags.length ? `（${tags.join(' · ')}）` : ''}`,
      value: c.id,
      disabled: !fits,
    }
  }),
])

// 选中项自己可能已经不匹配了（先选证书、后改 SNI）：显式点名，别让人以为一切正常
const selectedCertProblem = computed(() => {
  const id = form.value.tls_cert_id
  if (!id) return ''
  const c = certificates.value.find((x) => Number(x.id) === Number(id))
  if (!c) return ''
  const bad = []
  if (!certFits(c)) bad.push(`证书域名（${c.domain}）覆盖不到 ${certTargetHost.value}`)
  if (certExpired(c)) bad.push('证书已过期')
  return bad.length ? `当前选中的证书用不了：${bad.join('；')}` : ''
})

// 取到期最晚的一张（同域名多张时：换证后旧证还留在库里）
const latestExpiry = (list) =>
  list.reduce((best, c) => {
    const t = Date.parse(c?.not_after || '')
    const b = Date.parse(best?.not_after || '')
    return (Number.isFinite(t) ? t : 0) > (Number.isFinite(b) ? b : 0) ? c : best
  })

// 「自动填充证书」：① 节点 host 是域名时预填进 SNI（HY2 的 serverName 就是握手用的域名），
// ② 从证书库挑一张域名覆盖目标域名、未过期、证书+私钥齐全的条目填进下拉。
// 为什么让前端挑而不是留给后端 autoPick：后端只在请求没带 tls_cert_id 时才自动挑，
// 表单里显式选中才能让用户当场看见「用的是哪一张」，不会出现选了不引用、面板成功却握不上手。
const autoFillCert = () => {
  if (!isHy.value) return
  if (!certificates.value.length) {
    toast.error('证书库还是空的：请先到「TLS 证书库」添加一张再回来')
    return
  }
  const host = nodeHost.value
  const hostIsIp = /^\d{1,3}(\.\d{1,3}){3}$/.test(host) || host.includes(':')
  if (!String(form.value.tls_server_name || '').trim() && host && !hostIsIp) {
    form.value.tls_server_name = host
  }
  const target = certTargetHost.value
  const usable = certificates.value.filter((c) => c.has_cert !== false && c.has_key !== false && !certExpired(c))
  if (!usable.length) {
    toast.error('证书库里没有可用的证书：都已过期或缺少证书/私钥，请先到「TLS 证书库」补一张')
    return
  }
  if (!target) {
    // SNI 与节点 host 都空：没得匹配，退到最后到期的一张（与后端兜底一致），并说清原因
    const pick = latestExpiry(usable)
    form.value.tls_cert_id = pick.id
    toast.info(`没填 SNI、节点也没填域名：先按到期最晚选了 ${pick.name}，请把 serverName 补成证书域名`)
    return
  }
  const matched = usable.filter((c) => certCoversHost(c.domain, target))
  if (!matched.length) {
    toast.error(`证书库里没有覆盖 ${target} 的证书：先到「TLS 证书库」为该域名签/导一张，或手动选一张`)
    return
  }
  const pick = latestExpiry(matched)
  form.value.tls_cert_id = pick.id
  toast.success(`已填入证书：${pick.name}${pick.domain ? `（${pick.domain}）` : ''}`)
}

const loadCertificates = async () => {
  try {
    const res = await adminApi.listXrayCertificates()
    certificates.value = res?.certificates || []
  } catch (e) {
    certificates.value = []
  }
}

// ── 从证书库回填 TLS 入站的 PEM ────────────────────────────────
// TLS 入站（vless/trojan）读的是内联 PEM，不认证书库 id，所以「引用证书库」对它等于手贴。
// 这里补一条捷径：选中一张库里的证书 → 请求解密后的 cert_pem/key_pem → 填进两个框。
// 私钥只在这一个 admin 请求里回传，不进列表载荷，也没落进 stream_settings（保存时按内联 PEM 走）。
const pemCertId = ref(null)
const pemFilling = ref(false)

// 能回填的条目：证书+私钥齐全、没过期、域名覆盖得到目标域名（其余填了也握不上手）
const pemPickable = computed(() =>
  certificates.value.filter(
    (c) => c.has_cert !== false && c.has_key !== false && !certExpired(c) && certFits(c),
  ),
)

// 回填用的下拉：不复用 certOptions（那份首项是「不引用」，回填必须真选一张）
const pemCertOptions = computed(() =>
  certificates.value.map((c) => {
    const tags = []
    if (c.domain) tags.push(c.domain)
    if (c.not_after) tags.push(`${String(c.not_after).slice(0, 10)} 到期`)
    const complete = c.has_cert !== false && c.has_key !== false
    if (!complete) tags.push('缺证书或私钥')
    if (certExpired(c)) tags.push('已过期')
    if (!certFits(c)) tags.push('域名不匹配')
    return {
      label: `${c.name}${tags.length ? `（${tags.join(' · ')}）` : ''}`,
      value: c.id,
      disabled: !complete || certExpired(c) || !certFits(c),
    }
  }),
)

// 没手选时自动挑一张：域名能覆盖目标域名的条目里到期最晚的（与「自动填充证书」同口径）
const bestPemCert = () => {
  const usable = pemPickable.value
  if (!usable.length) return null
  const target = certTargetHost.value
  const matched = target ? usable.filter((c) => certCoversHost(c.domain, target)) : usable
  return latestExpiry(matched.length ? matched : usable)
}

const fillTlsPemFromLibrary = async () => {
  if (pemFilling.value) return
  if (!certificates.value.length) {
    toast.error('证书库还是空的：先到「TLS 证书库」添加一张，或用下面的「签发证书」签一张')
    return
  }
  const id = pemCertId.value || bestPemCert()?.id
  if (!id) {
    toast.error('证书库里没有能填的证书：都缺证书/私钥、已过期，或域名覆盖不到当前 serverName')
    return
  }
  pemFilling.value = true
  try {
    const res = await adminApi.getXrayCertificatePem(id)
    const certPem = String(res?.cert_pem || '').trim()
    const keyPem = String(res?.key_pem || '').trim()
    if (!certPem || !keyPem) {
      toast.error('证书库没回完整 PEM（缺证书或私钥），请先到「TLS 证书库」补齐')
      return
    }
    form.value.tls_certificate = certPem
    form.value.tls_key = keyPem
    pemCertId.value = id
    const picked = certificates.value.find((x) => Number(x.id) === Number(id))
    toast.success(`已从证书库填入证书与私钥${picked?.name ? `（${picked.name}）` : ''}`)
  } catch (e) {
    toast.error(e.message || '读取证书失败')
  } finally {
    pemFilling.value = false
  }
}

// ── 远程签发证书（面板下发 cert_issue，节点跑 acme.sh，成功后写回证书库）──
// 域名取节点 host：HTTP-01 要求 Let's Encrypt 能从 80 端口打到这台节点，IP 签不出来。
const nodeHostIsIp = computed(() => {
  const h = nodeHost.value
  return /^\d{1,3}(\.\d{1,3}){3}$/.test(h) || (h.includes(':') && !h.includes('.'))
})
const issuingCert = ref(false)

const issueNodeCert = async () => {
  if (issuingCert.value) return
  if (!nodeId.value || !nodeHost.value) {
    toast.error('先选节点，并确认节点地址填的是域名')
    return
  }
  if (nodeHostIsIp.value) {
    toast.error(`节点地址是 IP（${nodeHost.value}）：Let's Encrypt 不签 IP，先把节点地址改成域名`)
    return
  }
  issuingCert.value = true
  try {
    const res = await adminApi.issueXrayCertificate({
      node_id: Number(nodeId.value),
      domain: nodeHost.value,
      method: 'http-01',
    })
    await loadCertificates()
    // 新条目 id 优先取回执；后端只回路径时，按域名在刷新后的库里取到期最晚的那张
    let newId = res?.certificate?.id
    if (!newId) {
      const hit = certificates.value.filter((c) => certCoversHost(c.domain, nodeHost.value))
      newId = hit.length ? latestExpiry(hit).id : null
    }
    if (newId) {
      // HY2 认证书 id，TLS 入站认内联 PEM：两条路各自接上，用户不用再点一次
      if (isHy.value) form.value.tls_cert_id = newId
      else pemCertId.value = newId
    }
    const notAfter = res?.certificate?.not_after || res?.not_after
    toast.success(
      `已签发 ${nodeHost.value} 的证书${notAfter ? `（到期 ${String(notAfter).slice(0, 10)}）` : ''}，已写入证书库`,
    )
    if (newId && !isHy.value) await fillTlsPemFromLibrary()
  } catch (e) {
    toast.error(e.message || '签发失败')
  } finally {
    issuingCert.value = false
  }
}

const openCreate = () => {
  editing.value = null
  form.value = emptyForm()
  advOpen.value = false
  loadCertificates().then(() => {
    pemCertId.value = bestPemCert()?.id ?? null
  })
  modalOpen.value = true
}

const openEdit = (ib) => {
  editing.value = ib
  form.value = {
    protocol: ib.protocol,
    tag: ib.tag,
    remark: ib.remark || '',
    port: ib.port,
    listen: ib.listen || '0.0.0.0',
    network: ib.network || 'tcp',
    security: ib.security || 'none',
    dest: ib.dest || 'www.cloudflare.com:443',
    server_names: (ib.server_names || []).join(', '),
    short_ids: (ib.short_ids || []).join(', '),
    private_key: '',
    public_key: ib.public_key || '',
    show: !!ib.show,
    xver: ib.xver ?? 0,
    min_client_ver: ib.min_client_ver || '',
    max_client_ver: ib.max_client_ver || '',
    max_timediff: ib.max_timediff ?? 0,
    mldsa65_seed: ib.mldsa65_seed || '',
    mldsa65_verify: ib.mldsa65_verify || '',
    master_key_log: ib.master_key_log || '',
    fingerprint: ib.fingerprint || 'chrome',
    spider_x: ib.spider_x || '/',
    limit_fallback_upload: { ...emptyLimitFallback(), ...(ib.limit_fallback_upload || {}) },
    limit_fallback_download: { ...emptyLimitFallback(), ...(ib.limit_fallback_download || {}) },
    ws_path: ib.ws_path || '/',
    ws_host: ib.ws_host || '',
    httpupgrade_path: ib.httpupgrade_path || '/',
    httpupgrade_host: ib.httpupgrade_host || '',
    tcp_header_type: ib.tcp_header_type || 'none',
    tcp_header_path: ib.tcp_header_path || '/',
    tcp_header_host: ib.tcp_header_host || '',
    xhttp_path: ib.xhttp_path || '/',
    xhttp_host: ib.xhttp_host || '',
    xhttp_mode: ib.xhttp_mode || 'auto',
    xhttp_padding_bytes: ib.xhttp_padding_bytes || '',
    xhttp_padding_obfs_mode: !!ib.xhttp_padding_obfs_mode,
    xhttp_padding_key: ib.xhttp_padding_key || '',
    xhttp_padding_header: ib.xhttp_padding_header || '',
    xhttp_padding_placement: ib.xhttp_padding_placement || '',
    xhttp_padding_method: ib.xhttp_padding_method || '',
    xhttp_session_id_placement: ib.xhttp_session_id_placement || '',
    xhttp_session_id_key: ib.xhttp_session_id_key || '',
    xhttp_session_id_table: ib.xhttp_session_id_table || '',
    xhttp_session_id_length: ib.xhttp_session_id_length || '',
    xhttp_seq_placement: ib.xhttp_seq_placement || '',
    xhttp_seq_key: ib.xhttp_seq_key || '',
    xhttp_uplink_data_placement: ib.xhttp_uplink_data_placement || '',
    xhttp_uplink_data_key: ib.xhttp_uplink_data_key || '',
    xhttp_uplink_chunk_size: ib.xhttp_uplink_chunk_size || '',
    xhttp_sc_max_each_post_bytes: ib.xhttp_sc_max_each_post_bytes || '',
    xhttp_sc_max_buffered_posts: ib.xhttp_sc_max_buffered_posts || '',
    xhttp_sc_min_posts_interval_ms: ib.xhttp_sc_min_posts_interval_ms || '',
    xhttp_sc_stream_up_server_secs: ib.xhttp_sc_stream_up_server_secs || '',
    xhttp_server_max_header_bytes: ib.xhttp_server_max_header_bytes || '',
    xhttp_uplink_http_method: ib.xhttp_uplink_http_method || '',
    xhttp_no_sse_header: !!ib.xhttp_no_sse_header,
    xhttp_no_grpc_header: !!ib.xhttp_no_grpc_header,
    xhttp_headers: ib.xhttp_headers || '',
    xhttp_xmux_enabled: !!(
      ib.xhttp_xmux_max_concurrency ||
      ib.xhttp_xmux_max_connections ||
      ib.xhttp_xmux_c_max_reuse_times ||
      ib.xhttp_xmux_h_max_request_times ||
      ib.xhttp_xmux_h_max_reusable_secs ||
      ib.xhttp_xmux_h_keep_alive_period
    ),
    xhttp_xmux_max_concurrency: ib.xhttp_xmux_max_concurrency || '',
    xhttp_xmux_max_connections: ib.xhttp_xmux_max_connections || '',
    xhttp_xmux_c_max_reuse_times: ib.xhttp_xmux_c_max_reuse_times || '',
    xhttp_xmux_h_max_request_times: ib.xhttp_xmux_h_max_request_times || '',
    xhttp_xmux_h_max_reusable_secs: ib.xhttp_xmux_h_max_reusable_secs || '',
    xhttp_xmux_h_keep_alive_period: ib.xhttp_xmux_h_keep_alive_period || '',
    tls_server_name: ib.tls_server_name || '',
    tls_certificate: '',
    tls_key: '',
    ss_method: ib.ss_method || 'aes-256-gcm',
    // 回显落库的服务端 PSK：后端保存时可能自动生成（2022 档），编辑时必须带回来，
    // 否则「不改留空」会让人以为密码还在、实际提交空串。
    ss_password: ib.ss_password || '',
    // socks / http：用户名回显，密码永远不回显（留空=不改）
    auth_user: ib.auth_user || '',
    auth_pass: '',
    // tunnel（dokodemo-door）
    tunnel_address: ib.tunnel_address || '',
    tunnel_port: ib.tunnel_port || 0,
    tunnel_network: ib.tunnel_network || 'tcp,udp',
    // wireguard：只回显 MTU 与对端公钥清单，服务端私钥不出接口
    wg_mtu: ib.wg_mtu || 1420,
    wg_peers: ib.wg_peers || '',
    hy_hop_ports: ib.hy_hop_ports || '',
    hy_hop_interval: ib.hy_hop_interval || '',
    hy_udp_idle_timeout: ib.hy_udp_idle_timeout || 60,
    hy_congestion: ib.hy_congestion || '',
    hy_masquerade_type: ib.hy_masquerade?.type || '',
    hy_masquerade_url: ib.hy_masquerade?.url || '',
    hy_masquerade_dir: ib.hy_masquerade?.dir || '',
    hy_masquerade_content: ib.hy_masquerade?.content || '',
    hy_masquerade_status_code: ib.hy_masquerade?.statusCode ?? 0,
    hy_reject_unknown_sni: !!ib.hy_reject_unknown_sni,
    hy_session_resumption: !!ib.hy_session_resumption,
    hy_brutal_up: ib.hy_brutal_up ?? 0,
    hy_brutal_down: ib.hy_brutal_down ?? 0,
    hy_init_stream_window: ib.hy_init_stream_window ?? 0,
    hy_max_stream_window: ib.hy_max_stream_window ?? 0,
    hy_init_conn_window: ib.hy_init_conn_window ?? 0,
    hy_max_conn_window: ib.hy_max_conn_window ?? 0,
    // 密码永远不回显：留空不动，勾「清除」才清（按钮/勾选框在模板里按 hy_salamander_password_set 显示）
    hy_salamander_password: '',
    hy_salamander_clear: false,
    tls_cert_id: ib.tls_cert_id ?? null,
    sniffing_enabled: ib.sniffing ? ib.sniffing.enabled !== false : true,
    sniffing_dest: Array.isArray(ib.sniffing?.destOverride) ? [...ib.sniffing.destOverride] : ['http', 'tls', 'quic'],
    sniffing_route_only: !!(ib.sniffing && ib.sniffing.routeOnly),
    enabled: ib.enabled,
  }
  advOpen.value = false
  loadCertificates().then(() => {
    pemCertId.value = bestPemCert()?.id ?? null
  })
  modalOpen.value = true
}

const generateReality = async () => {
  generating.value = true
  try {
    const res = await adminApi.generateRealityKeypair(nodeId.value)
    form.value.private_key = res.private_key
    form.value.public_key = res.public_key
    form.value.short_ids = res.short_id
    form.value.dest = res.dest
    form.value.server_names = res.server_name
    toast.success('已生成新的 Reality 密钥对')
  } catch (e) {
    toast.error(e.message || '生成失败')
  } finally {
    generating.value = false
  }
}

const splitList = (s) =>
  String(s || '')
    .split(/[,，\s]+/)
    .map((x) => x.trim())
    .filter(Boolean)

// ── Reality 目标检测（节点侧真实 TLS 握手）────────────────
// 判定口径与 3x-ui 一致：TLS 1.3 + h2 + X25519 + 证书链受信任，四项全过才算可用。
const scanOpen = ref(false)
const scanInput = ref('')
const scanning = ref(false)
const scanResults = ref([])
const scanSelected = ref([])
const scanError = ref('')

// 最快的一个可用目标（结果已按「可用 → 延迟升序」排好）
const fastestTarget = computed(() => scanResults.value.find((r) => r.feasible)?.target || '')

const openScan = () => {
  scanOpen.value = true
  scanResults.value = []
  scanSelected.value = []
  scanError.value = ''
}

const runScan = async () => {
  if (!nodeId.value) return
  scanning.value = true
  scanError.value = ''
  try {
    const res = await adminApi.scanRealityTargets(nodeId.value, splitList(scanInput.value))
    if (res?.pending) {
      scanResults.value = []
      scanError.value = res.message || '节点未及时回执，请稍后重试'
    } else if (res?.error) {
      scanResults.value = []
      scanError.value = res.error
    } else {
      scanResults.value = res?.results || []
      // 默认勾上最快的一个（serverNames 取所选目标证书的 SAN 全集，单选语义）
      scanSelected.value = fastestTarget.value ? [fastestTarget.value] : []
    }
  } catch (e) {
    scanResults.value = []
    scanError.value = e.message || '检测失败'
  } finally {
    scanning.value = false
  }
}

// serverNames 必须来自同一张证书（REALITY 服务端按 dest 回源证书做 SNI 精确匹配），
// 故选择语义为单选：点击新目标即替换，再次点击取消
const toggleScan = (r) => {
  if (!r.feasible) return
  scanSelected.value = scanSelected.value.includes(r.target) ? [] : [r.target]
}

const formatChain = (bytes) => (bytes ? `${(bytes / 1024).toFixed(1)} KB` : '—')

// ── 端口跳跃（issue #94）──────────────────────────────────
// 三件事都在节点侧执行（agent 配防火墙），面板只转发白名单载荷：
//   probe  → 在池子里挑一段连续空闲端口（只读）
//   apply  → 写 HY2HOP-<inbound_id> 链并自验 + 持久化
//   status → 读回节点上真实的链状态（不是数据库里的意图）
const hopOpen = ref(false)
const hopInbound = ref(null) // 当前操作的入站
const hopBusy = ref('') // 'probe' | 'apply' | 'remove' | 'status'
const hopError = ref('')
const hopProbe = ref(null) // port_probe 回执
const hopProbePool = ref('20000-50000')
const hopProbeCount = ref(200)
const hopPorts = ref('') // 当前待应用的区间
const hopStatusResult = ref(null) // firewall_hop_status 的 result
const hopApplyResult = ref(null) // firewall_hop_apply / remove 的 result

const hopNodeMissing = computed(() => !hopSupported.value)

const openHop = async (ib) => {
  hopInbound.value = ib
  hopOpen.value = true
  hopError.value = ''
  hopProbe.value = null
  hopApplyResult.value = null
  hopPorts.value = ib.hy_hop_ports || ''
  hopStatusResult.value = null
  // 版本不足时不发指令（status 也会被后端 412 拒掉），直接显示升级提示。
  await refreshHopStatus()
}

// 「端口跳跃」区块的状态摘要：链在不在、是否 verified/persisted。
const hopChain = computed(() => {
  const chains = hopStatusResult.value?.chains
  if (!Array.isArray(chains) || !hopInbound.value) return null
  return chains.find((c) => c.name === `HY2HOP-${hopInbound.value.id}`) || null
})

const hopBadge = (ib) => {
  // 列表行上的小标记：只表示「库里配了区间」，真实状态要点进区块读 status。
  return ib.hy_hop_ports ? `跳跃 ${ib.hy_hop_ports}` : ''
}

const refreshHopStatus = async () => {
  if (!nodeId.value || !hopInbound.value || !hopSupported.value) return
  hopBusy.value = 'status'
  hopError.value = ''
  try {
    const res = await adminApi.hopStatus(nodeId.value)
    if (res?.pending) hopError.value = res.message || '节点未及时回执'
    else if (res?.error) hopError.value = res.error
    else hopStatusResult.value = res?.result || null
  } catch (e) {
    hopError.value = e.message || '读取节点状态失败'
  } finally {
    hopBusy.value = ''
  }
}

const runHopProbe = async () => {
  if (!nodeId.value || !hopInbound.value || !hopSupported.value) return
  hopBusy.value = 'probe'
  hopError.value = ''
  try {
    const res = await adminApi.hopProbe(nodeId.value, {
      inbound_id: hopInbound.value.id,
      pool: hopProbePool.value,
      count: Number(hopProbeCount.value) || 200,
    })
    if (res?.pending) hopError.value = res.message || '节点未及时回执'
    else if (res?.error) hopError.value = res.error
    else {
      hopProbe.value = res?.result || null
      // 推荐区间直接填进「待应用」框，管理员可改
      if (hopProbe.value?.picked) hopPorts.value = hopProbe.value.picked
    }
  } catch (e) {
    hopError.value = e.message || '探测失败'
  } finally {
    hopBusy.value = ''
  }
}

const runHopApply = async () => {
  if (!nodeId.value || !hopInbound.value || !hopSupported.value) return
  hopBusy.value = 'apply'
  hopError.value = ''
  try {
    const res = await adminApi.hopApply(nodeId.value, {
      inbound_id: hopInbound.value.id,
      ports: hopPorts.value || '',
    })
    if (res?.pending) hopError.value = res.message || '节点未及时回执'
    else if (res?.error) {
      hopError.value = res.error
      hopApplyResult.value = res.result || null
    } else {
      hopApplyResult.value = res?.result || null
      if (!hopApplyResult.value?.verified) {
        hopError.value = '节点回执未通过自验（verified=false），规则可能没生效'
      }
      await refreshHopStatus()
      await load()
    }
  } catch (e) {
    hopError.value = e.message || '应用失败'
  } finally {
    hopBusy.value = ''
  }
}

const runHopRemove = async () => {
  if (!nodeId.value || !hopInbound.value || !hopSupported.value) return
  if (!confirm(`删除 ${hopInbound.value.tag} 的跳跃防火墙规则（HY2HOP-${hopInbound.value.id}）？\n删除后跳跃区间内的端口不再转发到主端口。`)) return
  hopBusy.value = 'remove'
  hopError.value = ''
  try {
    const res = await adminApi.hopRemove(nodeId.value, { inbound_id: hopInbound.value.id })
    if (res?.pending) hopError.value = res.message || '节点未及时回执'
    else if (res?.error) hopError.value = res.error
    else {
      hopApplyResult.value = res?.result || null
      await refreshHopStatus()
    }
  } catch (e) {
    hopError.value = e.message || '删除失败'
  } finally {
    hopBusy.value = ''
  }
}

const applyScan = () => {
  const picked = scanResults.value.filter((r) => scanSelected.value.includes(r.target))
  if (picked.length === 0) {
    toast.error('请先勾选一个可用目标')
    return
  }
  // serverNames 取该目标证书的 SAN 全集（节点回执 server_names，已过滤通配符）；
  // 跨 target 混填会把「不属于 dest 证书」的名字塞进白名单，导致 REALITY 握手失败
  const first = picked[0]
  const names = Array.isArray(first.server_names) ? first.server_names.filter(Boolean) : []
  if (names.length === 0) {
    toast.error('该目标证书没有可用的 serverNames')
    return
  }
  form.value.server_names = names.join(', ')
  form.value.dest = first.target
  scanOpen.value = false
  toast.success(`已填入 ${names.length} 个 serverNames（dest = ${first.target}）`)
}

const save = async (forceServerNames = false) => {
  if (!nodeId.value) return
  // 后量子材料成对校验（前后端同守）：只在一边填会让 REALITY 握手失败
  if (isReality.value) {
    const seed = (form.value.mldsa65_seed || '').trim()
    const verify = (form.value.mldsa65_verify || '').trim()
    if ((seed === '') !== (verify === '')) {
      toast.error('mldsa65Seed 与 mldsa65Verify 必须成对填写（或都留空）')
      advOpen.value = true
      return
    }
  }
  saving.value = true
  try {
    const payload = {
      protocol: form.value.protocol,
      tag: form.value.tag || null,
      remark: form.value.remark || null,
      port: Number(form.value.port),
      listen: form.value.listen,
      network: form.value.network,
      security: form.value.security,
      enabled: form.value.enabled,
      // 嗅探（issue #39）：全量提交，默认值对齐现状
      sniffing: {
        enabled: !!form.value.sniffing_enabled,
        destOverride: [...form.value.sniffing_dest],
        routeOnly: !!form.value.sniffing_route_only,
      },
    }
    if (isReality.value) {
      payload.dest = form.value.dest
      payload.server_names = splitList(form.value.server_names)
      // issue #57：后端对「与 dest 不同站点」的 serverNames 返回 422，管理员确认后带这个标记重发
      if (forceServerNames) payload.confirm_server_names = true
      payload.short_ids = splitList(form.value.short_ids)
      if (form.value.private_key) payload.private_key = form.value.private_key
      if (form.value.public_key) payload.public_key = form.value.public_key
      // 高级参数（全量提交，保证编辑能清空）
      payload.show = form.value.show
      payload.xver = Number(form.value.xver) || 0
      payload.min_client_ver = form.value.min_client_ver
      payload.max_client_ver = form.value.max_client_ver
      payload.max_timediff = Number(form.value.max_timediff) || 0
      payload.mldsa65_seed = form.value.mldsa65_seed
      payload.mldsa65_verify = form.value.mldsa65_verify
      payload.master_key_log = form.value.master_key_log
      payload.fingerprint = form.value.fingerprint
      payload.spider_x = form.value.spider_x
      payload.limit_fallback_upload = {
        after_bytes: Number(form.value.limit_fallback_upload.after_bytes) || 0,
        bytes_per_sec: Number(form.value.limit_fallback_upload.bytes_per_sec) || 0,
        burst_bytes_per_sec: Number(form.value.limit_fallback_upload.burst_bytes_per_sec) || 0,
      }
      payload.limit_fallback_download = {
        after_bytes: Number(form.value.limit_fallback_download.after_bytes) || 0,
        bytes_per_sec: Number(form.value.limit_fallback_download.bytes_per_sec) || 0,
        burst_bytes_per_sec: Number(form.value.limit_fallback_download.burst_bytes_per_sec) || 0,
      }
    }
    if (isWs.value) {
      payload.ws_path = form.value.ws_path || '/'
      payload.ws_host = form.value.ws_host
    }
    if (isXhttp.value) {
      payload.xhttp_path = form.value.xhttp_path || '/'
      payload.xhttp_host = form.value.xhttp_host
      payload.xhttp_mode = form.value.xhttp_mode || 'auto'
      // xhttp 高级字段全量提交（编辑能清空；空值后端不落库）
      payload.xhttp_padding_bytes = form.value.xhttp_padding_bytes
      payload.xhttp_padding_obfs_mode = form.value.xhttp_padding_obfs_mode
      payload.xhttp_padding_key = form.value.xhttp_padding_key
      payload.xhttp_padding_header = form.value.xhttp_padding_header
      payload.xhttp_padding_placement = form.value.xhttp_padding_placement
      payload.xhttp_padding_method = form.value.xhttp_padding_method
      payload.xhttp_session_id_placement = form.value.xhttp_session_id_placement
      payload.xhttp_session_id_key = form.value.xhttp_session_id_key
      payload.xhttp_session_id_table = form.value.xhttp_session_id_table
      payload.xhttp_session_id_length = form.value.xhttp_session_id_length
      payload.xhttp_seq_placement = form.value.xhttp_seq_placement
      payload.xhttp_seq_key = form.value.xhttp_seq_key
      payload.xhttp_uplink_data_placement = form.value.xhttp_uplink_data_placement
      payload.xhttp_uplink_data_key = form.value.xhttp_uplink_data_key
      payload.xhttp_uplink_chunk_size = form.value.xhttp_uplink_chunk_size
      payload.xhttp_sc_max_each_post_bytes = form.value.xhttp_sc_max_each_post_bytes
      payload.xhttp_sc_max_buffered_posts = form.value.xhttp_sc_max_buffered_posts
      payload.xhttp_sc_min_posts_interval_ms = form.value.xhttp_sc_min_posts_interval_ms
      payload.xhttp_sc_stream_up_server_secs = form.value.xhttp_sc_stream_up_server_secs
      payload.xhttp_server_max_header_bytes = form.value.xhttp_server_max_header_bytes
      payload.xhttp_uplink_http_method = form.value.xhttp_uplink_http_method
      payload.xhttp_no_sse_header = form.value.xhttp_no_sse_header
      payload.xhttp_no_grpc_header = form.value.xhttp_no_grpc_header
      payload.xhttp_headers = form.value.xhttp_headers
      payload.xhttp_xmux_max_concurrency = form.value.xhttp_xmux_max_concurrency
      payload.xhttp_xmux_max_connections = form.value.xhttp_xmux_max_connections
      payload.xhttp_xmux_c_max_reuse_times = form.value.xhttp_xmux_c_max_reuse_times
      payload.xhttp_xmux_h_max_request_times = form.value.xhttp_xmux_h_max_request_times
      payload.xhttp_xmux_h_max_reusable_secs = form.value.xhttp_xmux_h_max_reusable_secs
      payload.xhttp_xmux_h_keep_alive_period = form.value.xhttp_xmux_h_keep_alive_period
    }
    if (isHttpupgrade.value) {
      payload.httpupgrade_path = form.value.httpupgrade_path || '/'
      payload.httpupgrade_host = form.value.httpupgrade_host
    }
    if (form.value.network === 'tcp') {
      payload.tcp_header_type = form.value.tcp_header_type || 'none'
      if (form.value.tcp_header_type === 'http') {
        payload.tcp_header_path = form.value.tcp_header_path || '/'
        payload.tcp_header_host = form.value.tcp_header_host
      }
    }
    if (isTls.value) {
      payload.tls_server_name = form.value.tls_server_name
      if (form.value.tls_certificate && form.value.tls_key) {
        payload.tls_certificate = form.value.tls_certificate
        payload.tls_key = form.value.tls_key
      }
    }
    if (isSs.value) {
      payload.ss_method = form.value.ss_method
      if (form.value.ss_password) payload.ss_password = form.value.ss_password
    }
    if (isProxyProto.value) {
      payload.auth_user = form.value.auth_user
      // 密码不回显：留空 = 不改（后端沿用原值），填了 = 覆盖
      if (form.value.auth_pass) payload.auth_pass = form.value.auth_pass
    }
    if (isTunnel.value) {
      payload.tunnel_address = String(form.value.tunnel_address || '').trim()
      payload.tunnel_port = Number(form.value.tunnel_port) || 0
      payload.tunnel_network = form.value.tunnel_network || 'tcp,udp'
    }
    if (isWg.value) {
      payload.wg_mtu = Number(form.value.wg_mtu) || 0
      payload.wg_peers = form.value.wg_peers || ''
    }
    if (isHy.value) {
      // 主端口就是 payload.port；这里补跳跃/伪装/拥塞控制/证书引用。
      // 全量提交：编辑时清空的项也要真的落空，不能被旧值残留。
      payload.hy_hop_ports = String(form.value.hy_hop_ports || '').trim()
      payload.hy_hop_interval = String(form.value.hy_hop_interval || '').trim()
      payload.hy_udp_idle_timeout = Number(form.value.hy_udp_idle_timeout) || 60
      payload.hy_congestion = form.value.hy_congestion || ''
      payload.hy_masquerade_type = form.value.hy_masquerade_type || ''
      payload.hy_masquerade_url = form.value.hy_masquerade_url || ''
      payload.hy_masquerade_dir = form.value.hy_masquerade_dir || ''
      payload.hy_masquerade_content = form.value.hy_masquerade_content || ''
      payload.hy_masquerade_status_code = Number(form.value.hy_masquerade_status_code) || 0
      payload.hy_reject_unknown_sni = !!form.value.hy_reject_unknown_sni
      payload.hy_session_resumption = !!form.value.hy_session_resumption
      payload.hy_brutal_up = Number(form.value.hy_brutal_up) || 0
      payload.hy_brutal_down = Number(form.value.hy_brutal_down) || 0
      payload.hy_init_stream_window = Number(form.value.hy_init_stream_window) || 0
      payload.hy_max_stream_window = Number(form.value.hy_max_stream_window) || 0
      payload.hy_init_conn_window = Number(form.value.hy_init_conn_window) || 0
      payload.hy_max_conn_window = Number(form.value.hy_max_conn_window) || 0
      // salamander 混淆密码：填了=覆盖（优先，避免误勾「清除」把新密码吃掉），
      // 勾「清除」=传空串，留空=不改（不带字段，后端沿用原值）。
      // 后端语义与之一一对应（xray_inbound.go: applyHysteriaSalamander）。
      if (form.value.hy_salamander_password) payload.hy_salamander_password = form.value.hy_salamander_password
      else if (form.value.hy_salamander_clear) payload.hy_salamander_password = ''
      if (form.value.tls_cert_id) payload.tls_cert_id = Number(form.value.tls_cert_id)
    }

    if (editing.value) {
      await adminApi.updateXrayInbound(nodeId.value, editing.value.id, payload)
      toast.success('入站已更新（节点将秒级生效）')
    } else {
      await adminApi.createXrayInbound(nodeId.value, payload)
      toast.success('入站已创建（节点将秒级生效）')
    }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    saved.value = true
    setTimeout(() => {
      saved.value = false
      modalOpen.value = false
    }, 900)
    await load()
  } catch (e) {
    // issue #57：后端拦下「serverNames 与 dest 不是同一站点」的配置。
    // 少数场景是合理的（dest 是 CDN，一张证书覆盖多个域名），所以给确认通道，不写死拒绝。
    if (e.code === 422 && /serverNames/i.test(String(e.message || ''))) {
      if (confirm(`${e.message}\n\n确认这些域名确实由 dest 的同一张证书覆盖，继续保存？`)) {
        return save(true)
      }
      // 取消：把原因留在屏幕上，别静默什么都不发生
      toast.error(e.message)
      return
    }
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const remove = async (ib) => {
  if (!confirm(`删除入站 ${ib.tag}（端口 ${ib.port}）？`)) return
  try {
    await adminApi.deleteXrayInbound(nodeId.value, ib.id)
    selectedIds.value = selectedIds.value.filter((id) => id !== ib.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

const protocolLabel = { vless: 'VLESS', vmess: 'VMess', trojan: 'Trojan', shadowsocks: 'SS', hysteria: 'HY2', socks: 'SOCKS', http: 'HTTP', tunnel: 'Tunnel', wireguard: 'WG' }
const protocolVariant = (p) => ({ vless: 'vless', trojan: 'trojan' }[p] || 'info')
const securityVariant = (s) => ({ reality: 'info', tls: 'success' }[s] || 'info')
// 用户数一列：这几种协议没有用户概念（凭据在入站级或根本没有），不按用户下发客户端，
// 列表里显示「—」。ss 以前也走这个特判（单凭据），SS-2022 多用户后它同样是逐用户
// client，跟其它协议一样显示真实 client_count。
const CLIENTLESS_PROTOCOLS = ['socks', 'http', 'tunnel', 'wireguard']
const clientLabel = (ib) => {
  if (CLIENTLESS_PROTOCOLS.includes(ib.protocol)) return '—'
  return ib.client_count
}
</script>

<template>
  <div class="an">
    <div class="an-head">
      <h1 class="an-title">xray 入站</h1>
      <Button variant="primary" size="sm" :disabled="!nodeId" @click="openCreate">
        <Icon name="plus" :size="16" /> 新建入站
      </Button>
    </div>
    <p class="an-desc">
      入站 = 客户端连接的协议入口（协议 / 端口 / 传输 / 安全层）。Reality 密钥由面板生成并随配置下发，节点零操作。
    </p>

    <div class="an-toolbar">
      <XrayNodePicker v-model="nodeId" />
      <!-- 勾选后出现的批量操作条（对齐 3x-ui：选中数可一键清空，动作靠右） -->
      <div v-if="selectedCount" class="an-bulk">
        <span class="an-bulk-count">
          {{ selectedCount }} 个已选
          <button class="an-bulk-clear" type="button" title="清空选择" @click="selectedIds = []">×</button>
        </span>
        <Button size="sm" variant="danger" :loading="batchRemoving" @click="removeSelected">
          <Icon name="trash" :size="14" /> 删除选中
        </Button>
      </div>
    </div>

    <Card padding="false">
      <div class="an-table-wrap">
        <table class="an-table">
          <thead>
            <tr>
              <th class="an-check-cell">
                <input
                  type="checkbox"
                  :checked="allSelected"
                  :disabled="!inbounds.length"
                  title="全选 / 取消全选"
                  @change="toggleSelectAll"
                />
              </th>
              <th>标识</th>
              <th>备注</th>
              <th>协议</th>
              <th>端口</th>
              <th>传输</th>
              <th>安全</th>
              <th>用户数</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="ib in inbounds" :key="ib.id">
              <td class="an-check-cell">
                <input type="checkbox" :checked="isSelected(ib)" @change="toggleSelect(ib)" />
              </td>
              <td class="mono">{{ ib.tag }}</td>
              <td>{{ ib.remark || '—' }}</td>
              <td><Badge :variant="protocolVariant(ib.protocol)">{{ protocolLabel[ib.protocol] || ib.protocol }}</Badge></td>
              <td class="mono">{{ ib.port }}</td>
              <td class="mono">
                {{ ib.network }}
                <span v-if="hopBadge(ib)" class="an-hop"> · {{ hopBadge(ib) }}</span>
              </td>
              <td>
                <Badge :variant="securityVariant(ib.security)">{{ ib.security === 'reality' ? 'Reality' : ib.security === 'tls' ? 'TLS' : '无' }}</Badge>
              </td>
              <td class="mono">{{ clientLabel(ib) }}</td>
              <td>
                <Badge :variant="ib.enabled ? 'success' : 'warning'" dot>{{ ib.enabled ? '启用' : '停用' }}</Badge>
              </td>
              <td>
                <div class="an-actions">
                  <Button
                    v-if="ib.protocol === 'hysteria'"
                    size="sm"
                    variant="ghost"
                    :title="hopSupported ? '端口跳跃（节点侧防火墙）' : `需升级 agent（≥ ${hopMinAgentVersion}）`"
                    :disabled="!hopSupported"
                    @click="openHop(ib)"
                  >
                    <Icon name="activity" :size="16" />
                  </Button>
                  <Button size="sm" variant="ghost" @click="openEdit(ib)"><Icon name="edit" :size="16" /></Button>
                  <Button size="sm" variant="ghost" @click="remove(ib)"><Icon name="trash" :size="16" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!loading && inbounds.length === 0">
              <td colspan="10" class="an-empty">{{ nodeId ? '该节点还没有入站 —— 点右上角「新建入站」开始' : '请先在上方选择一个 xray 节点' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Modal v-model="modalOpen" :title="editing ? `编辑入站 ${editing.tag}` : '新建入站'" width="560px">
      <div class="an-form">
        <div class="an-grid">
          <label class="an-field">
            <span>协议</span>
            <Select v-model="form.protocol" :options="protocolOptions" />
          </label>
          <label class="an-field">
            <span>端口</span>
            <Input v-model.number="form.port" type="number" placeholder="443" />
          </label>
        </div>
        <div v-if="!hideTransport || !hideSecurity" class="an-grid">
          <label v-if="!hideTransport" class="an-field">
            <span>传输</span>
            <Select v-model="form.network" :options="networkOptions" />
          </label>
          <label v-if="!hideSecurity" class="an-field">
            <span>安全层</span>
            <Select v-model="form.security" :options="securityOptions" :disabled="isSs" />
          </label>
        </div>
        <label class="an-field">
          <span>地址族（入站绑定的协议栈）</span>
          <Select v-model="listenFamily" :options="listenOptions" />
        </label>
        <p class="an-hint">
          选 IPv6（<code>::</code>）后，订阅链接里的地址改用节点表单填的「服务器 IPv6 地址」；
          没填时仍回落到主地址。选 IPv4 则行为与以前一致。
        </p>
        <label class="an-field">
          <span>标识 tag（留空自动生成，同节点内唯一；仅字母/数字/下划线/连字符）</span>
          <Input v-model="form.tag" placeholder="如 vless-reality" />
        </label>
        <label class="an-field">
          <span>备注（可选，可中文；订阅节点名优先展示它）</span>
          <Input v-model="form.remark" placeholder="如：香港中转" />
        </label>

        <template v-if="isReality">
          <div class="an-section-title">
            <span>Reality 参数</span>
            <span class="an-section-actions">
              <Button size="sm" variant="secondary" :disabled="!nodeId" @click="openScan">
                <Icon name="activity" :size="14" /> 检测目标
              </Button>
              <Button size="sm" variant="secondary" :loading="generating" @click="generateReality">
                <Icon name="refresh" :size="14" /> 生成密钥对
              </Button>
            </span>
          </div>
          <div class="an-grid">
            <label class="an-field">
              <span>dest（伪装目标，域名:端口）</span>
              <Input v-model="form.dest" placeholder="www.cloudflare.com:443" />
            </label>
            <label class="an-field">
              <span>serverNames（逗号分隔）</span>
              <Input v-model="form.server_names" placeholder="www.cloudflare.com" />
            </label>
          </div>
          <p class="an-hint">
            每个 serverName 必须是 <code>dest</code> 那台站点证书覆盖的域名 —— REALITY 会按客户端 SNI 向
            dest 取证书，dest 给不出证书的域名会让分到它的用户全部连不上（且按用户固定分派，重拉订阅也修不好）。
            建议点「检测目标」自动填：会取该目标证书的 SAN 全集，不会混进别家域名。
          </p>
          <div class="an-grid">
            <label class="an-field">
              <span>shortIds（逗号分隔，十六进制 0-9a-f，留空自动生成）</span>
              <Input v-model="form.short_ids" placeholder="如 ab12cd34（1-16 位十六进制）" />
            </label>
            <label class="an-field">
              <span>客户端公钥（自动回填，可留空）</span>
              <Input v-model="form.public_key" placeholder="点「生成密钥对」自动填" />
            </label>
          </div>
          <p class="an-hint">私钥只在服务端配置里，由面板保存并下发，不会出现在订阅链接里。</p>

          <div class="an-adv">
            <button type="button" class="an-adv-toggle" @click="advOpen = !advOpen">
              <Icon :name="advOpen ? 'chevron-down' : 'chevron-right'" :size="14" />
              高级参数（xver / 客户端版本 / 后量子 / fallback 限速）
            </button>
            <div v-if="advOpen" class="an-adv-body">
              <div class="an-grid">
                <label class="an-field">
                  <span>xver（REALITY 前置反代的 PROXY protocol 版本）</span>
                  <Input v-model.number="form.xver" type="number" placeholder="0" />
                </label>
                <label class="an-field">
                  <span>maxTimediff（客户端时钟容差 ms）</span>
                  <Input v-model.number="form.max_timediff" type="number" placeholder="0" />
                </label>
              </div>
              <div class="an-grid">
                <label class="an-field">
                  <span>minClientVer（如 1.8.0，可留空）</span>
                  <Input v-model="form.min_client_ver" placeholder="x.y.z" />
                </label>
                <label class="an-field">
                  <span>maxClientVer（如 1.8.24，可留空）</span>
                  <Input v-model="form.max_client_ver" placeholder="x.y.z" />
                </label>
              </div>
              <div class="an-grid">
                <label class="an-field">
                  <span>uTLS 指纹 fp（订阅链接用）</span>
                  <Select v-model="form.fingerprint" :options="fingerprintOptions" />
                </label>
                <label class="an-field">
                  <span>spiderX（爬虫路径，如 /）</span>
                  <Input v-model="form.spider_x" placeholder="/" />
                </label>
              </div>
              <label class="an-field-row">
                <input v-model="form.show" type="checkbox" />
                <span>show（调试：REALITY 日志打印握手细节）</span>
              </label>
              <div class="an-grid">
                <label class="an-field">
                  <span>mldsa65Seed（服务端，后量子签名种子）</span>
                  <textarea v-model="form.mldsa65_seed" class="an-textarea" rows="3" placeholder="留空=不启用后量子" />
                </label>
                <label class="an-field">
                  <span>mldsa65Verify（客户端配对，须与 Seed 同填）</span>
                  <textarea v-model="form.mldsa65_verify" class="an-textarea" rows="3" placeholder="须与 mldsa65Seed 成对" />
                </label>
              </div>
              <label class="an-field">
                <span>masterKeyLog（TLS 主密钥日志路径，调试用）</span>
                <Input v-model="form.master_key_log" placeholder="/path/to/sslkeylog.txt" />
              </label>
              <div class="an-grid">
                <div class="an-field">
                  <span>fallback 上传限速</span>
                  <div class="an-grid an-grid-3">
                    <Input v-model.number="form.limit_fallback_upload.after_bytes" type="number" placeholder="afterBytes" />
                    <Input v-model.number="form.limit_fallback_upload.bytes_per_sec" type="number" placeholder="bytesPerSec" />
                    <Input v-model.number="form.limit_fallback_upload.burst_bytes_per_sec" type="number" placeholder="burst" />
                  </div>
                </div>
                <div class="an-field">
                  <span>fallback 下载限速</span>
                  <div class="an-grid an-grid-3">
                    <Input v-model.number="form.limit_fallback_download.after_bytes" type="number" placeholder="afterBytes" />
                    <Input v-model.number="form.limit_fallback_download.bytes_per_sec" type="number" placeholder="bytesPerSec" />
                    <Input v-model.number="form.limit_fallback_download.burst_bytes_per_sec" type="number" placeholder="burst" />
                  </div>
                </div>
              </div>
              <p class="an-hint">以上均为可选，留空/0 即内核默认；限速三值全 0 表示不限速。</p>
            </div>
          </div>
        </template>

        <template v-if="form.network === 'tcp'">
          <div class="an-grid">
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
        </template>

        <template v-if="isWs">
          <div class="an-grid">
            <label class="an-field">
              <span>WS 路径</span>
              <Input v-model="form.ws_path" placeholder="/" />
            </label>
            <label class="an-field">
              <span>WS Host（可选）</span>
              <Input v-model="form.ws_host" placeholder="留空不校验" />
            </label>
          </div>
        </template>

        <template v-if="isHttpupgrade">
          <div class="an-grid">
            <label class="an-field">
              <span>HTTPUpgrade 路径</span>
              <Input v-model="form.httpupgrade_path" placeholder="/" />
            </label>
            <label class="an-field">
              <span>HTTPUpgrade Host（可选）</span>
              <Input v-model="form.httpupgrade_host" placeholder="留空不校验" />
            </label>
          </div>
        </template>

        <template v-if="isXhttp">
          <div class="an-grid">
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

        <template v-if="isTls">
          <label class="an-field">
            <span>TLS serverName</span>
            <Input v-model="form.tls_server_name" placeholder="your.domain.com" />
          </label>
          <!-- 证书 PEM 只对 TLS 入站（vless/trojan）有意义：后端 security==tls && protocol!=hysteria
               时才读 tls_certificate / tls_key。HY2 完全不读这两个框（只认 tlsCertId），
               显示出来会误导人以为必须手贴 → HY2 下一律隐藏。 -->
          <template v-if="!isHy">
            <div class="an-section-title">
              <span>证书与私钥</span>
              <span class="an-section-actions">
                <Button
                  size="sm"
                  variant="secondary"
                  :disabled="!certificates.length || pemFilling"
                  :loading="pemFilling"
                  @click="fillTlsPemFromLibrary"
                >
                  <Icon name="key" :size="14" /> 从证书库填充 PEM
                </Button>
              </span>
            </div>
            <label class="an-field">
              <span>证书库条目（只用于回填，不写进入站配置）</span>
              <Select v-model="pemCertId" :options="pemCertOptions" placeholder="选一张证书" />
            </label>
            <div class="an-grid">
              <label class="an-field">
                <span>证书 PEM{{ editing && editing.has_tls_cert ? '（不改留空）' : '' }}</span>
                <textarea v-model="form.tls_certificate" class="an-textarea" rows="3" placeholder="-----BEGIN CERTIFICATE-----" />
              </label>
              <label class="an-field">
                <span>私钥 PEM{{ editing && editing.has_tls_cert ? '（不改留空）' : '' }}</span>
                <textarea v-model="form.tls_key" class="an-textarea" rows="3" placeholder="-----BEGIN PRIVATE KEY-----" />
              </label>
            </div>
            <p class="an-hint">
              两个框可以手贴，也可以选一张证书库条目后点「从证书库填充 PEM」：回填的是解密后的证书与私钥，
              保存后与手贴完全一样（私钥不会出现在证书库列表接口里，只有点按钮时才单独取一次）。
            </p>
          </template>
          <p v-else class="an-hint">
            HY2 不读内联 PEM：证书走下方「TLS 证书库」引用，入站里只存证书 id、私钥留在库里。
            上面的 serverName 就是客户端握手用的域名，填对之后点证书区的「自动填充证书」即可挑到匹配的那张。
          </p>

          <!-- 远程签发：面板给裸内核节点下发 cert_issue，节点侧跑 acme.sh（HTTP-01），
               成功后后端把 PEM 写进证书库。签完自动选到上面（HY2）或回填两个框（TLS）。 -->
          <div class="an-section-title">
            <span>节点证书签发</span>
            <span class="an-section-actions">
              <Button
                size="sm"
                variant="secondary"
                :disabled="!nodeHost || nodeHostIsIp || issuingCert"
                :loading="issuingCert"
                @click="issueNodeCert"
              >
                <Icon name="shield" :size="14" /> 签发证书
              </Button>
            </span>
          </div>
          <p class="an-hint">
            按节点域名 <code>{{ nodeHost || '（未填）' }}</code> 向 Let's Encrypt 申请一张证书：
            HTTP-01 校验，节点的 80 端口要能从公网打到本机，签发要等几十秒。签好自动写进
            「TLS 证书库」并用于本表单。
            <template v-if="nodeHostIsIp">当前节点地址是 IP，Let's Encrypt 不签 IP，先把节点地址改成域名。</template>
          </p>
        </template>

        <template v-if="isHy">
          <div class="an-section-title"><span>Hysteria2 参数</span></div>
          <div class="an-grid">
            <label class="an-field">
              <span>跳跃区间（留空 = 不跳跃）</span>
              <Input v-model="form.hy_hop_ports" placeholder="20000-20199" />
            </label>
            <label class="an-field">
              <span>跳跃间隔（缺省 5-10）</span>
              <Input v-model="form.hy_hop_interval" placeholder="5-10" />
            </label>
          </div>
          <p class="an-hint">
            主端口（上面的「端口」）不能落在跳跃区间内 —— 否则主端口自己的流量也会被节点侧的
            REDIRECT 绕进跳跃链，连接直接断。区间由节点 agent 分配并配防火墙，面板只负责把
            <code>mport</code> 写进订阅链接。
          </p>
          <div class="an-grid">
            <label class="an-field">
              <span>QUIC 拥塞控制（留空 = 内核默认）</span>
              <Select v-model="form.hy_congestion" :options="hyCongestionOptions" />
            </label>
            <label class="an-field">
              <span>UDP 空闲超时（秒，默认 60）</span>
              <Input v-model.number="form.hy_udp_idle_timeout" type="number" placeholder="60" />
            </label>
          </div>
          <p class="an-hint">
            系统 TCP BBR 对 HY2 无效（HY2 走 UDP/QUIC）；HY2 自己的 BBR 就是这里的
            <code>finalmask.quicParams.congestion</code>。
          </p>
          <div v-if="isHyBrutal" class="an-grid">
            <label class="an-field">
              <span>brutal 上行带宽（Mbps，0 = 不写）</span>
              <Input v-model.number="form.hy_brutal_up" type="number" placeholder="100" />
            </label>
            <label class="an-field">
              <span>brutal 下行带宽（Mbps，0 = 不写）</span>
              <Input v-model.number="form.hy_brutal_down" type="number" placeholder="200" />
            </label>
          </div>
          <p v-if="isHyBrutal" class="an-hint">
            只对 brutal / force-brutal 生效：内核在做带宽整形，填的是线路实际带宽（单位 Mbps）；
            其它拥塞算法下这两项会被忽略。选 force-brutal 时上行必填（内核要求，留空会让整份节点配置起不来）。
          </p>

          <div class="an-section-title"><span>QUIC 接收窗口（留空 / 0 = 内核默认）</span></div>
          <div class="an-grid">
            <label class="an-field">
              <span>初始流窗口（字节）</span>
              <Input v-model.number="form.hy_init_stream_window" type="number" placeholder="0" />
            </label>
            <label class="an-field">
              <span>最大流窗口（字节）</span>
              <Input v-model.number="form.hy_max_stream_window" type="number" placeholder="0" />
            </label>
            <label class="an-field">
              <span>初始连接窗口（字节）</span>
              <Input v-model.number="form.hy_init_conn_window" type="number" placeholder="0" />
            </label>
            <label class="an-field">
              <span>最大连接窗口（字节）</span>
              <Input v-model.number="form.hy_max_conn_window" type="number" placeholder="0" />
            </label>
          </div>
          <p class="an-hint">
            非 0 时内核要求至少 16384 字节（再小直接拒绝整份节点配置）。窗口越大越吃内存，
            高延迟/高带宽线路才需要调，一般留空。
          </p>

          <div class="an-section-title"><span>混淆（salamander，可留空）</span></div>
          <label class="an-field">
            <span>
              obfs 密码{{ editing && editing.hy_salamander_password_set ? '（已设置，留空 = 不改）' : '（留空 = 不启用）' }}
            </span>
            <Input
              v-model="form.hy_salamander_password"
              type="password"
              placeholder="客户端 obfs-password 要填同一个"
            />
          </label>
          <label v-if="editing && editing.hy_salamander_password_set" class="an-field-row">
            <input v-model="form.hy_salamander_clear" type="checkbox" />
            <span>清除已设置的密码（保存后不再启用 salamander 混淆）</span>
          </label>
          <p class="an-hint">
            密码加密存独立列，落库的入站配置里只留 <code>finalmask.udp</code> 的
            <code>salamander</code> 占位，原文不进 <code>stream_settings</code>；下发节点配置时才解密注入。
            两端的混淆密码必须一致，否则客户端能握手但连不通。
          </p>

          <div class="an-section-title"><span>伪装（masquerade）</span></div>
          <div class="an-grid">
            <label class="an-field">
              <span>类型</span>
              <Select v-model="form.hy_masquerade_type" :options="hyMasqueradeOptions" />
            </label>
            <label class="an-field">
              <span>HTTP 状态码（string 类型可用，0 = 默认）</span>
              <Input v-model.number="form.hy_masquerade_status_code" type="number" placeholder="0" />
            </label>
          </div>
          <label v-if="form.hy_masquerade_type === 'string'" class="an-field">
            <span>返回内容</span>
            <Input v-model="form.hy_masquerade_content" placeholder="ok" />
          </label>
          <label v-if="form.hy_masquerade_type === 'file'" class="an-field">
            <span>本地目录（file 类型）</span>
            <Input v-model="form.hy_masquerade_dir" placeholder="/var/www/html" />
          </label>
          <label v-if="form.hy_masquerade_type === 'proxy'" class="an-field">
            <span>反代目标 URL（proxy 类型）</span>
            <Input v-model="form.hy_masquerade_url" placeholder="https://example.com" />
          </label>

          <div class="an-section-title">
            <span>TLS 证书（走证书库引用）</span>
            <span class="an-section-actions">
              <Button size="sm" variant="secondary" :disabled="!certificates.length" @click="autoFillCert">
                <Icon name="refresh" :size="14" /> 自动填充证书
              </Button>
            </span>
          </div>
          <label class="an-field">
            <span>证书库条目（私钥留在库里，不写进入站配置）</span>
            <Select v-model="form.tls_cert_id" :options="certOptions" />
          </label>
          <p v-if="!certificates.length" class="an-hint">
            证书库还是空的：HY2 强制 TLS，没有证书内核起不来。请先到「TLS 证书库」添加一张，
            再回来这里选。
          </p>
          <p v-if="selectedCertProblem" class="an-hint">{{ selectedCertProblem }}</p>
          <p v-if="certificates.length" class="an-hint">
            灰色条目的证书域名覆盖不到
            <code>{{ certTargetHost || '当前节点' }}</code>
            ，客户端按 SNI 握手会失败，所以不给选。「自动填充证书」会按 serverName / 节点域名挑一张
            未过期的填进来；没有条目被置灰说明还判断不出该用哪个域名（serverName 与节点域名都空）。
          </p>

          <div class="an-section-title"><span>TLS 高级开关</span></div>
          <label class="an-field-row">
            <input v-model="form.hy_reject_unknown_sni" type="checkbox" />
            <span>拒绝未知 SNI（客户端填的 SNI 与证书不匹配就不握手）</span>
          </label>
          <label class="an-field-row">
            <input v-model="form.hy_session_resumption" type="checkbox" />
            <span>会话恢复（复用 TLS 会话，减少重复握手开销）</span>
          </label>
          <p class="an-hint">
            两项默认都不开（与内核一致）。开了「拒绝未知 SNI」后，客户端用伪装 SNI（
            <code>insecure=1</code> 随便填个域名）会连不上，只有 SNI 和证书对得上才行。
          </p>
        </template>

        <template v-if="isSs">
          <div class="an-grid">
            <label class="an-field">
              <span>加密方式</span>
              <Select v-model="form.ss_method" :options="ssMethodOptions" />
            </label>
            <label class="an-field">
              <span>密码{{ editing ? '（不改留空）' : '' }}</span>
              <Input
                v-model="form.ss_password"
                :type="ssIs2022 ? 'text' : 'password'"
                :mono="ssIs2022"
                :placeholder="ssIs2022 ? '留空自动生成（编辑时留空 = 不改）' : '连接凭据'"
              />
            </label>
          </div>
          <p v-if="ssIs2022" class="an-hint">
            2022 加密方式的服务端密钥必须是 base64：aes-256 要 32 字节、aes-128 要 16 字节。
            新建时留空或长度不对，后端会<code>自动生成</code>一把合法的并落库，保存后这里显示的就是实际生效的值；
            编辑时留空 = 沿用原值（换 key 会让已经发出去的订阅链接全部失效）。
            它明文出现在每个用户的订阅链接里（<code>ss://方式:服务端密钥:用户密钥</code>）。
          </p>
        </template>

        <!-- socks / http：传统正向代理，一组账号密码（内核默认 noauth，面板强制认证） -->
        <template v-if="isProxyProto">
          <div class="an-section-title"><span>{{ isSocks ? 'SOCKS5 代理账号' : 'HTTP 代理账号' }}</span></div>
          <div class="an-grid">
            <label class="an-field">
              <span>用户名</span>
              <Input v-model="form.auth_user" placeholder="如 proxyuser" />
            </label>
            <label class="an-field">
              <span>密码{{ editing ? '（不改留空）' : '' }}</span>
              <Input v-model="form.auth_pass" type="password" placeholder="连接凭据" />
            </label>
          </div>
          <p class="an-hint">
            内核的 socks / http 入站默认 <code>noauth</code>（裸代理），面板强制要一组账号密码，
            免得 1080 / 8080 这种端口被扫到后当公共代理用。这类入站不按用户下发、也没有用户流量统计
            —— 需要按用户计费请用 VLESS / VMess / Trojan / HY2。
          </p>
        </template>

        <!-- tunnel（dokodemo-door）：固定目标转发 -->
        <template v-if="isTunnel">
          <div class="an-section-title"><span>Tunnel 转发目标</span></div>
          <div class="an-grid">
            <label class="an-field">
              <span>目标地址</span>
              <Input v-model="form.tunnel_address" placeholder="如 127.0.0.1 或 1.1.1.1" />
            </label>
            <label class="an-field">
              <span>目标端口</span>
              <Input v-model.number="form.tunnel_port" type="number" placeholder="如 443" />
            </label>
          </div>
          <label class="an-field">
            <span>转发协议（L4 网络）</span>
            <Select v-model="form.tunnel_network" :options="tunnelNetworkOptions" />
          </label>
          <p class="an-hint">
            dokodemo-door 把收到的流量原样转给目标（入口落地 / 内网跳板用）。
            L4 网络写在内核的 <code>settings.network</code>：选「TCP + UDP」会同时占同端口的 TCP 与 UDP，
            端口冲突判定按这个算。面板不配 tproxy / followRedirect，落地机侧要自己做转发。
          </p>
        </template>

        <!-- wireguard：服务端身份由面板生成，这里只填 MTU 与对端 -->
        <template v-if="isWg">
          <div class="an-section-title"><span>WireGuard 参数</span></div>
          <div class="an-grid">
            <label class="an-field">
              <span>MTU</span>
              <Input v-model.number="form.wg_mtu" type="number" placeholder="1420" />
            </label>
            <label class="an-field">
              <span>服务端公钥（配客户端用，创建后生成）</span>
              <Input :model-value="editing?.wg_public_key || '创建后生成'" disabled />
            </label>
          </div>
          <label class="an-field">
            <span>对端公钥（每行一条：公钥 [允许IP,...]）</span>
            <textarea
              v-model="form.wg_peers"
              class="an-textarea"
              rows="4"
              placeholder="客户端 WireGuard 公钥 10.0.0.2/32"
            />
          </label>
          <p class="an-hint">
            入站固定 UDP 监听、自带加密，所以没有传输层与安全层可选。服务端私钥由面板生成，
            只随下发配置进节点，接口不回显；客户端侧填三个值：服务端公钥（上面的只读框）、
            节点地址 + 这个入站端口、自己的隧道 IP（就是这里的「允许IP」）。
            对端列表可以留空 —— 入站能起，但没人能连上。
          </p>
        </template>

        <!-- 嗅探（issue #39）：默认对齐现状；不改则行为无感 -->
        <label class="an-field-row">
          <input v-model="form.sniffing_enabled" type="checkbox" />
          <span>嗅探（识别 HTTP/TLS/QUIC 流量，用于路由匹配与回源改写）</span>
        </label>
        <template v-if="form.sniffing_enabled">
          <div class="an-field">
            <span>嗅探协议（destOverride）</span>
            <div class="an-sniff-opts">
              <label v-for="d in sniffDestOptions" :key="d.value">
                <input type="checkbox" :value="d.value" v-model="form.sniffing_dest" />
                {{ d.label }}
              </label>
            </div>
          </div>
          <label class="an-field-row">
            <input v-model="form.sniffing_route_only" type="checkbox" />
            <span>仅用于路由（routeOnly）——嗅探结果不改写回源地址</span>
          </label>
        </template>
        <label class="an-field-row">
          <input v-model="form.enabled" type="checkbox" />
          <span>启用（停用后从节点配置中移除）</span>
        </label>
      </div>
      <template #footer>
        <Button variant="ghost" @click="modalOpen = false">取消</Button>
        <Button variant="primary" :loading="saving" :success="saved" @click="save()">{{ editing ? '保存' : '创建' }}</Button>
      </template>
    </Modal>

    <Modal v-model="scanOpen" title="Reality 目标检测" width="900px">
      <div class="an-scan">
        <p class="an-scan-tip">
          在<strong>当前节点</strong>上发起真实 TLS 握手：逐项验证 TLS 1.3 / HTTP2(h2) / X25519 / 证书链是否受信任，
          四项全过才判「可用」。延迟是节点到目标的握手耗时（节点视角才是 REALITY 真正走的路径）。
          勾选一个可用目标 → 填入该目标证书的 serverNames 全集（必须同一张证书，
          跨证书混填会导致 REALITY 握手失败），其地址作为 dest。
        </p>

        <div class="an-scan-bar">
          <Input v-model="scanInput" placeholder="自定义目标（逗号/空格分隔，可带端口，默认 443）；留空用内置常用候选" />
          <Button variant="primary" size="sm" :loading="scanning" @click="runScan">
            <Icon name="search" :size="14" /> 开始检测
          </Button>
        </div>

        <p v-if="scanError" class="an-scan-error">{{ scanError }}</p>

        <div v-if="scanResults.length" class="an-scan-table">
          <table class="an-table">
            <thead>
              <tr>
                <th>选</th>
                <th>目标</th>
                <th>状态</th>
                <th>TLS</th>
                <th>ALPN</th>
                <th>密钥交换</th>
                <th>证书</th>
                <th>链</th>
                <th>延迟</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="r in scanResults"
                :key="r.target"
                :class="{ 'is-picked': scanSelected.includes(r.target) }"
              >
                <td>
                  <input
                    type="checkbox"
                    class="an-scan-check"
                    :checked="scanSelected.includes(r.target)"
                    :disabled="!r.feasible"
                    @change="toggleScan(r)"
                  />
                </td>
                <td class="mono">{{ r.target }}</td>
                <td>
                  <Badge :variant="r.feasible ? 'success' : 'warning'" dot>
                    {{ r.feasible ? '可用' : '不可用' }}
                  </Badge>
                  <span v-if="r.reason" class="an-scan-reason">{{ r.reason }}</span>
                </td>
                <td class="mono">{{ r.tls_version || '—' }}</td>
                <td class="mono">{{ r.alpn || '—' }}</td>
                <td class="mono">{{ r.curve || '—' }}</td>
                <td>
                  <span class="mono">{{ r.cert_subject || '—' }}</span>
                  <span v-if="r.cert_issuer" class="an-scan-dim">{{ r.cert_issuer }}</span>
                </td>
                <td class="mono">
                  {{ formatChain(r.chain_bytes) }}
                  <span v-if="r.chain_warn" class="an-scan-warn" title="证书链偏大，可能影响 REALITY 伪装">偏大</span>
                </td>
                <td class="mono">
                  {{ r.latency_ms }} ms
                  <span v-if="r.target === fastestTarget" class="an-scan-tag">最快</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p v-else-if="!scanning && !scanError" class="an-scan-empty">
          还没有结果 —— 点「开始检测」（留空即测内置常用候选）。
        </p>

        <p v-if="scanning" class="an-scan-empty">正在节点上探测，正常几秒内出结果…</p>
      </div>
      <template #footer>
        <Button variant="ghost" @click="scanOpen = false">关闭</Button>
        <Button variant="secondary" :loading="scanning" @click="runScan">重新检测</Button>
        <Button variant="primary" :disabled="scanSelected.length === 0" @click="applyScan">
          填入 serverNames
        </Button>
      </template>
    </Modal>

    <!-- 端口跳跃（issue #94）：探测 → 应用 → 读回节点上的真实链状态 -->
    <Modal v-model="hopOpen" :title="`端口跳跃 ${hopInbound ? hopInbound.tag : ''}`" width="720px">
      <div class="an-hop-panel">
        <p class="an-hint">
          HY2 服务端只监听主端口（<strong>{{ hopInbound?.port }}</strong>）。跳跃区间内的流量要靠
          <strong>节点侧的防火墙</strong> REDIRECT 回来 —— 这是 agent 干的活，面板只下发白名单指令。
          要跳得动，入站表单里的「跳跃区间」与这里应用的区间要一致（订阅链接的 <code>mport</code> 来自入站配置）。
        </p>

        <p v-if="hopNodeMissing" class="an-hop-error">
          需升级 agent：端口跳跃要求 agent ≥ {{ hopMinAgentVersion }}。请先到「节点」页升级，主端口照常可用。
        </p>

        <div class="an-section-title"><span>① 探测空闲区间</span></div>
        <div class="an-grid">
          <label class="an-field">
            <span>搜索池</span>
            <Input v-model="hopProbePool" placeholder="20000-50000" :disabled="hopNodeMissing" />
          </label>
          <label class="an-field">
            <span>需要连续端口数</span>
            <Input v-model.number="hopProbeCount" type="number" placeholder="200" :disabled="hopNodeMissing" />
          </label>
        </div>
        <div class="an-hop-actions">
          <Button
            size="sm"
            variant="secondary"
            :loading="hopBusy === 'probe'"
            :disabled="hopNodeMissing || !!hopBusy"
            @click="runHopProbe"
          >
            <Icon name="search" :size="14" /> 探测
          </Button>
          <span v-if="hopProbe?.picked" class="an-hop-ok">
            推荐 <code>{{ hopProbe.picked }}</code> · 后端 {{ hopProbe.backend || '—' }}
            <template v-if="hopProbe.free_ranges?.length"> · 共 {{ hopProbe.free_ranges.length }} 段空闲</template>
          </span>
          <span v-else-if="hopProbe && !hopProbe.picked" class="an-hop-dim">池子里凑不出所需连续端口</span>
        </div>

        <div class="an-section-title"><span>② 应用规则</span></div>
        <div class="an-grid">
          <label class="an-field">
            <span>跳跃区间（留空 = 用入站表单里配的）</span>
            <Input v-model="hopPorts" placeholder="20000-20199" :disabled="hopNodeMissing" />
          </label>
          <label class="an-field">
            <span>主端口（固定取自入站，不可改）</span>
            <Input :model-value="String(hopInbound?.port ?? '')" disabled />
          </label>
        </div>
        <div class="an-hop-actions">
          <Button
            size="sm"
            variant="primary"
            :loading="hopBusy === 'apply'"
            :disabled="hopNodeMissing || !!hopBusy"
            @click="runHopApply"
          >
            <Icon name="shield" :size="14" /> 应用
          </Button>
          <Button
            size="sm"
            variant="danger"
            :loading="hopBusy === 'remove'"
            :disabled="hopNodeMissing || !!hopBusy"
            @click="runHopRemove"
          >
            <Icon name="trash" :size="14" /> 删除规则
          </Button>
        </div>

        <p v-if="hopError" class="an-hop-error">{{ hopError }}</p>

        <div class="an-section-title">
          <span>③ 节点真实状态</span>
          <Button size="sm" variant="ghost" :loading="hopBusy === 'status'" :disabled="hopNodeMissing || !!hopBusy" @click="refreshHopStatus">
            <Icon name="refresh" :size="14" /> 刷新
          </Button>
        </div>
        <div v-if="hopStatusResult" class="an-hop-status">
          <div class="an-hop-row">
            <span>防火墙后端</span>
            <span class="mono">{{ hopStatusResult.backend || '—' }}</span>
            <Badge :variant="hopStatusResult.persist_available ? 'success' : 'warning'" dot>
              {{ hopStatusResult.persist_available ? '可持久化' : '无 netfilter-persistent' }}
            </Badge>
          </div>
          <div v-if="hopChain" class="an-hop-row">
            <span>链</span>
            <span class="mono">{{ hopChain.name }}</span>
            <Badge :variant="hopChain.jumps?.length ? 'success' : 'warning'" dot>
              {{ hopChain.jumps?.length ? '已挂 PREROUTING' : '未挂跳转' }}
            </Badge>
            <Badge :variant="hopChain.persisted ? 'success' : 'warning'" dot>
              {{ hopChain.persisted ? '已持久化' : '未持久化（重启即丢）' }}
            </Badge>
          </div>
          <div v-else class="an-hop-row an-hop-dim">
            <span>链</span>
            <span>节点上没有 HY2HOP-{{ hopInbound?.id }}（尚未应用）</span>
          </div>
          <ul v-if="hopChain?.rules?.length" class="an-hop-rules">
            <li v-for="(rule, i) in hopChain.rules" :key="i" class="mono">{{ rule }}</li>
          </ul>
          <ul v-if="hopChain?.jumps?.length" class="an-hop-rules">
            <li v-for="(jump, i) in hopChain.jumps" :key="'j' + i" class="mono">PREROUTING {{ jump }}</li>
          </ul>
          <p v-if="hopStatusResult.reason" class="an-hop-error">{{ hopStatusResult.reason }}</p>
        </div>
        <p v-else class="an-hop-dim">还没读取到状态 —— 点「刷新」。</p>

        <div v-if="hopApplyResult" class="an-hop-status">
          <div class="an-hop-row">
            <span>上次下发</span>
            <span class="mono">{{ hopApplyResult.chain || '—' }}</span>
            <Badge :variant="hopApplyResult.verified ? 'success' : 'danger'" dot>
              {{ hopApplyResult.verified ? 'verified' : '未通过自验' }}
            </Badge>
            <Badge :variant="hopApplyResult.persisted ? 'success' : 'warning'" dot>
              {{ hopApplyResult.persisted ? 'persisted' : '未持久化' }}
            </Badge>
          </div>
        </div>
      </div>
      <template #footer>
        <Button variant="ghost" @click="hopOpen = false">关闭</Button>
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
.an-desc {
  margin: 0 0 var(--space-3);
  color: var(--text-muted);
  font-size: var(--text-sm);
}
.an-toolbar {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-bottom: var(--space-3);
}
/* 勾选列：窄列 + 右侧不留白，避免把表格撑宽 */
.an-check-cell {
  width: 36px;
  padding-right: 0 !important;
}
.an-check-cell input {
  width: 15px;
  height: 15px;
  cursor: pointer;
  accent-color: var(--accent);
}
/* 批量操作条：只在有勾选时出现，靠右 */
.an-bulk {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin-left: auto;
}
.an-bulk-count {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px;
  border-radius: 999px;
  background: var(--bg-hover);
  color: var(--text-muted);
  font-size: var(--text-xs);
}
.an-bulk-clear {
  border: none;
  background: transparent;
  color: var(--text-muted);
  font-size: 15px;
  line-height: 1;
  padding: 0;
  cursor: pointer;
}
.an-bulk-clear:hover { color: var(--text); }
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
/* 窄屏（手机）：表单两列并排会把控件压到只剩几十像素，一律单列。
   .an-grid-3（Reality 参数三列）同样并到单列。 */
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
.an-sniff-opts {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
}
.an-sniff-opts label {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: var(--text-sm);
  color: var(--text-secondary);
  cursor: pointer;
}
.an-section-title {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  color: var(--text-primary);
  margin-top: var(--space-1);
}
.an-hint {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
}
/* 表格里的次要信息（如 HY2 的跳跃区间） */
.an-hop {
  color: var(--text-muted);
  font-size: var(--text-xs);
}
/* ── 端口跳跃面板 ── */
.an-hop-panel {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.an-hop-actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-2);
}
.an-hop-ok {
  font-size: var(--text-xs);
  color: var(--text-secondary);
}
.an-hop-dim {
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.an-hop-error {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--danger, #e5484d);
  word-break: break-word;
}
.an-hop-status {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-3);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
}
.an-hop-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
}
.an-hop-row > span:first-child {
  min-width: 88px;
  color: var(--text-muted);
  font-size: var(--text-xs);
}
.an-hop-rules {
  margin: 0;
  padding-left: var(--space-4);
  font-size: var(--text-xs);
  color: var(--text-secondary);
  word-break: break-all;
}
/* ── Reality 高级参数折叠区 ── */
.an-adv {
  margin-top: var(--space-1);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
}
.an-adv-toggle {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  width: 100%;
  padding: var(--space-2) var(--space-3);
  background: transparent;
  border: none;
  cursor: pointer;
  color: var(--text-secondary);
  font-size: var(--text-sm);
  text-align: left;
}
.an-adv-toggle:hover {
  color: var(--text-primary);
}
.an-adv-body {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-3);
  border-top: 1px solid var(--border-subtle);
}
.an-grid-3 {
  grid-template-columns: 1fr 1fr 1fr;
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
/* ── Reality 目标检测弹窗 ── */
.an-section-actions {
  display: flex;
  gap: var(--space-2);
}
.an-scan {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.an-scan-tip {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
  line-height: 1.7;
}
.an-scan-bar {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.an-scan-bar :deep(.ch-input) {
  flex: 1;
  min-width: 0;
}
.an-scan-error {
  margin: 0;
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-md);
  background: var(--warning-muted);
  color: var(--warning);
  font-size: var(--text-xs);
}
.an-scan-table {
  max-height: 46vh;
  overflow: auto;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
}
.an-scan-table .an-table {
  font-size: var(--text-xs);
}
.an-scan-table .an-table th,
.an-scan-table .an-table td {
  padding: var(--space-2) var(--space-3);
}
.an-scan-table .an-table thead th {
  position: sticky;
  top: 0;
  z-index: 1;
  background: var(--bg-surface);
}
.an-scan-table tbody tr.is-picked {
  background: var(--accent-muted);
}
.an-scan-check {
  width: 15px;
  height: 15px;
  accent-color: var(--accent);
  cursor: pointer;
}
.an-scan-check:disabled {
  cursor: not-allowed;
  opacity: 0.4;
}
.an-scan-reason,
.an-scan-dim {
  display: block;
  margin-top: 2px;
  font-size: var(--text-xs);
  color: var(--text-muted);
  white-space: normal;
}
.an-scan-warn {
  margin-left: var(--space-1);
  color: var(--warning);
  font-size: var(--text-xs);
}
.an-scan-tag {
  margin-left: var(--space-1);
  padding: 0 6px;
  border-radius: 999px;
  background: var(--accent-muted);
  color: var(--accent);
  font-size: var(--text-xs);
}
.an-scan-empty {
  margin: 0;
  padding: var(--space-6);
  text-align: center;
  color: var(--text-muted);
  font-size: var(--text-sm);
}
.mono {
  font-family: var(--font-mono);
}

/* 表单栅格对齐：标签统一占两行高度，长标签换行时不会把输入框挤下去 */
.an-grid { align-items: start; }
.an-grid > .an-field > span:first-child { display: block; min-height: 2.6em; line-height: 1.35; }
</style>
