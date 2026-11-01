<script setup>
/**
 * Admin 节点流量倍率：原始倍率（继承的节点用）+ 单节点覆盖。
 *
 * 口径：生效倍率 = 节点倍率 ?? 原始倍率。
 * 改原始倍率只影响「继承中」的节点 —— 手动指定过的节点永远不受影响，
 * 所以这里没有任何「批量应用到全部节点」的操作，那与原始倍率的概念冲突。
 */
import { ref, computed, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const nodes = ref([])
const originalInput = ref('1.0')
const savingOriginal = ref(false)
const savedOriginal = ref(false)
const showMultiplier = ref(false)
const savingToggle = ref(false)

const modalOpen = ref(false)
const savedNode = ref(false)
const editing = ref(null)
// 编辑弹窗里的倍率输入；空串 = 继承原始倍率
const editInput = ref('')

const originalMultiplier = computed(() => {
  const parsed = parseFloat(originalInput.value)
  return Number.isFinite(parsed) ? parsed : 1.0
})

const inheritedCount = computed(() => nodes.value.filter(n => n.traffic_multiplier == null).length)

const load = async () => {
  try {
    // 倍率要逐个设到每个节点，必须一次看到全部 —— 显式 all=1 拿全量（列表页才走分页）
    nodes.value = await adminApi.listNodes({ all: 1 })
    const anyNode = nodes.value[0]
    // original_multiplier / show_multiplier 每行都带，取任意一条即可
    // （没有任何节点时保持输入框与开关的现值不动）
    if (anyNode) {
      if (anyNode.original_multiplier != null) {
        originalInput.value = String(anyNode.original_multiplier)
      }
      showMultiplier.value = !!anyNode.show_multiplier
    }
  } catch (e) {
    toast.error('加载失败')
  }
}

const saveOriginal = async () => {
  const value = parseFloat(originalInput.value)
  if (!Number.isFinite(value) || value < 1 || value > 100) {
    toast.error('倍率需在 1 ~ 100 之间')
    return
  }
  savingOriginal.value = true
  try {
    await adminApi.setOriginalNodeMultiplier(value)
    toast.success('原始倍率已保存，仅影响继承中的节点')
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段）
    savedOriginal.value = true
    setTimeout(() => { savedOriginal.value = false }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    savingOriginal.value = false
  }
}

const toggleDisplay = async () => {
  const next = !showMultiplier.value
  savingToggle.value = true
  try {
    await adminApi.setNodeMultiplierDisplay(next)
    showMultiplier.value = next
    toast.success(next ? '用户端节点列表将展示倍率' : '用户端不再展示倍率')
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    savingToggle.value = false
  }
}

const openEdit = (n) => {
  editing.value = n
  editInput.value = n.traffic_multiplier == null ? '' : String(n.traffic_multiplier)
  modalOpen.value = true
}

const saveNode = async () => {
  const raw = editInput.value.trim()
  let payload
  if (raw === '') {
    // 清空 = 改回继承原始倍率
    payload = null
  } else {
    const value = parseFloat(raw)
    if (!Number.isFinite(value) || value < 1 || value > 100) {
      toast.error('倍率需在 1 ~ 100 之间，或留空改回继承')
      return
    }
    payload = value
  }

  try {
    await adminApi.updateNode(editing.value.id, { traffic_multiplier: payload })
    toast.success(payload === null ? '已改回继承原始倍率' : '已保存')
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    savedNode.value = true
    setTimeout(() => {
      savedNode.value = false
      modalOpen.value = false
    }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  }
}

const clearToInherit = async (n) => {
  if (!confirm(`将 ${n.name} 改回继承原始倍率？`)) return
  try {
    await adminApi.updateNode(n.id, { traffic_multiplier: null })
    toast.success('已改回继承原始倍率')
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  }
}

const statusVariant = (s) => ({ online: 'success', offline: 'danger', maintenance: 'warning' }[s] || 'info')

// 倍率列：继承的显示实际生效值 + 「继承」标记，手动设的显示数值 + 「单独设置」标记
const effective = (n) => (n.traffic_multiplier == null ? (n.original_multiplier ?? 1.0) : n.traffic_multiplier)
const isInherited = (n) => n.traffic_multiplier == null

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="an">
    <div class="an-head">
      <h1 class="an-title">流量倍率</h1>
    </div>

    <p class="an-hint">
      倍率只作用于<strong>本次新增</strong>的流量增量，不追溯已统计的历史流量。
      快照始终保存面板原始字节数，所以改倍率不会导致用户流量突然暴涨。
    </p>

    <Card>
      <div class="an-original">
        <label class="an-field an-original-field">
          <span>原始倍率</span>
          <Input v-model="originalInput" type="number" step="0.01" min="1" max="100" mono placeholder="1.0" />
        </label>
        <div class="an-original-side">
          <Button variant="primary" size="sm" :loading="savingOriginal" :success="savedOriginal" @click="saveOriginal">
            保存
          </Button>
        </div>
        <label class="an-field an-toggle">
          <span class="an-toggle-row">
            <input type="checkbox" :checked="showMultiplier" :disabled="savingToggle" @change="toggleDisplay" />
            <span>用户端展示节点倍率</span>
          </span>
          <span class="an-muted">关闭时用户端接口不返回该字段</span>
        </label>
      </div>
    </Card>

    <Card :padding="false" class="an-table-card">
      <div class="an-table-wrap">
        <table class="an-table">
          <thead>
            <tr>
              <th>节点</th>
              <th>状态</th>
              <th>倍率</th>
              <th>来源</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="n in nodes" :key="n.id">
              <td>{{ n.name }}</td>
              <td><Badge :variant="statusVariant(n.status)" dot>{{ n.status }}</Badge></td>
              <td class="mono">{{ effective(n) }}</td>
              <td>
                <Badge :variant="isInherited(n) ? 'info' : 'warning'">
                  {{ isInherited(n) ? '继承' : '单独设置' }}
                </Badge>
              </td>
              <td>
                <div class="an-actions">
                  <Button variant="ghost" size="sm" @click="openEdit(n)"><Icon name="edit" :size="16" /></Button>
                  <Button
                    v-if="!isInherited(n)"
                    variant="ghost"
                    size="sm"
                    title="改回继承原始倍率"
                    @click="clearToInherit(n)"
                  >
                    <Icon name="refresh" :size="16" />
                  </Button>
                </div>
              </td>
            </tr>
            <tr v-if="!nodes.length">
              <td colspan="5" class="an-empty">暂无节点</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Modal v-model="modalOpen" :title="`设置倍率 · ${editing?.name || ''}`" width="440px">
      <div class="an-form">
        <p class="an-muted">
          当前来源：<strong>{{ editing && isInherited(editing) ? '继承' : '单独设置' }}</strong>
          <template v-if="editing && isInherited(editing)">
            ，生效倍率 {{ effective(editing) }}
          </template>
        </p>
        <label class="an-field">
          <span>节点倍率（留空 = 改回继承原始倍率）</span>
          <Input v-model="editInput" type="number" step="0.01" min="1" max="100" mono placeholder="留空即继承" />
        </label>
        <p class="an-muted">
          手动设置后，该节点不再跟随原始倍率变化；倍率只影响以后新增的流量。
        </p>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="modalOpen = false">取消</Button>
        <Button variant="primary" size="sm" :success="savedNode" @click="saveNode">保存</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
/* 页面根容器限宽：宽屏下表格不再被拉满视口（AdminLayout 的 .al-content 无宽度约束，不冲突） */
.an {
  max-width: 1100px;
}
/* 表格卡片与上方配置卡片留出间距，避免两张卡边框贴死 */
.an-table-card {
  margin-top: var(--space-4);
}
.an-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-4);
}
.an-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.an-hint {
  margin: 0 0 var(--space-4);
  color: var(--text-secondary);
  font-size: var(--text-sm);
}
/* 配置卡片：输入框 / 保存按钮 / 开关同一行同一视觉轴 */
.an-original {
  display: flex;
  align-items: flex-start;
  gap: var(--space-5);
  flex-wrap: wrap;
}
/* 输入框固定 160px（Input 组件 width:100%，由本列宽决定） */
.an-original-field {
  width: 160px;
  flex: 0 0 auto;
}
.an-original-side {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-2);
  /* label(约21px)+gap(4px) 之后是 36px 的输入框，其中心约 43px；按钮 32px 高，
     下移 27px 让按钮中心(27+16=43px)与输入框中心对齐 */
  padding-top: 27px;
}
/* 说明文字限宽，避免把按钮列撑到卡片 1/3 宽 */
.an-original-side .an-muted {
  max-width: 240px;
}
.an-toggle {
  margin-left: auto;
  /* checkbox 中心对齐输入框中心：实测比输入框低 5px，下移 32px 对齐 */
  padding-top: 32px;
}
.an-toggle-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--text-primary);
}
.an-toggle-row input[type='checkbox'] {
  cursor: pointer;
}
.an-table-wrap {
  overflow-x: auto;
}
.an-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.an-table th,
.an-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  vertical-align: middle;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
/* 操作列右对齐（空态行保持居中） */
.an-table th:last-child,
.an-table td:last-child:not(.an-empty) {
  text-align: right;
}
/* 最后一行去掉下边框，消除表格底部的多余空行感 */
.an-table tbody tr:last-child td {
  border-bottom: none;
}
.an-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.an-table tbody tr:hover {
  background: var(--bg-hover);
}
.an-actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-1);
}
.an-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
.an-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.an-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.an-muted {
  color: var(--text-muted);
  font-size: var(--text-xs);
  margin: 0;
}
.mono {
  font-family: var(--font-mono);
}

@media (max-width: 768px) {
  .an-head {
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-2);
  }
  .an-original {
    align-items: flex-end;
    gap: var(--space-3);
  }
  /* 输入框占满剩余宽度，与保存按钮同一行 */
  .an-original-field {
    width: auto;
    flex: 1 1 auto;
    min-width: 0;
  }
  /* side 让位：按钮/说明直接参与外层 flex，说明可换行到下一行 */
  .an-original-side {
    display: contents;
    padding-top: 0;
  }
  .an-original-side .ch-btn {
    flex: 0 0 auto;
  }
  .an-original-side .an-muted {
    max-width: none;
    flex: 1 1 100%;
  }
  /* 开关左对齐，去掉桌面端的右推与顶部对齐补偿 */
  .an-toggle {
    flex: 1 1 100%;
    margin-left: 0;
    min-width: 0;
    padding-top: 0;
  }
}
</style>
