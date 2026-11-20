<script setup>
/**
 * Reality 参数组（仅 vless + reality）：伪装目标、serverNames、shortIds、公钥，
 * 以及折叠的高级参数（xver / 客户端版本 / 后量子 / fallback 限速）。
 */
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import { useInboundCtx, writable } from './formContext'

const { form, isReality, nodeId, generating, generateReality, openScan, advOpen } = useInboundCtx()

const advOpenModel = writable(advOpen)

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
</script>

<template>
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
    <button type="button" class="an-adv-toggle" @click="advOpenModel = !advOpenModel">
      <Icon :name="advOpenModel ? 'chevron-down' : 'chevron-right'" :size="14" />
      高级参数（xver / 客户端版本 / 后量子 / fallback 限速）
    </button>
    <div v-if="advOpenModel" class="an-adv-body">
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
      <label class="ch-check-row">
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
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
