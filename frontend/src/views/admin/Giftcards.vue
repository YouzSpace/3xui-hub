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
import { ref, reactive, computed, onMounted } from 'vue'
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
// 面值排序：'' = 默认（按 id 倒序），asc / desc
const valueOrder = ref('')
const loading = ref(false)
const loadError = ref('')

// 文件夹（列表接口顺带返回，不用单独拉）——对齐优惠码分组
const folders = ref([])
// 展开状态 + 每个文件夹里已加载的码（{ rows, total, loading }）
const expanded = reactive({})
const folderCodes = reactive({})
const folderLoaded = reactive({})

const pageCount = computed(() => Math.max(1, Math.ceil(total.value / PER_PAGE)))
const statusLabel = (s) => STATUS_LABELS[s] || s

const load = async () => {
  loading.value = true
  loadError.value = ''
  try {
    const params = { page: page.value, per_page: PER_PAGE }
    if (filterStatus.value) params.status = filterStatus.value
    if (valueOrder.value) params.value_order = valueOrder.value
    const res = await adminApi.listGiftcards(params)
    list.value = Array.isArray(res?.list) ? res.list : []
    total.value = Number(res?.total || 0)
    folders.value = Array.isArray(res?.folders) ? res.folders : []
  } catch (e) {
    list.value = []
    total.value = 0
    folders.value = []
    loadError.value = e.message || '加载失败'
  } finally {
    loading.value = false
  }
  // 已展开的文件夹内容整体作废（筛选/排序/增删后可能已换归属）
  const openIds = Object.keys(expanded).filter((id) => expanded[id])
  for (const id of Object.keys(folderLoaded)) delete folderLoaded[id]
  for (const id of Object.keys(folderCodes)) delete folderCodes[id]
  await Promise.all(openIds.map((id) => loadFolderCodes(id)))
}

/* ── 面值排序（点列头循环 默认 → 升序 → 降序 → 默认）── */
const cycleValueOrder = () => {
  valueOrder.value = valueOrder.value === '' ? 'asc' : valueOrder.value === 'asc' ? 'desc' : ''
  page.value = 1
  load()
}
const valueOrderIcon = computed(() => {
  if (valueOrder.value === 'asc') return 'chevron-up'
  if (valueOrder.value === 'desc') return 'chevron-down'
  return 'chevron-down'
})

/* ── 文件夹内码的加载/展开 ── */
const loadFolderCodes = async (id) => {
  folderCodes[id] = { rows: [], total: 0, loading: true }
  try {
    const params = { folder: id, page: 1, per_page: 500 }
    if (filterStatus.value) params.status = filterStatus.value
    if (valueOrder.value) params.value_order = valueOrder.value
    const res = await adminApi.listGiftcards(params)
    folderCodes[id] = {
      rows: Array.isArray(res?.list) ? res.list : [],
      total: Number(res?.total || 0),
      loading: false,
    }
    folderLoaded[id] = true
  } catch (e) {
    folderCodes[id] = { rows: [], total: 0, loading: false }
    delete folderLoaded[id]
  }
}

