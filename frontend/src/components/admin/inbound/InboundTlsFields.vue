<script setup>
/**
 * TLS 证书组（security = tls）：serverName、节点证书签发、证书库引用与内联 PEM。
 *
 * 口径：HY2 只认证书库 id（不读 PEM），普通 TLS 入站两者都认（选了库条目就按引用走）。
 */
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import { useInboundCtx, writable } from './formContext'

const {
  form,
  editing,
  isTls,
  isHy,
  certificates,
  certSignDomain,
  certSignDomainIsIp,
  issuingCert,
  issueNodeCert,
  pemFilling,
  fillTlsPemFromLibrary,
  pemCertId,
  pemCertOptions,
  pemCertPlaceholder,
  pemPickable,
  certTargetHost,
} = useInboundCtx()

const pemCertIdModel = writable(pemCertId)
</script>

<template>
<template v-if="isTls">
  <label class="an-field">
    <span>TLS serverName</span>
    <Input v-model="form.tls_server_name" placeholder="your.domain.com" />
  </label>

  <!-- 远程签发：面板给裸内核节点下发 cert_issue，节点侧跑 acme.sh（HTTP-01），
       成功后后端把 PEM 写进证书库。签发域名跟上面的 serverName 走（证书必须覆盖客户端
       握手用的 SNI），只有 serverName 留空才回落节点 host；签完自动选到下方（HY2）
       或回填两个框（TLS）。 -->
  <div class="an-section-title">
    <span>节点证书签发</span>
    <span class="an-section-actions">
      <Button
        size="sm"
        variant="secondary"
        :disabled="!certSignDomain || certSignDomainIsIp || issuingCert"
        :loading="issuingCert"
        @click="issueNodeCert"
      >
        <Icon name="shield" :size="14" /> 签发证书
      </Button>
    </span>
  </div>
  <p class="an-hint">
    按 <code>{{ certSignDomain || '（未填域名）' }}</code> 向 Let's Encrypt 申请一张证书：
    HTTP-01 校验，这个域名要能解析到本节点、节点的 80 端口要能从公网打到本机，签发要等几十秒。
    签好自动写进「TLS 证书库」并用于本表单。
    <template v-if="certSignDomainIsIp">当前填的是 IP，Let's Encrypt 不签 IP，请先把 serverName 改成域名。</template>
  </p>

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
      <span>证书库条目（引用证书库；选中后 PEM 可留空）</span>
      <Select v-model="pemCertIdModel" :options="pemCertOptions" :placeholder="pemCertPlaceholder" />
    </label>
    <p v-if="!pemPickable.length" class="an-hint">
      证书库里没有能用于 <code>{{ certTargetHost || '当前域名' }}</code> 的证书（没有、已过期或域名不匹配）：
      先点上方「签发证书」签一张，签完会自动填进来。
    </p>
    <div class="an-grid">
      <label class="an-field">
        <span>证书 PEM{{ editing && editing.has_tls_cert ? '（不改留空）' : '（引用证书库时可留空）' }}</span>
        <textarea v-model="form.tls_certificate" class="an-textarea" rows="3" placeholder="-----BEGIN CERTIFICATE-----" />
      </label>
      <label class="an-field">
        <span>私钥 PEM{{ editing && editing.has_tls_cert ? '（不改留空）' : '（引用证书库时可留空）' }}</span>
        <textarea v-model="form.tls_key" class="an-textarea" rows="3" placeholder="-----BEGIN PRIVATE KEY-----" />
      </label>
    </div>
    <p class="an-hint">
      选中证书库条目 = 引用（推荐）：PEM 可以留空，续签由证书库统一处理、节点自动重拉；
      手贴 PEM 也行，保存时会按内容写进证书库并自动挂上引用，之后同样自动续签。
    </p>
  </template>
  <p v-else class="an-hint">
    HY2 不读内联 PEM：证书走下方「TLS 证书库」引用，入站里只存证书 id、私钥留在库里。
    上面的 serverName 就是客户端握手用的域名，填对之后点证书区的「自动填充证书」即可挑到匹配的那张。
  </p>
</template>
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
