<script setup>
/**
 * HY2 端口跳跃弹窗（issue #94）：① 探测空闲区间 → ② 应用防火墙规则 →
 * ③ 读回节点上的真实链状态。跳跃本身在节点侧执行，面板只下发白名单指令。
 */
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import Input from '@/components/ui/Input.vue'
import Modal from '@/components/ui/Modal.vue'
import { useInboundCtx, writable } from './formContext'

const {
  hopOpen,
  hopInbound,
  hopMinAgentVersion,
  hopNodeMissing,
  hopProbe,
  hopProbePool,
  hopProbeCount,
  hopPorts,
  hopBusy,
  hopError,
  hopStatusResult,
  hopApplyResult,
  hopChain,
  refreshHopStatus,
  runHopProbe,
  runHopApply,
  runHopRemove,
} = useInboundCtx()

const hopOpenModel = writable(hopOpen)
const hopPortsModel = writable(hopPorts)
const hopProbePoolModel = writable(hopProbePool)
const hopProbeCountModel = writable(hopProbeCount)
</script>

<template>
<!-- 端口跳跃（issue #94）：探测 → 应用 → 读回节点上的真实链状态 -->
<Modal v-model="hopOpenModel" :title="`端口跳跃 ${hopInbound ? hopInbound.tag : ''}`" width="720px">
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
        <Input v-model="hopProbePoolModel" placeholder="20000-50000" :disabled="hopNodeMissing" />
      </label>
      <label class="an-field">
        <span>需要连续端口数</span>
        <Input v-model.number="hopProbeCountModel" type="number" placeholder="200" :disabled="hopNodeMissing" />
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
        <Input v-model="hopPortsModel" placeholder="20000-20199" :disabled="hopNodeMissing" />
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
    <Button variant="ghost" @click="hopOpenModel = false">关闭</Button>
  </template>
</Modal>
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
