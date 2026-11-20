<script setup>
/**
 * Hysteria2 参数组：跳跃区间（起点/终点两框）、QUIC 拥塞与 brutal 带宽、接收窗口、
 * salamander 混淆、masquerade 伪装、证书库引用与 TLS 高级开关。
 */
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import { useInboundCtx } from './formContext'

const {
  form,
  editing,
  isHy,
  isHyBrutal,
  certificates,
  certOptions,
  certTargetHost,
  selectedCertProblem,
  hasFittingCert,
  autoFillCert,
} = useInboundCtx()

const hyMasqueradeOptions = [
  { label: '不伪装（内核默认）', value: '' },
  { label: 'string（固定文本）', value: 'string' },
  { label: 'file（固定目录）', value: 'file' },
  { label: 'proxy（反代到 URL）', value: 'proxy' },
]

const hyCongestionOptions = [
  { label: '内核默认（bbr）', value: '' },
  { label: 'reno', value: 'reno' },
  { label: 'bbr', value: 'bbr' },
  { label: 'brutal', value: 'brutal' },
  { label: 'force-brutal', value: 'force-brutal' },
]
</script>

<template>
<template v-if="isHy">
  <div class="an-section-title"><span>Hysteria2 参数</span></div>
  <div class="an-grid">
    <label class="an-field">
      <span>跳跃区间（留空 = 不跳跃）</span>
      <!-- 起点/终点拆两框，「-」写死：用户只填数字，杜绝全角/en-dash 连字符导致的
           「跳跃区间格式错误」（真实踩过：30000–30099 用了 en-dash 直接被后端拒）。 -->
      <div class="an-hop-range">
        <Input
          v-model="form.hy_hop_start"
          type="number"
          inputmode="numeric"
          placeholder="30000"
          :min="1"
          :max="65535"
        />
        <span class="an-hop-dash">–</span>
        <Input
          v-model="form.hy_hop_end"
          type="number"
          inputmode="numeric"
          placeholder="30099"
          :min="1"
          :max="65535"
        />
      </div>
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
  <label v-if="editing && editing.hy_salamander_password_set" class="ch-check-row">
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
  <p v-if="certificates.length && !hasFittingCert" class="an-hint">
    没有覆盖 <code>{{ certTargetHost }}</code> 的证书：先点上方「签发证书」签一张，签完会自动选中。
  </p>
  <p v-if="certificates.length" class="an-hint">
    灰色条目的证书域名覆盖不到
    <code>{{ certTargetHost || '当前节点' }}</code>
    ，客户端按 SNI 握手会失败，所以不给选。「自动填充证书」会按 serverName / 节点域名挑一张
    未过期的填进来；没有条目被置灰说明还判断不出该用哪个域名（serverName 与节点域名都空）。
  </p>

  <div class="an-section-title"><span>TLS 高级开关</span></div>
  <label class="ch-check-row">
    <input v-model="form.hy_reject_unknown_sni" type="checkbox" />
    <span>拒绝未知 SNI（客户端填的 SNI 与证书不匹配就不握手）</span>
  </label>
  <label class="ch-check-row">
    <input v-model="form.hy_session_resumption" type="checkbox" />
    <span>会话恢复（复用 TLS 会话，减少重复握手开销）</span>
  </label>
  <p class="an-hint">
    两项默认都不开（与内核一致）。开了「拒绝未知 SNI」后，客户端用伪装 SNI（
    <code>insecure=1</code> 随便填个域名）会连不上，只有 SNI 和证书对得上才行。
  </p>
</template>
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
