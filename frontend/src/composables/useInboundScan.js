/**
 * Reality 目标检测弹窗：节点侧真实 TLS 握手（TLS1.3 + h2 + X25519 + 证书链受信任）。
 *
 * 选中目标后把该目标证书的 serverNames 全集填进表单（必须同一张证书，跨证书混填会握手失败）。
 */
import { ref, computed } from 'vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { splitList } from '@/composables/inboundFormModel'

/**
 * @param {{ nodeId: import('vue').Ref, form: import('vue').Ref }} deps
 */
export function useInboundScan({ nodeId, form }) {
  const toast = useToast()

  // ── Reality 目标检测（节点侧真实 TLS 握手）────────────────
  // 判定口径与 3x-ui 一致：TLS 1.3 + h2 + X25519 + 证书链受信任，四项全过才算可用。
  const scanOpen = ref(false)
  const scanInput = ref('')
  const scanning = ref(false)
  const scanResults = ref([])
  const scanSelected = ref([])
  const scanError = ref('')

  // 最快的一个可用目标（结果已按「可用 → 延迟升序」排好）
  const fastestTarget = computed(() => scanResults.value.find((r) => r.feasible)?.target || '')

  const openScan = () => {
    scanOpen.value = true
    scanResults.value = []
    scanSelected.value = []
    scanError.value = ''
  }

  const runScan = async () => {
    if (!nodeId.value) return
    scanning.value = true
    scanError.value = ''
    try {
      const res = await adminApi.scanRealityTargets(nodeId.value, splitList(scanInput.value))
      if (res?.pending) {
        scanResults.value = []
        scanError.value = res.message || '节点未及时回执，请稍后重试'
      } else if (res?.error) {
        scanResults.value = []
        scanError.value = res.error
      } else {
        scanResults.value = res?.results || []
        // 默认勾上最快的一个（serverNames 取所选目标证书的 SAN 全集，单选语义）
        scanSelected.value = fastestTarget.value ? [fastestTarget.value] : []
      }
    } catch (e) {
      scanResults.value = []
      scanError.value = e.message || '检测失败'
    } finally {
      scanning.value = false
    }
  }

  // serverNames 必须来自同一张证书（REALITY 服务端按 dest 回源证书做 SNI 精确匹配），
  // 故选择语义为单选：点击新目标即替换，再次点击取消
  const toggleScan = (r) => {
    if (!r.feasible) return
    scanSelected.value = scanSelected.value.includes(r.target) ? [] : [r.target]
  }

  const formatChain = (bytes) => (bytes ? `${(bytes / 1024).toFixed(1)} KB` : '—')

  const applyScan = () => {
    const picked = scanResults.value.filter((r) => scanSelected.value.includes(r.target))
    if (picked.length === 0) {
      toast.error('请先勾选一个可用目标')
      return
    }
    // serverNames 取该目标证书的 SAN 全集（节点回执 server_names，已过滤通配符）；
    // 跨 target 混填会把「不属于 dest 证书」的名字塞进白名单，导致 REALITY 握手失败
    const first = picked[0]
    const names = Array.isArray(first.server_names) ? first.server_names.filter(Boolean) : []
    if (names.length === 0) {
      toast.error('该目标证书没有可用的 serverNames')
      return
    }
    form.value.server_names = names.join(', ')
    form.value.dest = first.target
    scanOpen.value = false
    toast.success(`已填入 ${names.length} 个 serverNames（dest = ${first.target}）`)
  }

  return {
    scanOpen,
    scanInput,
    scanning,
    scanResults,
    scanSelected,
    scanError,
    fastestTarget,
    openScan,
    runScan,
    toggleScan,
    formatChain,
    applyScan,
  }
}
