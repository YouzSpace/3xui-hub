<script setup>
/**
 * Admin · 余额兑换码。
 *
 * 后端：internal/handler/admin/balance.go（POST/GET /admin-api/giftcards、DELETE /admin-api/giftcards/:id/delete）
 * 口径见 docs/balance/implementation-plan.md §3.2.1–3.2.3：
 *  - 面值一律「分」：输入按元收，提交前用 yuanToCents 换成整数分
 *  - 单次批量 1–100 个；码体 XXXX-XXXX-XXXX-XXXX（后端生成，前端只展示/复制）
 *  - 只保留「删除」（物理删除，任意状态）；没有「作废」状态
 */
import { ref, computed, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { formatYuan, yuanToCents } from '@/utils/money'

const STATUS_LABELS = { active: '未使用', used: '已使用' }

const STATUS_OPTIONS = [
  { label: '全部状态', value: '' },
  { label: '未使用', value: 'active' },
  { label: '已使用', value: 'used' },
]

const PER_PAGE = 20
const MAX_COUNT = 100

const toast = useToast()

const list = ref([])
const total = ref(0)
const page = ref(1)
const filterStatus = ref('')
const loading = ref(false)
const loadError = ref('')

const pageCount = computed(() => Math.max(1, Math.ceil(total.value / PER_PAGE)))
const statusLabel = (s) => STATUS_LABELS[s] || s

const load = async () => {
  loading.value = true
  loadError.value = ''
  try {
    const params = { page: page.value, per_page: PER_PAGE }
    if (filterStatus.value) params.status = filterStatus.value
    const res = await adminApi.listGiftcards(params)
    list.value = Array.isArray(res?.list) ? res.list : []
    total.value = Number(res?.total || 0)
  } catch (e) {
    list.value = []
    total.value = 0
    loadError.value = e.message || '加载失败'
  } finally {
    loading.value = false
  }
}

/* ── 勾选（当前页，用于批量删除）── */
const selected = ref(new Map())
const batchRemoving = ref(false)
const selectedCount = computed(() => selected.value.size)
const clearSelection = () => { selected.value = new Map() }

const isSelected = (g) => selected.value.has(g.id)
const toggleSelect = (g) => {
  const next = new Map(selected.value)
  if (next.has(g.id)) next.delete(g.id)
  else next.set(g.id, g)
  selected.value = next
}

const pageAllSelected = computed(() => list.value.length > 0 && list.value.every((g) => selected.value.has(g.id)))
const togglePageSelection = () => {
  const next = new Map(selected.value)
  const shouldSelect = !pageAllSelected.value
  for (const g of list.value) {
    if (shouldSelect) next.set(g.id, g)
    else next.delete(g.id)
  }
  selected.value = next
}

const removeSelected = async () => {
  const rows = [...selected.value.values()]
  if (!rows.length) return
  const preview = rows.slice(0, 6).map((g) => g.code).join('、')
  const more = rows.length > 6 ? ` 等 ${rows.length} 个` : ''
  if (!confirm(`确定删除选中的 ${rows.length} 个兑换码？\n${preview}${more}\n删除后不可恢复。`)) return

  batchRemoving.value = true
  try {
    const ids = rows.map((g) => g.id)
    let done = 0
    let failed = 0
    for (let i = 0; i < ids.length; i += 500) {
      const data = await adminApi.batchDeleteGiftcards(ids.slice(i, i + 500))
      done += data?.deleted?.length || 0
      failed += data?.failed?.length || 0
    }
    if (failed) toast.error(`已删除 ${done} 个，${failed} 个失败`)
    else toast.success(`已删除 ${done} 个兑换码`)
    selected.value = new Map()
    if (list.value.length <= done && page.value > 1) page.value--
    await load()
  } catch (e) {
    toast.error(e.message || '批量删除失败')
  } finally {
    batchRemoving.value = false
  }
}

const applyFilter = () => {
  page.value = 1
  load()
}

const goPage = (delta) => {
  const next = page.value + delta
  if (next < 1 || next > pageCount.value) return
  page.value = next
  load()
}

/* ===== 生成 ===== */
const createOpen = ref(false)
const creating = ref(false)
// 生成成功后把本批码摊在弹窗里，方便一次复制走（关掉弹窗即清空）
const createdCodes = ref([])

const blankForm = () => ({ count: 10, amount: '10', name: '', note: '', expiresAt: '' })
const createForm = ref(blankForm())

const openCreate = () => {
  createForm.value = blankForm()
  createdCodes.value = []
  createOpen.value = true
}

const submitCreate = async () => {
  const value = yuanToCents(createForm.value.amount)
  if (value === null) {
    toast.error('面值格式不对，最多两位小数')
    return
  }
  if (value <= 0) {
    toast.error('面值必须大于 0')
    return
  }
  const count = Number(createForm.value.count)
  if (!Number.isInteger(count) || count < 1 || count > MAX_COUNT) {
    toast.error(`生成数量需在 1-${MAX_COUNT} 之间`)
    return
  }

  creating.value = true
  try {
    const payload = { count, value }
    if (createForm.value.name.trim()) payload.name = createForm.value.name.trim()
    if (createForm.value.note.trim()) payload.note = createForm.value.note.trim()
    // datetime-local 的值就是后端能解析的「YYYY-MM-DDTHH:MM(:SS)」
    if (createForm.value.expiresAt) payload.expires_at = createForm.value.expiresAt
    const res = await adminApi.createGiftcards(payload)
    createdCodes.value = Array.isArray(res?.codes) ? res.codes : []
    toast.success(`已生成 ${res?.count ?? createdCodes.value.length} 个兑换码`)
    page.value = 1
    await load()
  } catch (e) {
    toast.error(e.message || '生成失败')
  } finally {
    creating.value = false
  }
}

const pendingId = ref(null)

const remove = async (row) => {
  if (!confirm(`删除兑换码「${row.code}」（${formatYuan(row.value)}）？\n删除后不可恢复，已到账的余额不退回。`)) return
  pendingId.value = row.id
  try {
    await adminApi.deleteGiftcard(row.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  } finally {
    pendingId.value = null
  }
}

onMounted(load)
</script>

<template>
  <div class="gc">
    <div class="gc-head">
      <div>
        <h1 class="gc-title">兑换码</h1>
        <p class="gc-sub">批量生成余额兑换码；用户在我的-账户余额里输入兑换，余额可用于买套餐。</p>
      </div>
      <div class="gc-tools">
        <Select v-model="filterStatus" :options="STATUS_OPTIONS" @change="applyFilter" />
        <Button variant="secondary" size="sm" :loading="loading" @click="load">
          <Icon name="refresh" :size="14" /> 刷新
        </Button>
        <Button variant="primary" size="sm" @click="openCreate">
          <Icon name="plus" :size="14" /> 生成兑换码
        </Button>
      </div>
    </div>

    <!-- 批量操作条：勾选 > 0 才出现 -->
    <div v-if="selectedCount" class="gc-bulkbar">
      <span class="gc-bulkbar-info">已勾选 {{ selectedCount }} 个</span>
      <div class="gc-bulkbar-actions">
        <Button variant="secondary" size="sm" @click="clearSelection">取消全部</Button>
        <Button variant="danger" size="sm" :disabled="batchRemoving" @click="removeSelected">
          <Icon name="trash" :size="14" /> {{ batchRemoving ? '删除中…' : '删除选中' }}
        </Button>
      </div>
    </div>

    <Card>
      <div class="admin-table-wrap">
        <table class="admin-table">
          <thead>
            <tr>
              <th class="gc-check-cell"><input type="checkbox" :checked="pageAllSelected" :disabled="!list.length" @change="togglePageSelection" /></th>
              <th>兑换码</th>
              <th>名称</th>
              <th>面值</th>
              <th>状态</th>
              <th>使用人</th>
              <th>使用时间</th>
              <th>过期时间</th>
              <th>备注</th>
              <th>创建时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="g in list" :key="g.id">
              <td class="gc-check-cell"><input type="checkbox" :checked="isSelected(g)" @change="toggleSelect(g)" /></td>
              <td class="gc-code">{{ g.code }}</td>
              <td>{{ g.name || '—' }}</td>
              <td class="gc-num">{{ formatYuan(g.value) }}</td>
              <td>
                <span class="gc-status" :class="`gc-status--${g.status}`">{{ statusLabel(g.status) }}</span>
              </td>
              <td class="gc-dim">{{ g.used_by ? `#${g.used_by}` : '—' }}</td>
              <td class="gc-dim">{{ g.used_at || '—' }}</td>
              <td class="gc-dim">{{ g.expires_at || '永久' }}</td>
              <td class="gc-note" :title="g.note || ''">{{ g.note || '—' }}</td>
              <td class="gc-dim">{{ g.created_at || '—' }}</td>
              <td>
                <div class="gc-actions">
                  <button
                    class="gc-icon-btn gc-icon-btn--danger"
                    :disabled="pendingId === g.id"
                    title="删除（彻底移除，不可恢复）"
                    @click="remove(g)"
                  >
                    <Icon name="trash" :size="16" />
                  </button>
                </div>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="11" class="gc-empty">加载失败：{{ loadError }}</td>
            </tr>
            <tr v-else-if="!list.length && !loading">
              <td colspan="11" class="gc-empty">还没有兑换码，点右上角「生成兑换码」</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <div class="gc-pager">
      <span class="gc-pager-info">共 {{ total }} 个 · 第 {{ page }} / {{ pageCount }} 页</span>
      <div class="gc-pager-btns">
        <Button variant="secondary" size="sm" :disabled="page <= 1 || loading" @click="goPage(-1)">
          <Icon name="chevron-left" :size="14" /> 上一页
        </Button>
        <Button variant="secondary" size="sm" :disabled="page >= pageCount || loading" @click="goPage(1)">
          下一页 <Icon name="chevron-right" :size="14" />
        </Button>
      </div>
    </div>

    <Modal v-model="createOpen" title="生成余额兑换码" width="480px">
      <div class="gc-form">
        <label class="gc-field">
          <span class="gc-label">数量（1-{{ MAX_COUNT }}）</span>
          <Input v-model="createForm.count" type="number" />
        </label>
        <label class="gc-field">
          <span class="gc-label">面值（元）</span>
          <Input v-model="createForm.amount" placeholder="例如 10 = 每个码 10 元" />
        </label>
        <label class="gc-field">
          <span class="gc-label">名称（可选，便于区分批次）</span>
          <Input v-model="createForm.name" placeholder="例如 双十一活动" />
        </label>
        <label class="gc-field">
          <span class="gc-label">过期时间（可选，留空=永久）</span>
          <Input v-model="createForm.expiresAt" type="datetime-local" />
        </label>
        <label class="gc-field">
          <span class="gc-label">备注（可选）</span>
          <Input v-model="createForm.note" placeholder="仅后台可见" />
        </label>

        <div v-if="createdCodes.length" class="gc-created">
          <div class="gc-created-head">
            <span>本批生成的 {{ createdCodes.length }} 个码</span>
            <CopyButton :text="createdCodes.join('\n')" label="复制全部" />
          </div>
          <pre class="gc-created-list">{{ createdCodes.join('\n') }}</pre>
        </div>
      </div>

      <template #footer>
        <Button variant="secondary" size="sm" @click="createOpen = false">
          {{ createdCodes.length ? '关闭' : '取消' }}
        </Button>
        <Button variant="primary" size="sm" :loading="creating" @click="submitCreate">
          生成
        </Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.gc {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.gc-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}
.gc-title {
  margin: 0 0 4px;
  font-size: 20px;
  font-weight: 700;
  color: var(--text-primary);
}
.gc-sub {
  margin: 0;
  font-size: 13px;
  color: var(--text-secondary);
}
.gc-tools {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.gc-bulkbar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  padding: 8px 12px;
  margin: 0 0 10px;
  border: 1px solid var(--accent-muted, rgba(13, 148, 136, 0.24));
  border-radius: 10px;
  background: var(--card, #fff);
}
.gc-bulkbar-info {
  font-size: 12px;
  font-weight: 700;
  color: var(--accent, #0d9488);
}
.gc-bulkbar-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-left: auto;
  flex-wrap: wrap;
}
.gc-check-cell {
  width: 36px;
  text-align: center;
  padding-right: 0;
}
.gc-check-cell input[type='checkbox'] {
  width: 15px;
  height: 15px;
  cursor: pointer;
  accent-color: var(--accent);
}

.gc-code {
  font-family: var(--font-mono);
  font-weight: 700;
  letter-spacing: 0.03em;
  font-size: 13px;
  white-space: nowrap;
}
.gc-num {
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}
.gc-dim {
  color: var(--text-muted);
  font-size: 13px;
  white-space: nowrap;
}
.gc-status {
  padding: 2px 8px;
  border-radius: 999px;
  font-size: 11px;
  font-weight: 600;
  background: var(--bg-hover);
  color: var(--text-muted);
  white-space: nowrap;
}
.gc-status--active {
  background: var(--success-muted);
  color: var(--success);
}
.gc-status--used {
  background: var(--info-muted);
  color: var(--info);
}
.gc-actions {
  display: flex;
  align-items: center;
  gap: 4px;
}
.gc-icon-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 30px;
  height: 30px;
  border-radius: 8px;
  border: none;
  background: transparent;
  color: var(--text-muted);
  cursor: pointer;
  transition: all 0.2s;
}
.gc-icon-btn:hover {
  background: var(--bg-hover);
  color: var(--text-primary);
}
.gc-icon-btn:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}
.gc-icon-btn--danger:hover {
  background: var(--danger-muted);
  color: var(--danger);
}
.gc-note {
  max-width: 200px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text-secondary);
  font-size: 13px;
}
.gc-empty {
  text-align: center;
  color: var(--text-muted);
  padding: 24px 8px;
}

.gc-pager {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.gc-pager-info {
  font-size: 13px;
  color: var(--text-secondary);
}
.gc-pager-btns {
  display: flex;
  gap: 8px;
}

.gc-form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.gc-field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.gc-label {
  font-size: 13px;
  color: var(--text-secondary);
}

/* 生成结果：等宽字体一行一码，窄屏内滚动不破版 */
.gc-created {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px;
  border-radius: 12px;
  background: var(--bg-elevated);
  border: 1px solid var(--border-subtle);
}
.gc-created-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  font-size: 13px;
  font-weight: 600;
  color: var(--text-primary);
}
.gc-created-list {
  margin: 0;
  max-height: 220px;
  overflow: auto;
  font-family: var(--font-mono);
  font-size: 13px;
  line-height: 1.7;
  color: var(--text-secondary);
}

@media (max-width: 720px) {
  .gc-tools {
    width: 100%;
  }
  .gc-note {
    max-width: 120px;
  }
}
</style>
