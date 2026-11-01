<script setup>
/**
 * XrayNodePicker.vue — xray 配置四页共用的节点选择器。
 *
 * - 加载全部 xray 节点（driver_type=xray）→ 下拉选择
 * - 选中项同步到 URL ?node=（刷新/分享链接不丢）
 * - 无 xray 节点时展示引导（去节点页新建）
 */
import { ref, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Select from '@/components/ui/Select.vue'
import Badge from '@/components/ui/Badge.vue'
import { adminApi } from '@/api/admin'

const props = defineProps({
  modelValue: { type: [Number, null], default: null },
})
const emit = defineEmits(['update:modelValue'])

const route = useRoute()
const router = useRouter()

const nodes = ref([])
const loading = ref(true)

const load = async () => {
  loading.value = true
  try {
    const res = await adminApi.listNodes({ all: 1 })
    const list = Array.isArray(res) ? res : []
    nodes.value = list.filter((n) => n.driver_type === 'xray')

    const q = Number(route.query.node || 0)
    const valid = nodes.value.some((n) => n.id === q) ? q : (nodes.value[0]?.id || null)
    if (valid !== props.modelValue) emit('update:modelValue', valid)
  } catch {
    nodes.value = []
  } finally {
    loading.value = false
  }
}

watch(
  () => props.modelValue,
  (v) => {
    if (v && Number(route.query.node) !== Number(v)) {
      router.replace({ query: { ...route.query, node: v } })
    }
  },
)

onMounted(load)

defineExpose({ reload: load })
</script>

<template>
  <div class="xnp">
    <span class="xnp-label">节点</span>
    <div class="xnp-select">
      <Select
        :model-value="modelValue"
        :options="nodes.map((n) => ({ label: `${n.name}（${n.host || '未填主机'}）`, value: n.id }))"
        :placeholder="loading ? '加载中…' : '选择 xray 节点'"
        :disabled="loading || nodes.length === 0"
        size="sm"
        @update:model-value="emit('update:modelValue', $event)"
      />
    </div>
    <Badge
      v-if="modelValue"
      :variant="nodes.find((n) => n.id === modelValue)?.status === 'online' ? 'success' : 'danger'"
      dot
    >
      {{ nodes.find((n) => n.id === modelValue)?.status === 'online' ? '在线' : '离线' }}
    </Badge>
    <Badge v-if="modelValue && nodes.find((n) => n.id === modelValue)?.ws_connected" variant="info">WS 实时</Badge>
    <span v-if="!loading && nodes.length === 0" class="xnp-empty">还没有 xray 节点，请先到「节点列表」新建并安装内核</span>
  </div>
</template>

<style scoped>
.xnp {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.xnp-label {
  font-size: var(--text-sm);
  color: var(--text-muted);
}
.xnp-select {
  min-width: 240px;
}
.xnp-empty {
  font-size: var(--text-xs);
  color: var(--text-muted);
}
</style>