const toggleFolder = async (f) => {
  expanded[f.id] = !expanded[f.id]
  if (expanded[f.id] && !folderLoaded[f.id]) await loadFolderCodes(f.id)
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

const blankForm = () => ({ count: 10, amount: '10', name: '', note: '', expiresAt: '', folderId: '', folderName: '' })
const createForm = ref(blankForm())

// 生成时归档用的文件夹下拉：已有文件夹 + 「未归档」
const folderOptions = computed(() => [
  { label: '未归档', value: '' },
  ...folders.value.map((f) => ({ label: f.name, value: f.id })),
])

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
    // 归档：选了已有文件夹传 folder_id；填了新名字传 folder_name（后端按名字找，不存在就建）
    if (createForm.value.folderName.trim()) payload.folder_name = createForm.value.folderName.trim()
    else if (createForm.value.folderId) payload.folder_id = Number(createForm.value.folderId)
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

/* ── 文件夹：新建 / 改名 / 删除 ── */
const folderOpen = ref(false)
const folderEditId = ref(null)
const folderName = ref('')
const folderSaving = ref(false)

const openFolderCreate = () => {
  folderEditId.value = null
  folderName.value = ''
  folderOpen.value = true
}
const openFolderRename = (f) => {
  folderEditId.value = f.id
  folderName.value = f.name
  folderOpen.value = true
}
const submitFolder = async () => {
  const name = folderName.value.trim()
  if (!name) {
    toast.error('请填写文件夹名')
    return
  }
  folderSaving.value = true
  try {
    if (folderEditId.value) {
      await adminApi.updateGiftcardFolder(folderEditId.value, { name })
      toast.success('已改名')
    } else {
      await adminApi.createGiftcardFolder({ name })
      toast.success('已新建文件夹')
    }
    folderOpen.value = false
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    folderSaving.value = false
  }
}

// 删除文件夹：码的去向二选一（与优惠码同一交互）
const folderDeleteOpen = ref(false)
const folderDeleteTarget = ref(null)
const folderDeleteWithCodes = ref(false)
const folderDeleting = ref(false)

const openFolderDelete = (f) => {
  folderDeleteTarget.value = f
  folderDeleteWithCodes.value = false
  folderDeleteOpen.value = true
}
const submitFolderDelete = async () => {
  const f = folderDeleteTarget.value
  if (!f) return
  folderDeleting.value = true
  try {
    const withCodes = folderDeleteWithCodes.value
    const data = await adminApi.deleteGiftcardFolder(f.id, withCodes)
    if (withCodes) {
      const n = data?.deleted_codes ?? 0
      toast.success(`已删除文件夹及其中 ${n} 个兑换码`)
      // 删掉的码要从勾选里摘掉，避免后续批量删除操作不存在的行
      const next = new Map(selected.value)
      for (const id of (folderCodes[f.id]?.rows || []).map((c) => c.id)) next.delete(id)
      selected.value = next
    } else {
      toast.success('已删除文件夹，其中兑换码已转为未归档')
    }
    delete expanded[f.id]
    delete folderCodes[f.id]
    delete folderLoaded[f.id]
    folderDeleteTarget.value = null
    folderDeleteOpen.value = false
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  } finally {
    folderDeleting.value = false
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
        <Button variant="secondary" size="sm" @click="openFolderCreate">
          <Icon name="plus" :size="14" /> 新建文件夹
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
      <!-- 文件夹行：点名字展开看里面的码；右侧可改名 / 删除 -->
      <div v-if="folders.length" class="gc-folders">
        <div v-for="f in folders" :key="f.id" class="gc-folder">
          <div class="gc-folder-row" :class="{ 'is-open': expanded[f.id] }">
            <button class="gc-folder-toggle" type="button" @click="toggleFolder(f)">
              <Icon :name="expanded[f.id] ? 'chevron-down' : 'chevron-right'" :size="16" />
              <span class="gc-folder-name">{{ f.name }}</span>
              <span class="gc-folder-count">{{ f.code_count }}</span>
            </button>
            <div class="gc-folder-actions">
              <button class="gc-icon-btn" title="改名" @click="openFolderRename(f)"><Icon name="edit" :size="16" /></button>
              <button class="gc-icon-btn gc-icon-btn--danger" title="删除文件夹" @click="openFolderDelete(f)"><Icon name="trash" :size="16" /></button>
            </div>
          </div>

          <div v-if="expanded[f.id]" class="gc-folder-body">
            <p v-if="folderCodes[f.id]?.loading" class="gc-folder-tip">加载中...</p>
            <p v-else-if="!folderCodes[f.id]?.rows.length" class="gc-folder-tip">
              {{ filterStatus ? '当前筛选条件下该文件夹没有匹配的码' : '该文件夹还没有兑换码' }}
            </p>
            <table v-else class="admin-table gc-table--folder">
              <thead>
                <tr>
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
                <tr v-for="g in folderCodes[f.id].rows" :key="g.id">
                  <td class="gc-code">{{ g.code }}</td>
                  <td>{{ g.name || '—' }}</td>
                  <td class="gc-num">{{ formatYuan(g.value) }}</td>
                  <td><span class="gc-status" :class="`gc-status--${g.status}`">{{ statusLabel(g.status) }}</span></td>
                  <td class="gc-dim">{{ g.used_by ? `#${g.used_by}` : '—' }}</td>
                  <td class="gc-dim">{{ g.used_at || '—' }}</td>
                  <td class="gc-dim">{{ g.expires_at || '永久' }}</td>
                  <td class="gc-note" :title="g.note || ''">{{ g.note || '—' }}</td>
                  <td class="gc-dim">{{ g.created_at || '—' }}</td>
                  <td>
                    <div class="gc-actions">
                      <button class="gc-icon-btn gc-icon-btn--danger" :disabled="pendingId === g.id" title="删除（彻底移除，不可恢复）" @click="remove(g)">
                        <Icon name="trash" :size="16" />
                      </button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      <div v-if="folders.length" class="gc-flat-title">未归档</div>
      <div class="admin-table-wrap">
        <table class="admin-table">
          <thead>
            <tr>
              <th class="gc-check-cell"><input type="checkbox" :checked="pageAllSelected" :disabled="!list.length" @change="togglePageSelection" /></th>
              <th>兑换码</th>
              <th>名称</th>
              <th class="gc-sortable" :title="`点击切换面值排序（当前：${valueOrder === 'asc' ? '升序' : valueOrder === 'desc' ? '降序' : '默认'}）`" @click="cycleValueOrder">
                面值<Icon :name="valueOrderIcon" :size="14" class="gc-sort-icon" :class="{ 'is-active': valueOrder }" />
              </th>
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
          <span class="gc-label">归档到文件夹（可选）</span>
          <Select v-model="createForm.folderId" :options="folderOptions" />
        </label>
        <label class="gc-field">
          <span class="gc-label">或新建文件夹（填了就用这个名字）</span>
          <Input v-model="createForm.folderName" placeholder="如：双十一活动（留空则用上面的选择）" maxlength="60" />
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

    <!-- 文件夹新建 / 改名 -->
    <Modal v-model="folderOpen" :title="folderEditId ? '文件夹改名' : '新建文件夹'" width="420px">
      <div class="gc-form">
        <label class="gc-field">
          <span class="gc-label">文件夹名（最多 60 字）</span>
          <Input v-model="folderName" placeholder="如：双十一活动" maxlength="60" @keyup.enter="submitFolder" />
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="folderOpen = false">取消</Button>
        <Button variant="primary" size="sm" :loading="folderSaving" @click="submitFolder">
          {{ folderSaving ? '保存中…' : '保存' }}
        </Button>
      </template>
    </Modal>

    <!-- 删除文件夹：码的去向二选一 -->
    <Modal v-model="folderDeleteOpen" title="删除文件夹" width="460px">
      <div v-if="folderDeleteTarget" class="gc-form">
        <p class="gc-delete-tip">
          确定删除文件夹「{{ folderDeleteTarget.name }}」？里面有
          <strong>{{ folderDeleteTarget.code_count }}</strong> 个兑换码。
        </p>
        <label class="gc-radio">
          <input v-model="folderDeleteWithCodes" type="radio" :value="false" />
          <span>
            只删文件夹，码退回「未归档」
            <em>（默认，码一条都不删）</em>
          </span>
        </label>
        <label class="gc-radio" :class="{ 'is-danger': folderDeleteWithCodes }">
          <input v-model="folderDeleteWithCodes" type="radio" :value="true" :disabled="!folderDeleteTarget.code_count" />
          <span>
            连同文件夹内的 {{ folderDeleteTarget.code_count }} 个码一起删除
            <em class="gc-danger-text">（不可恢复）</em>
          </span>
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="folderDeleteOpen = false">取消</Button>
        <Button
          :variant="folderDeleteWithCodes ? 'danger' : 'primary'"
          size="sm"
          :loading="folderDeleting"
          @click="submitFolderDelete"
        >
          {{ folderDeleting ? '删除中…' : (folderDeleteWithCodes ? '删除文件夹和码' : '删除文件夹') }}
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

/* 面值列排序：列头可点，有箭头提示当前方向 */
.gc-sortable {
  cursor: pointer;
  user-select: none;
  white-space: nowrap;
}
.gc-sortable:hover {
  color: var(--text-primary);
}
.gc-sort-icon {
  vertical-align: -2px;
  margin-left: 2px;
  opacity: 0.35;
}
.gc-sort-icon.is-active {
  opacity: 1;
  color: var(--accent);
}

/* 文件夹行（对齐优惠码分组） */
.gc-folders {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-bottom: 16px;
}
.gc-folder {
  border: 1px solid var(--border-subtle);
  border-radius: 12px;
  overflow: hidden;
}
.gc-folder-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 12px;
  background: var(--bg-elevated);
}
.gc-folder-row.is-open {
  border-bottom: 1px solid var(--border-subtle);
}
.gc-folder-toggle {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: 1;
  min-width: 0;
  border: none;
  background: transparent;
  color: var(--text-primary);
  font-size: 14px;
  font-weight: 600;
  cursor: pointer;
  text-align: left;
  padding: 0;
}
.gc-folder-name {
  max-width: 320px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.gc-folder-count {
  padding: 1px 8px;
  border-radius: 999px;
  font-size: 12px;
  font-weight: 600;
  background: var(--bg-hover);
  color: var(--text-muted);
}
.gc-folder-actions {
  display: flex;
  align-items: center;
  gap: 4px;
}
.gc-folder-body {
  padding: 4px 8px 8px;
}
.gc-folder-tip {
  margin: 12px 8px;
  font-size: 12px;
  color: var(--text-muted);
}
.gc-table--folder {
  min-width: 820px;
}
.gc-flat-title {
  font-size: 13px;
  font-weight: 700;
  color: var(--text-secondary);
  margin: 4px 0 8px;
}
.gc-delete-tip {
  margin: 0;
  font-size: 14px;
  color: var(--text-primary);
  line-height: 1.6;
}
.gc-radio {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  font-size: 13px;
  color: var(--text-secondary);
  cursor: pointer;
}
.gc-radio input {
  margin-top: 3px;
  accent-color: var(--accent);
}
.gc-radio em {
  font-style: normal;
  color: var(--text-muted);
  font-size: 12px;
}
.gc-radio.is-danger {
  color: var(--danger);
}
.gc-danger-text {
  color: var(--danger) !important;
}

@media (max-width: 720px) {
  .gc-folder-name {
    max-width: 130px;
  }
  .gc-folder-body {
    max-height: none;
    overflow-y: visible;
  }
  .gc-table--folder {
    min-width: 0;
  }
  .gc-note {
    max-width: 120px;
  }
}
</style>
