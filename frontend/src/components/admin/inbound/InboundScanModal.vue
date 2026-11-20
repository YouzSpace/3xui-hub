<script setup>
/**
 * Reality 目标检测弹窗：在节点上跑真实 TLS 握手，勾一个可用目标后回填
 * serverNames（同一张证书的 SAN 全集）+ dest。
 */
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import Input from '@/components/ui/Input.vue'
import Modal from '@/components/ui/Modal.vue'
import { useInboundCtx, writable } from './formContext'

const {
  scanOpen,
  scanInput,
  scanning,
  scanResults,
  scanSelected,
  scanError,
  fastestTarget,
  runScan,
  toggleScan,
  formatChain,
  applyScan,
} = useInboundCtx()

const scanOpenModel = writable(scanOpen)
const scanInputModel = writable(scanInput)
</script>

<template>
<Modal v-model="scanOpenModel" title="Reality 目标检测" width="900px">
  <div class="an-scan">
    <p class="an-scan-tip">
      在<strong>当前节点</strong>上发起真实 TLS 握手：逐项验证 TLS 1.3 / HTTP2(h2) / X25519 / 证书链是否受信任，
      四项全过才判「可用」。延迟是节点到目标的握手耗时（节点视角才是 REALITY 真正走的路径）。
      勾选一个可用目标 → 填入该目标证书的 serverNames 全集（必须同一张证书，
      跨证书混填会导致 REALITY 握手失败），其地址作为 dest。
    </p>

    <div class="an-scan-bar">
      <Input v-model="scanInputModel" placeholder="自定义目标（逗号/空格分隔，可带端口，默认 443）；留空用内置常用候选" />
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
    <Button variant="ghost" @click="scanOpenModel = false">关闭</Button>
    <Button variant="secondary" :loading="scanning" @click="runScan">重新检测</Button>
    <Button variant="primary" :disabled="scanSelected.length === 0" @click="applyScan">
      填入 serverNames
    </Button>
  </template>
</Modal>
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
