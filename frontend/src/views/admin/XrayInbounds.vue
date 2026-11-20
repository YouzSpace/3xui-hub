<script setup>
/**
 * xray 节点配置 · 入站管理（页面容器）。
 *
 * 页面只做四件事：列表数据 + 勾选、模板编排、把「表单 / 证书库 / Reality 检测 /
 * 端口跳跃」四块上下文 provide 给子组件。字段渲染在 components/admin/inbound/ 下，
 * 表单状态与提交在 composables/useInboundForm.js。
 */
import { ref, computed, watch, provide } from 'vue'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import XrayNodePicker from '@/components/admin/XrayNodePicker.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { useInboundForm } from '@/composables/useInboundForm'
import { useInboundScan } from '@/composables/useInboundScan'
import { useInboundHop } from '@/composables/useInboundHop'
import {
  joinHopRange,
  protocolOptions,
  splitHopRange,
} from '@/composables/inboundFormModel'
import { INBOUND_CTX } from '@/components/admin/inbound/formContext'
import InboundTable from '@/components/admin/inbound/InboundTable.vue'
import InboundFormModal from '@/components/admin/inbound/InboundFormModal.vue'
import InboundScanModal from '@/components/admin/inbound/InboundScanModal.vue'
import InboundHopModal from '@/components/admin/inbound/InboundHopModal.vue'

const toast = useToast()
const nodeId = ref(null)
const inbounds = ref([])
const loading = ref(false)
// 端口跳跃的节点级能力（issue #94）：agent ≥ 1.0.7 才认那四个 action。
const hopSupported = ref(false)
const hopMinAgentVersion = ref('1.0.7')
// 多选删除用的勾选集合（只存 id，切节点清空）
const selectedIds = ref([])

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

const formApi = useInboundForm({ nodeId, onSaved: load })
const scan = useInboundScan({ nodeId, form: formApi.form })
const hop = useInboundHop({ nodeId, hopSupported, reload: load })

// 字段组件与三个弹窗都从这份上下文取状态（见 components/admin/inbound/formContext.js）
provide(INBOUND_CTX, { ...formApi, ...scan, ...hop, nodeId, hopSupported, hopMinAgentVersion })

// 切节点要清空勾选：跨节点的 id 没有意义；节点 host 也要重取（HY2 证书按它做域名匹配）
watch(nodeId, () => { selectedIds.value = []; load(); formApi.loadNodeHost() })

/* ── 多选 / 批量删除（对齐 3x-ui 入站列表的 checkbox + bulk delete） ── */

const selectedCount = computed(() => selectedIds.value.length)

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

// XrayInbounds*.spec.js 直接读 wrapper.vm 上的这些名字（form / listenFamily / save ...），
// 解构到组件实例以保持绑定名不变；模板只用到上面声明的那几个。
const {
  form,
  customAddress,
  listenFamily,
  listenOptions,
  networkOptions,
  securityOptions,
  openCreate,
  openEdit,
  save,
  pemCertId,
  pemCertPlaceholder,
  certOptions,
  certTargetHost,
  certSignDomain,
  certSignDomainIsIp,
  issueNodeCert,
  fillTlsPemFromLibrary,
  autoFillCert,
  nodeHost,
  openHop,
  hopChain,
  hopPorts,
  hopApplyResult,
  runHopProbe,
  runHopApply,
  runHopRemove,
} = { ...formApi, ...scan, ...hop }
</script>

<template>
  <div class="an">
<div class="an-head">
  <h1 class="ch-page-title">xray 入站</h1>
  <Button variant="primary" size="sm" :disabled="!nodeId" @click="openCreate">
    <Icon name="plus" :size="16" /> 新建入站
  </Button>
</div>
<p class="ch-desc">
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

    <InboundTable
      v-model:selected-ids="selectedIds"
      :inbounds="inbounds"
      :loading="loading"
      :node-id="nodeId"
      :hop-supported="hopSupported"
      :hop-min-agent-version="hopMinAgentVersion"
      @edit="openEdit"
      @delete="remove"
      @hop="openHop"
    />

    <InboundFormModal />
    <InboundScanModal />
    <InboundHopModal />
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
