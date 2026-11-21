<script setup>
/**
 * 其它协议与全局开关：Shadowsocks、SOCKS5/HTTP 代理、Tunnel、WireGuard，
 * 以及嗅探（issue #39）与启用开关。
 */
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import { useInboundCtx } from './formContext'

const {
  form,
  editing,
  isSs,
  ssIs2022,
  isProxyProto,
  isSocks,
  isTunnel,
  isWg,
  regionChoices,
} = useInboundCtx()

const ssMethodOptions = [
  { label: 'aes-256-gcm', value: 'aes-256-gcm' },
  { label: 'aes-128-gcm', value: 'aes-128-gcm' },
  { label: 'chacha20-poly1305', value: 'chacha20-poly1305' },
  { label: 'xchacha20-poly1305', value: 'xchacha20-poly1305' },
  { label: '2022-blake3-aes-256-gcm', value: '2022-blake3-aes-256-gcm' },
  { label: '2022-blake3-aes-128-gcm', value: '2022-blake3-aes-128-gcm' },
]

const sniffDestOptions = [
  { label: 'HTTP', value: 'http' },
  { label: 'TLS', value: 'tls' },
  { label: 'QUIC', value: 'quic' },
  { label: 'FakeDNS（需配合 FakeDNS 使用）', value: 'fakedns' },
]

const tunnelNetworkOptions = [
  { label: 'TCP + UDP（推荐）', value: 'tcp,udp' },
  { label: '仅 TCP', value: 'tcp' },
  { label: '仅 UDP', value: 'udp' },
]
</script>

<template>
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

<!-- 归属分组（#入站级分组）：不选 = 跟随节点地区；选了 = 这个入站单独归到该地区 -->
<label class="an-field">
  <span>归属分组</span>
  <Select v-model="form.region_id" :options="regionChoices" />
</label>
<p class="an-hint">
  「跟随节点」时，这个入站和节点其他入站一样按节点地区卖；选了某个分组，就只有套餐包含该分组的用户能看到它。
  只影响订阅里显示哪些入口，不影响用户与节点的分配关系。
</p>

<!-- 嗅探（issue #39）：默认对齐现状；不改则行为无感 -->
<label class="ch-check-row">
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
  <label class="ch-check-row">
    <input v-model="form.sniffing_route_only" type="checkbox" />
    <span>仅用于路由（routeOnly）——嗅探结果不改写回源地址</span>
  </label>
</template>
<label class="ch-check-row">
  <input v-model="form.enabled" type="checkbox" />
  <span>启用（停用后从节点配置中移除）</span>
</label>
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
