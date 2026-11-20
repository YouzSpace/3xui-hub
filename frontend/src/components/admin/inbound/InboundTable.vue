<script setup>
/**
 * 入站列表表格：勾选列 / 协议 / 端口 / 传输 / 安全层 / 用户数 / 状态 / 操作。
 *
 * 纯展示 + 事件上抛：数据、勾选集合、删除动作都由页面容器持有。
 */
import { computed } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'

const props = defineProps({
  inbounds: { type: Array, default: () => [] },
  loading: { type: Boolean, default: false },
  selectedIds: { type: Array, default: () => [] },
  nodeId: { type: [Number, String], default: null },
  hopSupported: { type: Boolean, default: false },
  hopMinAgentVersion: { type: String, default: '1.0.7' },
})

const emit = defineEmits(['update:selectedIds', 'edit', 'delete', 'hop'])

const isSelected = (ib) => props.selectedIds.includes(ib.id)
const allSelected = computed(
  () => props.inbounds.length > 0 && props.selectedIds.length === props.inbounds.length,
)

const toggleSelect = (ib) => {
  emit(
    'update:selectedIds',
    isSelected(ib) ? props.selectedIds.filter((id) => id !== ib.id) : [...props.selectedIds, ib.id],
  )
}

const toggleSelectAll = () => {
  emit('update:selectedIds', allSelected.value ? [] : props.inbounds.map((ib) => ib.id))
}

// 动作统一改名成 emit，模板与拆分前保持一字不差
const openEdit = (ib) => emit('edit', ib)
const remove = (ib) => emit('delete', ib)
const openHop = (ib) => emit('hop', ib)

const protocolLabel = { vless: 'VLESS', vmess: 'VMess', trojan: 'Trojan', shadowsocks: 'SS', hysteria: 'HY2', socks: 'SOCKS', http: 'HTTP', tunnel: 'Tunnel', wireguard: 'WG' }
const protocolVariant = (p) => ({ vless: 'vless', trojan: 'trojan' }[p] || 'info')
const securityVariant = (s) => ({ reality: 'info', tls: 'success' }[s] || 'info')
// 用户数一列：这几种协议没有用户概念（凭据在入站级或根本没有），不按用户下发客户端，
// 列表里显示「—」。ss 以前也走这个特判（单凭据），SS-2022 多用户后它同样是逐用户
// client，跟其它协议一样显示真实 client_count。
const CLIENTLESS_PROTOCOLS = ['socks', 'http', 'tunnel', 'wireguard']
const clientLabel = (ib) => {
  if (CLIENTLESS_PROTOCOLS.includes(ib.protocol)) return '—'
  return ib.client_count
}

// 列表行上的小标记：只表示「库里配了区间」，真实状态要点进区块读 status。
const hopBadge = (ib) => {
  return ib.hy_hop_ports ? `跳跃 ${ib.hy_hop_ports}` : ''
}
</script>

<template>
<Card padding="false">
  <div class="an-table-wrap">
    <table class="an-table">
      <thead>
        <tr>
          <th class="an-check-cell">
            <input
              type="checkbox"
              :checked="allSelected"
              :disabled="!inbounds.length"
              title="全选 / 取消全选"
              @change="toggleSelectAll"
            />
          </th>
          <th>标识</th>
          <th>备注</th>
          <th>协议</th>
          <th>端口</th>
          <th>传输</th>
          <th>安全</th>
          <th>用户数</th>
          <th>状态</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="ib in inbounds" :key="ib.id">
          <td class="an-check-cell">
            <input type="checkbox" :checked="isSelected(ib)" @change="toggleSelect(ib)" />
          </td>
          <td class="mono">{{ ib.tag }}</td>
          <td>{{ ib.remark || '—' }}</td>
          <td><Badge :variant="protocolVariant(ib.protocol)">{{ protocolLabel[ib.protocol] || ib.protocol }}</Badge></td>
          <td class="mono">{{ ib.port }}</td>
          <td class="mono">
            {{ ib.network }}
            <span v-if="hopBadge(ib)" class="an-hop"> · {{ hopBadge(ib) }}</span>
          </td>
          <td>
            <Badge :variant="securityVariant(ib.security)">{{ ib.security === 'reality' ? 'Reality' : ib.security === 'tls' ? 'TLS' : '无' }}</Badge>
          </td>
          <td class="mono">{{ clientLabel(ib) }}</td>
          <td>
            <Badge :variant="ib.enabled ? 'success' : 'warning'" dot>{{ ib.enabled ? '启用' : '停用' }}</Badge>
          </td>
          <td>
            <div class="ch-actions">
              <Button
                v-if="ib.protocol === 'hysteria'"
                size="sm"
                variant="ghost"
                :title="hopSupported ? '端口跳跃（节点侧防火墙）' : `需升级 agent（≥ ${hopMinAgentVersion}）`"
                :disabled="!hopSupported"
                @click="openHop(ib)"
              >
                <Icon name="activity" :size="16" />
              </Button>
              <Button size="sm" variant="ghost" @click="openEdit(ib)"><Icon name="edit" :size="16" /></Button>
              <Button size="sm" variant="ghost" @click="remove(ib)"><Icon name="trash" :size="16" /></Button>
            </div>
          </td>
        </tr>
        <tr v-if="!loading && inbounds.length === 0">
          <td colspan="10" class="ch-empty">{{ nodeId ? '该节点还没有入站 —— 点右上角「新建入站」开始' : '请先在上方选择一个 xray 节点' }}</td>
        </tr>
      </tbody>
    </table>
  </div>
</Card>
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
