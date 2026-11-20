/**
 * 入站表单 → 提交给后端的 payload（纯函数）。
 *
 * 分协议/传输/安全层拼字段，全量提交保证「编辑时清空」能真的落空。
 * 与页面状态无关，便于单测；不在表单里的校验（如 serverNames 站点一致）由后端负责。
 */
import { splitList, joinHopRange } from '@/composables/inboundFormModel'

/**
 * 提交前的前端校验。返回 null 表示通过，否则返回 { error, openAdvanced? }：
 *   error        —— 直接给用户看的提示文案
 *   openAdvanced —— 需要把「高级参数」折起来的那一块展开（否则用户看不到要改哪个框）
 */
export function validateInboundForm(formRef, { customAddress, isTls, isHy, isReality, pemCertId }) {
  const form = formRef.value
  // 选了「自定义客户端地址」就必须填一个地址（留空等于没选，却挡住用户不知道怎么办）
  if (customAddress && !String(form.client_host || '').trim()) {
    return { error: '地址族选了「自定义」：请在下面填入 CDN 域名或 IP，或把地址族改回 IPv4/IPv6' }
  }
  // TLS 入站（非 HY2）必须能落一张证书：引用证书库（推荐）或手填 PEM。
  // 引用证书库才能进续签链（tlsCertId），只填 PEM 时后端会按内容自动入库并补上引用。
  if (isTls && !isHy) {
    const hasRef = !!pemCertId
    const hasPem =
      !!String(form.tls_certificate || '').trim() && !!String(form.tls_key || '').trim()
    if (!hasRef && !hasPem) {
      return { error: 'TLS 入站需要先选一张证书库证书（或点上方「签发证书」），也可以手填证书与私钥' }
    }
  }
  // 后量子材料成对校验（前后端同守）：只在一边填会让 REALITY 握手失败
  if (isReality) {
    const seed = (form.mldsa65_seed || '').trim()
    const verify = (form.mldsa65_verify || '').trim()
    if ((seed === '') !== (verify === '')) {
      return { error: 'mldsa65Seed 与 mldsa65Verify 必须成对填写（或都留空）', openAdvanced: true }
    }
  }
  return null
}

/** 表单 ref → payload。flags 是表单联动出来的协议/传输/安全层布尔。 */
export function buildInboundPayload(
  form,
  { isReality, isWs, isXhttp, isHttpupgrade, isTls, isSs, isProxyProto, isTunnel, isWg, isHy },
  { forceServerNames = false, pemCertId = null } = {},
) {
  const payload = {
    protocol: form.value.protocol,
    tag: form.value.tag || null,
    remark: form.value.remark || null,
    port: Number(form.value.port),
    listen: form.value.listen,
    client_host: String(form.value.client_host || '').trim(),
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
    // TLS 入站一律引用证书库（与 HY2 同一条续签链）：选中条目 → 只存 tlsCertId，PEM 可留空；
    // 没选但贴了 PEM → 后端按内容匹配/入库后自动补上引用。
    if (!isHy.value && pemCertId.value) payload.tls_cert_id = Number(pemCertId.value)
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
    // 跳跃区间：UI 是起点/终点两个数字框，提交时拼回半角 a-b（提交口径不变）。
    // 边界：只填一边 = 视为没填全，当作空提交（后端对残缺串必然报格式错，
    // 与其让报错信息难懂，这里直接当「不跳跃」；真想跳就得填全两边）。
    payload.hy_hop_ports = joinHopRange(form.value.hy_hop_start, form.value.hy_hop_end)
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
  return payload
}
