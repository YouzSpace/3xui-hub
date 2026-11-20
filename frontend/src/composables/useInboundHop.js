/**
 * HY2 端口跳跃（issue #94）：探测 → 应用 → 读回节点真实链状态。
 *
 * 三件事都在节点侧执行（agent 配防火墙），面板只转发白名单载荷；
 * 状态以「节点读回」为准，不看数据库里的意图。
 */
import { ref, computed } from 'vue'
import { adminApi } from '@/api/admin'

/**
 * @param {{
 *   nodeId: import('vue').Ref,
 *   hopSupported: import('vue').Ref,
 *   reload: () => Promise<void>,
 * }} deps
 */
export function useInboundHop({ nodeId, hopSupported, reload }) {
  // ── 端口跳跃（issue #94）──────────────────────────────────
  // 三件事都在节点侧执行（agent 配防火墙），面板只转发白名单载荷：
  //   probe  → 在池子里挑一段连续空闲端口（只读）
  //   apply  → 写 HY2HOP-<inbound_id> 链并自验 + 持久化
  //   status → 读回节点上真实的链状态（不是数据库里的意图）
  const hopOpen = ref(false)
  const hopInbound = ref(null) // 当前操作的入站
  const hopBusy = ref('') // 'probe' | 'apply' | 'remove' | 'status'
  const hopError = ref('')
  const hopProbe = ref(null) // port_probe 回执
  const hopProbePool = ref('20000-50000')
  const hopProbeCount = ref(200)
  const hopPorts = ref('') // 当前待应用的区间
  const hopStatusResult = ref(null) // firewall_hop_status 的 result
  const hopApplyResult = ref(null) // firewall_hop_apply / remove 的 result

  const hopNodeMissing = computed(() => !hopSupported.value)

  const openHop = async (ib) => {
    hopInbound.value = ib
    hopOpen.value = true
    hopError.value = ''
    hopProbe.value = null
    hopApplyResult.value = null
    hopPorts.value = ib.hy_hop_ports || ''
    hopStatusResult.value = null
    // 版本不足时不发指令（status 也会被后端 412 拒掉），直接显示升级提示。
    await refreshHopStatus()
  }

  // 「端口跳跃」区块的状态摘要：链在不在、是否 verified/persisted。
  const hopChain = computed(() => {
    const chains = hopStatusResult.value?.chains
    if (!Array.isArray(chains) || !hopInbound.value) return null
    return chains.find((c) => c.name === `HY2HOP-${hopInbound.value.id}`) || null
  })

  const refreshHopStatus = async () => {
    if (!nodeId.value || !hopInbound.value || !hopSupported.value) return
    hopBusy.value = 'status'
    hopError.value = ''
    try {
      const res = await adminApi.hopStatus(nodeId.value)
      if (res?.pending) hopError.value = res.message || '节点未及时回执'
      else if (res?.error) hopError.value = res.error
      else hopStatusResult.value = res?.result || null
    } catch (e) {
      hopError.value = e.message || '读取节点状态失败'
    } finally {
      hopBusy.value = ''
    }
  }

  const runHopProbe = async () => {
    if (!nodeId.value || !hopInbound.value || !hopSupported.value) return
    hopBusy.value = 'probe'
    hopError.value = ''
    try {
      const res = await adminApi.hopProbe(nodeId.value, {
        inbound_id: hopInbound.value.id,
        pool: hopProbePool.value,
        count: Number(hopProbeCount.value) || 200,
      })
      if (res?.pending) hopError.value = res.message || '节点未及时回执'
      else if (res?.error) hopError.value = res.error
      else {
        hopProbe.value = res?.result || null
        // 推荐区间直接填进「待应用」框，管理员可改
        if (hopProbe.value?.picked) hopPorts.value = hopProbe.value.picked
      }
    } catch (e) {
      hopError.value = e.message || '探测失败'
    } finally {
      hopBusy.value = ''
    }
  }

  const runHopApply = async () => {
    if (!nodeId.value || !hopInbound.value || !hopSupported.value) return
    hopBusy.value = 'apply'
    hopError.value = ''
    try {
      const res = await adminApi.hopApply(nodeId.value, {
        inbound_id: hopInbound.value.id,
        ports: hopPorts.value || '',
      })
      if (res?.pending) hopError.value = res.message || '节点未及时回执'
      else if (res?.error) {
        hopError.value = res.error
        hopApplyResult.value = res.result || null
      } else {
        hopApplyResult.value = res?.result || null
        if (!hopApplyResult.value?.verified) {
          hopError.value = '节点回执未通过自验（verified=false），规则可能没生效'
        }
        await refreshHopStatus()
        await reload()
      }
    } catch (e) {
      hopError.value = e.message || '应用失败'
    } finally {
      hopBusy.value = ''
    }
  }

  const runHopRemove = async () => {
    if (!nodeId.value || !hopInbound.value || !hopSupported.value) return
    if (!confirm(`删除 ${hopInbound.value.tag} 的跳跃防火墙规则（HY2HOP-${hopInbound.value.id}）？\n删除后跳跃区间内的端口不再转发到主端口。`)) return
    hopBusy.value = 'remove'
    hopError.value = ''
    try {
      const res = await adminApi.hopRemove(nodeId.value, { inbound_id: hopInbound.value.id })
      if (res?.pending) hopError.value = res.message || '节点未及时回执'
      else if (res?.error) hopError.value = res.error
      else {
        hopApplyResult.value = res?.result || null
        await refreshHopStatus()
      }
    } catch (e) {
      hopError.value = e.message || '删除失败'
    } finally {
      hopBusy.value = ''
    }
  }

  return {
    hopOpen,
    hopInbound,
    hopBusy,
    hopError,
    hopProbe,
    hopProbePool,
    hopProbeCount,
    hopPorts,
    hopStatusResult,
    hopApplyResult,
    hopNodeMissing,
    hopChain,
    openHop,
    refreshHopStatus,
    runHopProbe,
    runHopApply,
    runHopRemove,
  }
}
