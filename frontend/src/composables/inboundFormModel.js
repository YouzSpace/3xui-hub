/**
 * xray 入站表单的纯数据模型（无 Vue 依赖）：协议/传输白名单、表单默认值、
 * 入站记录 → 表单回填、跳跃区间与逗号列表的拆分合并。
 *
 * 这里是「后端口径」的常量与纯函数；纯 UI 文案（tcpHeaderOptions 等）留在用它的组件里。
 */

export const protocolOptions = [
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

export const NETWORK_ALL = [
  { label: 'TCP', value: 'tcp' },
  { label: 'WebSocket', value: 'ws' },
  { label: 'gRPC', value: 'grpc' },
  { label: 'xhttp', value: 'xhttp' },
  { label: 'HTTPUpgrade', value: 'httpupgrade' },
]
export const REALITY_NETWORKS = ['tcp', 'xhttp', 'grpc']

export const emptyLimitFallback = () => ({ after_bytes: 0, bytes_per_sec: 0, burst_bytes_per_sec: 0 })

export const emptyForm = () => ({
  protocol: 'vless',
  tag: '',
  remark: '',
  port: 443,
  listen: '0.0.0.0',
  // 客户端连接地址（可选）：填了订阅链接就用它（CDN 域名等），端口仍用本入站端口
  client_host: '',
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
  ss_method: '2022-blake3-aes-256-gcm',
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
  // 跳跃区间拆成两个数字框（起点/终点），中间的「-」写死 —— 用户手敲连字符常敲成
  // 全角或 en-dash，后端校验只认半角 20000-30000，直接报「区间格式错误」。
  // 两个框都空 = 不跳跃；填了就把两半拼回 a-b 提交（提交口径不变）。
  hy_hop_ports: '',
  hy_hop_start: '',
  hy_hop_end: '',
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

// ── HY2 跳跃区间：UI 是起点/终点两个数字框，中间「-」写死 ──
// 背景：整个输入框让用户手敲连字符时，中文输入法常打成全角「－」或 en-dash「–」，
// 后端只认半角 a-b，直接报「跳跃区间格式错误：应形如 20000-30000」。
// 拆两框后用户只碰数字，符号由代码生成 —— 这类提交错误从根上消失。
//
// hopSplitRe 半角 a-b（允许首尾空格）；同时把全角－／en-dash／em-dash 归一成半角，
// 兼容历史数据里已经存进去的错字符（回显时还能拆得开）。
const HOP_DASH_RE = /[–—－]/g
const hopSplitRe = /^(\d{1,5})\s*-\s*(\d{1,5})$/

/** "20000-20199" → { hy_hop_start: "20000", hy_hop_end: "20199" }；解析不了就两框留空。 */
export function splitHopRange(raw) {
  const s = String(raw || '').trim().replace(HOP_DASH_RE, '-')
  const m = hopSplitRe.exec(s)
  if (!m) return { hy_hop_start: '', hy_hop_end: '' }
  return { hy_hop_start: m[1], hy_hop_end: m[2] }
}

/** 起点/终点 → "a-b"。只填一边视为没填全（返回空串=不跳跃），两边都填才拼。 */
export function joinHopRange(start, end) {
  const a = String(start ?? '').trim()
  const b = String(end ?? '').trim()
  if (!a && !b) return ''
  if (!a || !b) return ''
  return `${a}-${b}`
}

export const splitList = (s) =>
  String(s || '')
    .split(/[,，\s]+/)
    .map((x) => x.trim())
    .filter(Boolean)

/**
 * 入站记录 → 表单值（编辑回填）。密码类字段一律留空：留空 = 不改。
 * 默认值来源与 emptyForm 保持一致，不再另抄一份。
 */
export const formFromInbound = (ib) => ({
  protocol: ib.protocol,
  tag: ib.tag,
  remark: ib.remark || '',
  port: ib.port,
  listen: ib.listen || '0.0.0.0',
  client_host: ib.client_host || '',
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
  ss_method: ib.ss_method || '2022-blake3-aes-256-gcm',
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
  ...splitHopRange(ib.hy_hop_ports || ''),
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
})
