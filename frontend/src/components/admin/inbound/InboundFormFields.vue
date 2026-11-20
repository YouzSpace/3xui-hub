<script setup>
/**
 * 入站表单字段区：协议/端口/传输/安全层/地址族/tag/备注 + 传输层细节（tcp / ws /
 * httpupgrade / xhttp）。协议专属字段各自成组件，渲染顺序与拆分前完全一致：
 *   Reality → 传输细节 → TLS 证书 → HY2 → SS/代理/Tunnel/WG/嗅探
 *
 * form 是上下文里的表单对象，直接写它的字段（与拆分前同一份响应式数据）。
 */
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import { protocolOptions } from '@/composables/inboundFormModel'
import { useInboundCtx, writable } from './formContext'
import InboundRealityFields from './InboundRealityFields.vue'
import InboundTlsFields from './InboundTlsFields.vue'
import InboundHyFields from './InboundHyFields.vue'
import InboundProtoFields from './InboundProtoFields.vue'

const {
  form,
  customAddress,
  listenOptions,
  listenFamily,
  hideTransport,
  hideSecurity,
  isSs,
  networkOptions,
  securityOptions,
  isWs,
  isXhttp,
  isHttpupgrade,
} = useInboundCtx()

const listenFamilyModel = writable(listenFamily)

const tcpHeaderOptions = [
  { label: '无', value: 'none' },
  { label: 'HTTP 伪装', value: 'http' },
]
</script>

<template>
  <div class="ch-form">
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
  没填时仍回落到主地址。选「自定义」则不改绑定地址，只把订阅链接里的地址换成下面填的地址（CDN 场景）。
</p>
<template v-if="customAddress">
  <label class="an-field">
    <span>客户端连接地址（CDN 域名 / 另一个 IP；端口仍是本入站端口）</span>
    <Input v-model="form.client_host" mono placeholder="如 cdn.example.com" />
  </label>
  <p class="an-hint">
    客户端连这个地址，回源才连节点。证书 serverName、WS Host 仍按本入站自己的配置走。
  </p>
</template>
<label class="an-field">
  <span>标识 tag（留空自动生成，同节点内唯一；仅字母/数字/下划线/连字符）</span>
  <Input v-model="form.tag" placeholder="如 vless-reality" />
</label>
<label class="an-field">
  <span>备注（可选，可中文；订阅节点名优先展示它）</span>
  <Input v-model="form.remark" placeholder="如：香港中转" />
</label>
    <InboundRealityFields />
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
    <label class="ch-check-row">
      <input v-model="form.xhttp_no_sse_header" type="checkbox" />
      <span>noSSEHeader（禁用 SSE 头）</span>
    </label>
    <label class="ch-check-row">
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
  <label class="ch-check-row">
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
    <InboundTlsFields />
    <InboundHyFields />
    <InboundProtoFields />
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
