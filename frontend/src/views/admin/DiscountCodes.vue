<script setup>
/**
 * DiscountCodes — 管理员优惠码管理（生成 / 导出 / 文件夹分组 / 批量删除）。
 * 邀请机制与校验限流已拆到独立页 InviteSettings.vue。
 * 列表（码/来源/折扣/已用次数/有效期/状态/关联套餐/说明文字）+ 生成弹窗 + 编辑弹窗。
 *
 * 分组（prompts/discount-folders-20261023）：
 * - 列表上方是文件夹行，点开展开看里面的码，逐条可勾；文件夹行可全选/反选/取消
 * - 下方是未归档码平铺表（folder_id = null）
 * - 勾选按码 id 存在一个 Map 里，跨文件夹与平铺混勾，一起导出 CSV
 */
import { ref, reactive, computed, onMounted } from 'vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import Select from '@/components/ui/Select.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const SOURCE_LABELS = { invite: '邀请码', redeem: '兑换码', admin: '管理员码' }
const STATUS_LABELS = { active: '生效中', used_up: '已用完', expired: '已过期', refreshed: '已刷新' }

const list = ref([])
const total = ref(0)
const page = ref(1)
const perPage = 20
const filterSource = ref('')
const filterStatus = ref('')
const filterUser = ref('')
const loading = ref(false)

// 文件夹（GET 列表接口顺带返回，不用单独拉）
const folders = ref([])
// 展开状态 + 每个文件夹里已加载的码（{ rows, total, loading }）
const expanded = reactive({})
const folderCodes = reactive({})
// 该文件夹的内容是否已经按「当前筛选条件」拉过一次（空的文件夹也是加载过的）
const folderLoaded = reactive({})

// 已勾选的码：id -> 行数据。跨文件夹/平铺混勾，导出就靠它。
const selected = ref(new Map())
// 批量删除进行中（按钮 loading/禁用）
const batchRemoving = ref(false)

const plans = ref([])

const sourceOptions = [
  { label: '全部来源', value: '' },
  { label: '邀请码', value: 'invite' },
  { label: '兑换码', value: 'redeem' },
  { label: '管理员码', value: 'admin' },
]

const statusFilterOptions = [
  { label: '全部状态', value: '' },
  { label: '生效中', value: 'active' },
  { label: '已用完', value: 'used_up' },
  { label: '已过期', value: 'expired' },
  { label: '已刷新', value: 'refreshed' },
]

const statusOptions = [
  { label: '生效中', value: 'active' },
  { label: '已用完', value: 'used_up' },
  { label: '已过期', value: 'expired' },
  { label: '已刷新', value: 'refreshed' },
]

const planOptions = computed(() => [
  { label: '不限套餐', value: '' },
  ...plans.value.map((p) => ({ label: p.name, value: p.id })),
])

const folderOptions = computed(() => [
  { label: '未归档', value: '' },
  ...folders.value.map((f) => ({ label: f.name, value: f.id })),
])

const saving = ref(false)

// 生成弹窗
const createOpen = ref(false)
const createForm = ref(blankCreate())

// 编辑弹窗
const editOpen = ref(false)
const editTarget = ref(null)
const editForm = ref({})

// 文件夹弹窗：新建 / 改名共用一个，folderEditId 为 null 表示新建
const folderOpen = ref(false)
const folderEditId = ref(null)
const folderName = ref('')
const folderSaving = ref(false)

// 导出弹窗
const exportOpen = ref(false)
const exportName = ref('discount-codes.csv')


const pageCount = computed(() => Math.max(1, Math.ceil(total.value / perPage)))
const selectedCount = computed(() => selected.value.size)

function blankCreate() {
  return {
    discount: 0.9,
    max_uses: 1,
    expires_at: '',
    note: '',
    plan_id: '',
    count: 1,
    prefix: 'PRO',
    folder_id: '',
    folder_name: '',
  }
}

const sourceLabel = (s) => SOURCE_LABELS[s] || s
const statusLabel = (s) => STATUS_LABELS[s] || s
const discountLabel = (d) => `${(Number(d) * 10).toFixed(1)} 折`

// datetime-local <-> 后端可解析字符串
const toApiDate = (v) => {
  if (!v) return null
  const s = String(v).replace('T', ' ')
  return s.length === 16 ? `${s}:00` : s
}
const toInputDate = (v) => (v ? String(v).replace(' ', 'T').slice(0, 16) : '')
// <input type="date"> 只吃 YYYY-MM-DD；后端 date 规则也接受这个格式
const toInputDay = (v) => (v ? String(v).replace(' ', 'T').slice(0, 10) : '')

/** 当前筛选条件（source/status/user），列表与文件夹内码共用一套。 */
const filterParams = () => {
  const params = {}
  if (filterSource.value) params.source = filterSource.value
  if (filterStatus.value) params.status = filterStatus.value
  if (filterUser.value) params.user = filterUser.value
  return params
}

const load = async () => {
  loading.value = true
  try {
    const params = { page: page.value, per_page: perPage, ...filterParams() }
    const data = await adminApi.listDiscountCodes(params)
    list.value = data?.list || []
    total.value = data?.total || 0
    folders.value = data?.folders || []
  } catch (e) {
    list.value = []
    total.value = 0
    folders.value = []
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }

  // 筛选条件/增删改之后，之前拉过的文件夹内容都作废（可能整批码已经换了归属）；
  // 展开着的立刻重拉，收起的等下次展开再拉 —— 否则「全选」会拿旧数据勾到已删的码
  for (const id of Object.keys(folderLoaded)) delete folderLoaded[id]
  const openIds = Object.keys(expanded).filter((id) => expanded[id]).map(Number)
  await Promise.all(openIds.map((id) => loadFolderCodes(id)))

  refreshSelectedRows()
}

/**
 * 勾选里存的是「勾那一刻的行快照」，导出用的是快照。
 * 列表/文件夹重新加载后拿新数据覆盖一遍，避免编辑过折扣或码后导出还是旧值。
 */
const refreshSelectedRows = () => {
  if (!selected.value.size) return
  const next = new Map(selected.value)
  const candidates = [...list.value, ...Object.values(folderCodes).flatMap((f) => f.rows || [])]
  for (const row of candidates) {
    if (next.has(row.id)) next.set(row.id, row)
  }
  selected.value = next
}

/**
 * 把一个筛选条件下的码全部取回来（分页拉，单页上限 100）。
 * 全选文件夹 / 全选未归档都要「全部」，不能只拿当前页 —— 导出漏码比多拉几次请求糟得多。
 */
const fetchAllCodes = async (params) => {
  const size = 100
  const rows = []
  let pageNo = 1
  let all = 0

  // 安全阈：50 页 = 5000 条，正常优惠码量级远到不了，防止后端 total 抖动时无限翻页
  for (let guard = 0; guard < 50; guard++) {
    const data = await adminApi.listDiscountCodes({ ...params, page: pageNo, per_page: size })
    const chunk = data?.list || []
    all = data?.total || 0
    rows.push(...chunk)
    if (chunk.length < size || rows.length >= all) break
    pageNo++
  }

  return { rows, total: all }
}

const loadFolderCodes = async (id) => {
  folderCodes[id] = { rows: [], total: 0, loading: true }
  try {
    const { rows, total: count } = await fetchAllCodes({ folder: id, ...filterParams() })
    folderCodes[id] = { rows, total: count, loading: false }
    folderLoaded[id] = true
  } catch (e) {
    folderCodes[id] = { rows: [], total: 0, loading: false }
    delete folderLoaded[id]
    toast.error(e.message || '文件夹内容加载失败')
  }
}

const toggleFolder = async (f) => {
  if (expanded[f.id]) {
    expanded[f.id] = false
    return
  }
  expanded[f.id] = true
  if (!folderLoaded[f.id]) await loadFolderCodes(f.id)
}

const loadPlans = async () => {
  // 套餐选择器要一次看到全部套餐，显式 all=1（列表页才走分页）
  try { plans.value = await adminApi.listPlans({ all: 1 }) } catch (e) { plans.value = [] }
}

const applyFilter = () => { page.value = 1; load() }

const goPage = (delta) => {
  const next = page.value + delta
  if (next < 1 || next > pageCount.value) return
  page.value = next
  load()
}

/* ── 勾选（跨文件夹与平铺混勾） ───────────────────────────── */

const isSelected = (c) => selected.value.has(c.id)

const toggleSelect = (c) => {
  // Map 是同一引用，set/delete 后要换一个新 Map 才能触发视图更新
  const next = new Map(selected.value)
  if (next.has(c.id)) next.delete(c.id)
  else next.set(c.id, c)
  selected.value = next
}

/** 一批码：allSelected=true 全勾，false 全取消，null 反选 */
const setSelection = (codes, allSelected) => {
  const next = new Map(selected.value)
  for (const c of codes) {
    const shouldSelect = allSelected === null ? !next.has(c.id) : allSelected
    if (shouldSelect) next.set(c.id, c)
    else next.delete(c.id)
  }
  selected.value = next
}

const clearSelection = () => { selected.value = new Map() }

/* issue #59：文件夹行的「全选/反选/取消」三个文字按钮收成一个勾选框（+ 已勾选 n/m），
   批量动作统一挪到页面上方的吸顶工具条 —— 按钮密度降下来，勾选作用域也能一眼看见。 */

const folderSelectedCount = (f) => {
  const rows = folderCodes[f.id]?.rows || []
  let n = 0
  for (const c of rows) if (selected.value.has(c.id)) n++
  return n
}

const folderAllSelected = (f) => {
  const rows = folderCodes[f.id]?.rows || []
  return rows.length > 0 && rows.every((c) => selected.value.has(c.id))
}

const folderPartlySelected = (f) => folderSelectedCount(f) > 0 && !folderAllSelected(f)

/** 文件夹行勾选框：已全选 → 取消；否则全选该文件夹（含未展开的，按当前筛选条件） */
const toggleFolderSelection = async (f) => {
  const rows = await ensureFolderRows(f.id)
  if (!rows.length) return
  setSelection(rows, !folderAllSelected(f))
}

/**
 * 取文件夹内全部码；还没按当前筛选条件拉过就现拉。
 * 刻意**不自动展开**：改勾选不把文件夹铺开（铺开会把下面的平铺表挤走，用户还得手动收起）。
 */
const ensureFolderRows = async (id) => {
  if (!folderLoaded[id]) {
    await loadFolderCodes(id)
  } else if (folderCodes[id]?.loading) {
    // 正在加载：等这一轮结束（轮询到 loading 结束，最多 ~3s）
    for (let i = 0; i < 30 && folderCodes[id]?.loading; i++) {
      await new Promise((r) => setTimeout(r, 100))
    }
  }
  return folderCodes[id]?.rows || []
}

/**
 * 批量删除：删的是勾选里的全部码（跨文件夹 + 平铺混勾）。
 * 用快照里的 code 做确认清单，避免只报数量让人误删。
 */
const removeSelected = async () => {
  const rows = [...selected.value.values()]
  if (!rows.length) return
  const preview = rows.slice(0, 6).map((c) => c.code).join('、')
  const more = rows.length > 6 ? ` 等 ${rows.length} 个` : ''
  if (!confirm(`确定删除选中的 ${rows.length} 个优惠码？\n${preview}${more}\n删除后不可恢复。`)) return

  batchRemoving.value = true
  try {
    // 后端单次上限 500（与生成上限一致）；勾选可能跨文件夹超过 500，这里分批提交
    const ids = rows.map((c) => c.id)
    let done = 0
    let failed = 0
    for (let i = 0; i < ids.length; i += 500) {
      const data = await adminApi.batchDeleteDiscountCodes(ids.slice(i, i + 500))
      done += data?.deleted?.length || 0
      failed += data?.failed?.length || 0
    }
    if (failed) toast.error(`已删除 ${done} 个，${failed} 个失败（可能已被别处删除）`)
    else toast.success(`已删除 ${done} 个优惠码`)
    selected.value = new Map()
    // 当前页删光了就回退一页，别停在空白页
    if (list.value.length <= done && page.value > 1) page.value--
    await load()
  } catch (e) {
    toast.error(e.message || '批量删除失败')
  } finally {
    batchRemoving.value = false
  }
}

/** 平铺表：表头勾选框作用于当前页 */
const pageAllSelected = computed(() => list.value.length > 0 && list.value.every((c) => selected.value.has(c.id)))
const togglePageSelection = () => setSelection(list.value, !pageAllSelected.value)

/* ── 文件夹 CRUD（弹窗，不跳独立设置页） ──────────────────── */

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
  if (!name) { toast.error('请填写文件夹名'); return }
  if (name.length > 60) { toast.error('文件夹名不能超过 60 个字'); return }
  folderSaving.value = true
  try {
    if (folderEditId.value) {
      await adminApi.updateDiscountCodeFolder(folderEditId.value, { name })
      toast.success('已改名')
    } else {
      await adminApi.createDiscountCodeFolder({ name })
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

/* ── 删除文件夹（issue #59：可选连码一起删） ──────────────── */

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
    const data = await adminApi.deleteDiscountCodeFolder(f.id, withCodes)
    toast.success(withCodes
      ? `已删除文件夹及其中 ${data?.deleted_codes ?? 0} 个优惠码`
      : '已删除文件夹，其中优惠码已转为未归档')

    // 已删掉的码不能继续留在勾选里（否则导出/批量删除会打到不存在的 id）
    if (withCodes) {
      const known = new Set((folderCodes[f.id]?.rows || []).map((c) => c.id))
      const next = new Map()
      for (const [id, row] of selected.value) {
        if (known.has(id)) continue
        // 没展开过（拿不到 id 清单）时按归属判断，宁多清也不留脏快照
        if (known.size === 0 && row?.folder_id === f.id) continue
        next.set(id, row)
      }
      selected.value = next
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

/* ── 生成 / 编辑 / 删除 ───────────────────────────────────── */

const openCreate = () => {
  createForm.value = blankCreate()
  createOpen.value = true
}

const submitCreate = async () => {
  const f = createForm.value
  if (!(f.discount > 0 && f.discount <= 1)) { toast.error('折扣需在 0.01 ~ 1 之间'); return }
  // 留空 = 用默认 PRO（与后端一致）：清空输入框不该变成一条「前缀非法」的报错
  const prefix = String(f.prefix || '').trim().toUpperCase() || 'PRO'
  if (!/^[A-Z0-9]{1,10}$/.test(prefix)) { toast.error('前缀只能是 1~10 位字母或数字'); return }
  saving.value = true
  try {
    const data = await adminApi.createDiscountCodes({
      discount: Number(f.discount),
      max_uses: Number(f.max_uses) || 1,
      expires_at: toApiDate(f.expires_at),
      note: f.note || null,
      plan_id: f.plan_id ? Number(f.plan_id) : null,
      count: Number(f.count) || 1,
      prefix,
      // 新文件夹名优先（后端规则：给了 folder_id 才用 id，否则按名字找/建）
      folder_id: f.folder_name.trim() ? null : (f.folder_id ? Number(f.folder_id) : null),
      folder_name: f.folder_name.trim() || null,
    })
    // 归档目标的显示名：现场新建的用输入名，选已有文件夹的从列表里取
    const targetName = f.folder_name.trim()
      || (f.folder_id ? (folders.value.find((x) => String(x.id) === String(f.folder_id))?.name || '') : '')
    toast.success(`已生成 ${data?.count ?? 1} 个优惠码${targetName ? `（已归档到「${targetName}」）` : ''}`)
    createOpen.value = false
    page.value = 1
    await load()
    // 归档到文件夹时自动展开它：列表默认只出「未归档」的码，
    // 不展开的话用户会因为下方列表空的而以为没生成成功。
    if (data?.folder_id) {
      expanded[data.folder_id] = true
      await loadFolderCodes(data.folder_id)
    }
  } catch (e) {
    toast.error(e.message || '生成失败')
  } finally {
    saving.value = false
  }
}

const openEdit = (c) => {
  editTarget.value = c
  editForm.value = {
    discount: Number(c.discount),
    max_uses: c.max_uses,
    max_uses_per_user: c.max_uses_per_user ?? 1,
    expires_at: toInputDay(c.expires_at),
    note: c.note || '',
    status: c.status,
    plan_id: c.plan_id || '',
  }
  editOpen.value = true
}

const submitEdit = async () => {
  const f = editForm.value
  if (!(f.discount > 0 && f.discount <= 1)) { toast.error('折扣需在 0.01 ~ 1 之间'); return }
  // 前后端双重把关：后端也会拒（防止并发下 used_count 已变），这里先给即时提示
  if (Number(f.max_uses) < (editTarget.value?.used_count || 0)) {
    toast.error(`总次数上限不能小于已使用次数（当前已用 ${editTarget.value.used_count} 次）`)
    return
  }
  saving.value = true
  try {
    await adminApi.updateDiscountCode(editTarget.value.id, {
      discount: Number(f.discount),
      max_uses: Number(f.max_uses) || 1,
      max_uses_per_user: Number(f.max_uses_per_user) || 1,
      expires_at: toApiDate(f.expires_at),
      note: f.note || null,
      status: f.status,
      plan_id: f.plan_id ? Number(f.plan_id) : null,
    })
    toast.success('已保存')
    editOpen.value = false
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const remove = async (c) => {
  // 物理删除、不可恢复，必须二次确认
  if (!confirm(`确定删除优惠码「${c.code}」？\n删除后不可恢复。`)) return
  try {
    await adminApi.deleteDiscountCode(c.id)
    // 撤销：码是随机生成的，没有精确恢复接口 → 给一次反悔提示，让用户重新生成
    toast.success(`已删除「${c.code}」`, 4000, async () => {
      toast.info('已撤销，请重新生成优惠码')
    })
    // 删掉的码不能留在勾选里，否则导出会带上一条已经不存在的记录
    if (selected.value.has(c.id)) {
      const next = new Map(selected.value)
      next.delete(c.id)
      selected.value = next
    }
    if (list.value.length === 1 && page.value > 1) page.value--
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

/* ── 导出 CSV（写法对齐 Dashboard.vue：BOM + RFC4180 转义 + Blob 下载） ── */

const CSV_HEADER = ['码', '折扣', '有效期', '已用上限', '说明']

/** 缺失值输出空字段；含逗号/引号/换行的按 RFC4180 转义。 */
const csvCell = (value) => {
  if (value === null || value === undefined || value === '') return ''
  const text = String(value)
  return /[",\n\r]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text
}

/** 导出勾选的码：按码排序，输出稳定（同一批勾选每次导出内容一致）。 */
const buildCsv = (rows) => {
  const lines = [CSV_HEADER.join(',')]
  const sorted = [...rows].sort((a, b) => String(a.code).localeCompare(String(b.code)))
  for (const c of sorted) {
    lines.push([
      c.code,
      Number(c.discount).toFixed(2),
      c.expires_at || '永久',
      c.max_uses,
      c.note,
    ].map(csvCell).join(','))
  }
  return lines.join('\n')
}

const dateStamp = (date = new Date()) => {
  const pad = (n) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

const openExport = () => {
  if (!selectedCount.value) { toast.error('请先勾选要导出的优惠码'); return }
  // 默认文件名带日期，但不是随机生成 —— 用户可以改成自己想要的
  exportName.value = `discount-codes-${dateStamp()}.csv`
  exportOpen.value = true
}

const submitExport = () => {
  // 只取文件名，去掉路径分隔符（下载属性里带路径会被浏览器忽略或截断）
  let name = String(exportName.value || '').trim().replace(/[\\/:*?"<>|]/g, '_')
  if (!name) name = `discount-codes-${dateStamp()}.csv`
  if (!/\.csv$/i.test(name)) name += '.csv'

  const content = buildCsv([...selected.value.values()])
  const blob = new Blob([`﻿${content}`], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = name
  document.body.appendChild(link)
  link.click()
  document.body.removeChild(link)
  URL.revokeObjectURL(url)

  exportOpen.value = false
  toast.success(`已导出 ${selectedCount.value} 个优惠码`)
}


onMounted(async () => {
  await Promise.all([load(), loadPlans()])
})
</script>

<template>
  <div class="dc">
    <!-- 标题 + 工具栏（卡片外，对齐余额码） -->
    <div class="dc-list-head">
      <h2 class="dc-title">优惠码</h2>
        <div class="dc-tools">
          <Select v-model="filterSource" :options="sourceOptions" @change="applyFilter" />
          <Select v-model="filterStatus" :options="statusFilterOptions" @change="applyFilter" />
          <!-- 按持有用户邮箱模糊搜；回车即搜 -->
          <input
            v-model.trim="filterUser"
            class="dc-search"
            type="text"
            placeholder="按用户邮箱搜索"
            maxlength="255"
            @keyup.enter="applyFilter"
          />
          <button class="dc-ghost-btn" @click="applyFilter">搜索</button>
          <button class="dc-ghost-btn" @click="load"><Icon name="refresh" :size="14" /> 刷新</button>
          <button class="dc-ghost-btn" @click="openFolderCreate"><Icon name="plus" :size="14" /> 新建文件夹</button>
          <button class="dc-add-btn" @click="openCreate"><Icon name="plus" :size="14" /> 生成优惠码</button>
        </div>
      </div>

      <!-- 批量操作条（issue #59）：勾选 > 0 才出现，sticky 常驻，滚动时不会被页面划走。
           之前「导出勾选/删除选中」只挂在「未归档」标题旁，勾了文件夹里的码就找不到入口。 -->
      <div v-if="selectedCount" class="dc-bulkbar">
        <span class="dc-bulkbar-info">已勾选 {{ selectedCount }} 个</span>
        <div class="dc-bulkbar-actions">
          <button class="dc-ghost-btn dc-ghost-btn--sm" @click="clearSelection">取消全部</button>
          <button class="dc-ghost-btn dc-ghost-btn--sm dc-export-btn" @click="openExport">
            <Icon name="inbox" :size="14" /> 导出勾选
          </button>
          <button
            class="dc-ghost-btn dc-ghost-btn--sm dc-danger-btn"
            :disabled="batchRemoving"
            @click="removeSelected"
          >
            <Icon name="trash" :size="14" /> {{ batchRemoving ? '删除中...' : '删除选中' }}
          </button>
        </div>
      </div>

    <!-- 文件夹 + 表格（卡片内） -->
    <section class="dc-card">
      <!-- 文件夹行：点名字展开看里面码；右侧勾选框全选该文件夹全部码（含未展开的） -->
      <div v-if="folders.length" class="dc-folders">
        <div v-for="f in folders" :key="f.id" class="dc-folder">
          <div class="dc-folder-row" :class="{ 'is-open': expanded[f.id] }">
            <button class="dc-folder-toggle" type="button" @click="toggleFolder(f)">
              <Icon :name="expanded[f.id] ? 'chevron-down' : 'chevron-right'" :size="16" />
              <span class="dc-folder-name">{{ f.name }}</span>
              <span class="dc-folder-count">{{ f.code_count }}</span>
              <!-- 搜索命中在文件夹里时提示：码不会出现在下方平铺表，要点开才看得见 -->
              <span v-if="f.matched" class="dc-folder-matched">搜索命中</span>
            </button>
            <div class="dc-folder-actions">
              <span v-if="folderSelectedCount(f)" class="dc-folder-picked">
                已勾选 {{ folderSelectedCount(f) }}/{{ f.code_count }}
              </span>
              <input
                type="checkbox"
                class="dc-folder-check"
                :checked="folderAllSelected(f)"
                :indeterminate.prop="folderPartlySelected(f)"
                :disabled="!f.code_count"
                :title="folderAllSelected(f) ? '取消勾选该文件夹的码' : '全选该文件夹的码（含未展开的）'"
                @change="toggleFolderSelection(f)"
              />
              <button class="dc-icon-btn" title="改名" @click="openFolderRename(f)"><Icon name="edit" :size="16" /></button>
              <button class="dc-icon-btn dc-icon-btn--danger" title="删除文件夹" @click="openFolderDelete(f)"><Icon name="trash" :size="16" /></button>
            </div>
          </div>

          <div v-if="expanded[f.id]" class="dc-folder-body">
            <p v-if="folderCodes[f.id]?.loading" class="dc-folder-tip">加载中...</p>
            <p v-else-if="!folderCodes[f.id]?.rows.length" class="dc-folder-tip">
              {{ f.matched || filterSource || filterStatus || filterUser ? '当前筛选条件下该文件夹没有匹配的码' : '该文件夹还没有优惠码' }}
            </p>
            <table v-else class="dc-table dc-table--folder">
              <thead>
                <tr>
                  <th class="dc-check-cell"></th>
                  <th>码</th>
                  <th class="dc-col-opt">来源</th>
                  <th class="dc-col-opt">持有用户</th>
                  <th>折扣</th>
                  <th>已用/上限</th>
                  <th>有效期</th>
                  <th>状态</th>
                  <th class="dc-col-opt">关联套餐</th>
                  <th class="dc-col-opt">说明文字</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="c in folderCodes[f.id].rows" :key="c.id">
                  <td class="dc-check-cell">
                    <input type="checkbox" :checked="isSelected(c)" @change="toggleSelect(c)" />
                  </td>
                  <td class="dc-code">{{ c.code }}</td>
                  <td class="dc-col-opt"><span class="dc-tag" :class="`dc-tag--${c.source}`">{{ sourceLabel(c.source) }}</span></td>
                  <td class="dc-owner dc-col-opt" :title="c.user_email || ''">{{ c.user_email || '—' }}</td>
                  <td class="dc-num">{{ discountLabel(c.discount) }}</td>
                  <td class="dc-num">{{ c.used_count }} / {{ c.max_uses >= 999999 ? '∞' : c.max_uses }}</td>
                  <td class="dc-dim">{{ c.expires_at || '永久' }}</td>
                  <td><span class="dc-status" :class="`dc-status--${c.status}`">{{ statusLabel(c.status) }}</span></td>
                  <td class="dc-dim dc-col-opt">{{ c.plan_name || '不限' }}</td>
                  <td class="dc-note dc-col-opt" :title="c.note || ''">{{ c.note || '—' }}</td>
                  <td>
                    <div class="dc-actions">
                      <button class="dc-icon-btn" title="编辑" @click="openEdit(c)"><Icon name="edit" :size="16" /></button>
                      <button class="dc-icon-btn dc-icon-btn--danger" title="删除" @click="remove(c)"><Icon name="trash" :size="16" /></button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      <!-- 未归档码平铺表（folder_id = null）；批量动作统一在上方吸顶工具条 -->
      <div class="dc-flat-head">
        <span class="dc-flat-title">未归档</span>
      </div>

      <div class="dc-table-wrap">
        <table class="dc-table">
          <thead>
            <tr>
              <th class="dc-check-cell">
                <input type="checkbox" :checked="pageAllSelected" :disabled="!list.length" @change="togglePageSelection" />
              </th>
              <th>码</th>
              <th class="dc-col-opt">来源</th>
              <th class="dc-col-opt">持有用户</th>
              <th>折扣</th>
              <th>已用/上限</th>
              <th>有效期</th>
              <th>状态</th>
              <th class="dc-col-opt">关联套餐</th>
              <th class="dc-col-opt">说明文字</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in list" :key="c.id">
              <td class="dc-check-cell">
                <input type="checkbox" :checked="isSelected(c)" @change="toggleSelect(c)" />
              </td>
              <td class="dc-code">{{ c.code }}</td>
              <td class="dc-col-opt"><span class="dc-tag" :class="`dc-tag--${c.source}`">{{ sourceLabel(c.source) }}</span></td>
              <!-- 管理员码 / 未被邀请人持有的码 user_id 为 null，显示占位符 -->
              <td class="dc-owner dc-col-opt" :title="c.user_email || ''">{{ c.user_email || '—' }}</td>
              <td class="dc-num">{{ discountLabel(c.discount) }}</td>
              <td class="dc-num">{{ c.used_count }} / {{ c.max_uses >= 999999 ? '∞' : c.max_uses }}</td>
              <td class="dc-dim">{{ c.expires_at || '永久' }}</td>
              <td><span class="dc-status" :class="`dc-status--${c.status}`">{{ statusLabel(c.status) }}</span></td>
              <td class="dc-dim dc-col-opt">{{ c.plan_name || '不限' }}</td>
              <td class="dc-note dc-col-opt" :title="c.note || ''">{{ c.note || '—' }}</td>
              <td>
                <div class="dc-actions">
                  <button class="dc-icon-btn" title="编辑" @click="openEdit(c)"><Icon name="edit" :size="16" /></button>
                  <button class="dc-icon-btn dc-icon-btn--danger" title="删除" @click="remove(c)"><Icon name="trash" :size="16" /></button>
                </div>
              </td>
            </tr>
            <tr v-if="!list.length && !loading">
              <td colspan="11" class="dc-empty">暂无未归档的优惠码</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="dc-pager">
        <span class="dc-pager-info">共 {{ total }} 条 · 第 {{ page }} / {{ pageCount }} 页</span>
        <div class="dc-pager-btns">
          <button class="dc-ghost-btn" :disabled="page <= 1" @click="goPage(-1)">上一页</button>
          <button class="dc-ghost-btn" :disabled="page >= pageCount" @click="goPage(1)">下一页</button>
        </div>
      </div>
    </section>

    <!-- 生成弹窗 -->
    <Modal v-model="createOpen" title="生成优惠码" width="480px">
      <div class="dc-form">
        <label class="dc-field">
          <span class="dc-label">折扣（0.90 = 9 折）</span>
          <input v-model.number="createForm.discount" type="number" step="0.05" min="0.01" max="1" class="dc-input" />
        </label>
        <label class="dc-field">
          <span class="dc-label">码前缀（1~10 位字母或数字，码 = 前缀 + 8 位随机）</span>
          <input v-model="createForm.prefix" class="dc-input dc-prefix-input" placeholder="PRO" maxlength="10" />
        </label>
        <label class="dc-field">
          <span class="dc-label">总使用次数（全站）</span>
          <input v-model.number="createForm.max_uses" type="number" min="1" class="dc-input" />
        </label>
        <label class="dc-field">
          <span class="dc-label">有效期（留空 = 永久）</span>
          <input v-model="createForm.expires_at" type="datetime-local" class="dc-input" />
        </label>
        <label class="dc-field">
          <span class="dc-label">绑定套餐（可选，留空 = 不限）</span>
          <Select v-model="createForm.plan_id" :options="planOptions" />
        </label>
        <label class="dc-field">
          <span class="dc-label">归档到文件夹（可选）</span>
          <Select v-model="createForm.folder_id" :options="folderOptions" />
        </label>
        <label class="dc-field">
          <span class="dc-label">或新建文件夹（填了就忽略上面的选择）</span>
          <input v-model="createForm.folder_name" class="dc-input" placeholder="如：双十一活动" maxlength="60" />
        </label>
        <label class="dc-field">
          <span class="dc-label">自定义说明文字（展示给用户）</span>
          <input v-model="createForm.note" class="dc-input" placeholder="如：双十一活动券" maxlength="255" />
        </label>
        <label class="dc-field">
          <span class="dc-label">生成数量</span>
          <input v-model.number="createForm.count" type="number" min="1" max="500" class="dc-input" />
        </label>
      </div>
      <template #footer>
        <div class="dc-foot">
          <button class="dc-ghost-btn" @click="createOpen = false">取消</button>
          <button class="dc-add-btn" :disabled="saving" @click="submitCreate">{{ saving ? '生成中...' : '生成' }}</button>
        </div>
      </template>
    </Modal>

    <!-- 编辑弹窗 -->
    <Modal v-model="editOpen" :title="editTarget ? `编辑 ${editTarget.code}` : '编辑优惠码'" width="480px">
      <div class="dc-form">
        <label class="dc-field">
          <span class="dc-label">折扣（0.90 = 9 折）</span>
          <input v-model.number="editForm.discount" type="number" step="0.05" min="0.01" max="1" class="dc-input" />
        </label>
        <label class="dc-field">
          <span class="dc-label">总使用次数（全站）</span>
          <input v-model.number="editForm.max_uses" type="number" min="1" class="dc-input" />
          <span class="dc-rate-tip">不能小于已使用次数（当前已用 {{ editTarget?.used_count ?? 0 }} 次）</span>
        </label>
        <label class="dc-field">
          <span class="dc-label">每用户可用次数</span>
          <input v-model.number="editForm.max_uses_per_user" type="number" min="1" class="dc-input" />
        </label>
        <label class="dc-field">
          <span class="dc-label">有效期（留空 = 永久）</span>
          <input v-model="editForm.expires_at" type="date" class="dc-input" />
        </label>
        <label class="dc-field">
          <span class="dc-label">绑定套餐（可选，留空 = 不限）</span>
          <Select v-model="editForm.plan_id" :options="planOptions" />
        </label>
        <label class="dc-field">
          <span class="dc-label">自定义说明文字（展示给用户）</span>
          <input v-model="editForm.note" class="dc-input" placeholder="如：双十一活动券" maxlength="255" />
        </label>
        <label class="dc-field">
          <span class="dc-label">状态</span>
          <Select v-model="editForm.status" :options="statusOptions" />
        </label>
      </div>
      <template #footer>
        <div class="dc-foot">
          <button class="dc-ghost-btn" @click="editOpen = false">取消</button>
          <button class="dc-add-btn" :disabled="saving" @click="submitEdit">{{ saving ? '保存中...' : '保存' }}</button>
        </div>
      </template>
    </Modal>

    <!-- 文件夹弹窗（新建 / 改名） -->
    <Modal v-model="folderOpen" :title="folderEditId ? '文件夹改名' : '新建文件夹'" width="420px">
      <div class="dc-form">
        <label class="dc-field">
          <span class="dc-label">文件夹名（最多 60 字）</span>
          <input v-model="folderName" class="dc-input" maxlength="60" placeholder="如：双十一活动" @keyup.enter="submitFolder" />
        </label>
      </div>
      <template #footer>
        <div class="dc-foot">
          <button class="dc-ghost-btn" @click="folderOpen = false">取消</button>
          <button class="dc-add-btn" :disabled="folderSaving" @click="submitFolder">{{ folderSaving ? '保存中...' : '保存' }}</button>
        </div>
      </template>
    </Modal>

    <!-- 删除文件夹：码的去向二选一（issue #59） -->
    <Modal v-model="folderDeleteOpen" title="删除文件夹" width="460px">
      <div v-if="folderDeleteTarget" class="dc-form">
        <p class="dc-delete-tip">
          确定删除文件夹「{{ folderDeleteTarget.name }}」？里面有
          <strong>{{ folderDeleteTarget.code_count }}</strong> 个优惠码。
        </p>
        <label class="dc-radio">
          <input v-model="folderDeleteWithCodes" type="radio" :value="false" />
          <span>
            只删文件夹，码退回「未归档」
            <em>（默认，码一条都不删）</em>
          </span>
        </label>
        <label class="dc-radio" :class="{ 'is-danger': folderDeleteWithCodes }">
          <input v-model="folderDeleteWithCodes" type="radio" :value="true" :disabled="!folderDeleteTarget.code_count" />
          <span>
            连同文件夹内的 {{ folderDeleteTarget.code_count }} 个码一起删除
            <em class="dc-danger-text">（不可恢复，已核销的记录会一起消失）</em>
          </span>
        </label>
      </div>
      <template #footer>
        <div class="dc-foot">
          <button class="dc-ghost-btn" @click="folderDeleteOpen = false">取消</button>
          <button
            class="dc-add-btn"
            :class="{ 'dc-add-btn--danger': folderDeleteWithCodes }"
            :disabled="folderDeleting"
            @click="submitFolderDelete"
          >
            {{ folderDeleting ? '删除中...' : (folderDeleteWithCodes ? '删除文件夹和码' : '删除文件夹') }}
          </button>
        </div>
      </template>
    </Modal>

    <!-- 导出弹窗：文件名自己填，不随机 -->
    <Modal v-model="exportOpen" title="导出优惠码 CSV" width="440px">
      <div class="dc-form">
        <label class="dc-field">
          <span class="dc-label">文件名</span>
          <input v-model="exportName" class="dc-input" maxlength="120" placeholder="discount-codes.csv" @keyup.enter="submitExport" />
          <span class="dc-rate-tip">共 {{ selectedCount }} 个码；列：码 / 折扣 / 有效期 / 已用上限 / 说明</span>
        </label>
      </div>
      <template #footer>
        <div class="dc-foot">
          <button class="dc-ghost-btn" @click="exportOpen = false">取消</button>
          <button class="dc-add-btn" @click="submitExport"><Icon name="inbox" :size="14" /> 导出</button>
        </div>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.dc { display: flex; flex-direction: column; gap: 20px; }

.dc-card {
  padding: 20px;
  border-radius: 16px;
  background: #fff;
  border: 1px solid rgba(0,0,0,0.05);
  box-shadow: 0 2px 12px -2px rgba(0,0,0,0.06);
}
.dc-card-head, .dc-list-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 16px;
}
.dc-card-title { margin: 0; font-size: 15px; font-weight: 700; color: #1d1d1f; }
.dc-title { margin: 0; font-size: 18px; font-weight: 800; color: #1d1d1f; }
.dc-tools { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
/* 工具栏里的下拉框：限宽，与搜索框并排一行（Select 组件默认 width:100%，在工具栏里会独占整行） */
.dc-tools :deep(.ch-select) { width: 132px; flex: 0 0 auto; }

.dc-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 14px;
}
.dc-field { display: flex; flex-direction: column; gap: 6px; }
.dc-label { font-size: 12px; font-weight: 600; color: #666; }
/* 限流字段的说明小字（字段内 + 卡片底部那行总说明共用） */
.dc-rate-tip { font-size: 11px; color: #999; line-height: 1.5; margin: 0; }
.dc-input {
  padding: 9px 12px;
  border: 1px solid rgba(0,0,0,0.08);
  border-radius: 10px;
  font-size: 14px;
  font-family: inherit;
  outline: none;
  background: #fff;
  color: #1d1d1f;
  transition: border-color 0.2s;
  width: 100%;
  box-sizing: border-box;
}
.dc-input:focus { border-color: var(--accent); }
/* 前缀固定显示大写，输入后即时反馈（后端也会再转一次） */
.dc-prefix-input { text-transform: uppercase; letter-spacing: 0.06em; font-weight: 600; }

/* 用户邮箱搜索框：与 .ch-select 同高，免得工具栏高低不齐 */
.dc-search {
  padding: 8px 14px;
  border: 1px solid rgba(0,0,0,0.08);
  border-radius: 999px;
  font-size: 13px;
  font-family: inherit;
  background: #fff;
  color: #1d1d1f;
  outline: none;
  width: 180px;
}
.dc-search:focus { border-color: var(--accent); }


/* 按钮 */
.dc-add-btn, .dc-ghost-btn, .dc-save-btn {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 8px 16px; border-radius: 999px; font-size: 13px; font-weight: 600;
  border: 1px solid rgba(0,0,0,0.08); background: #fff; color: #1d1d1f;
  cursor: pointer; transition: all 0.2s; font-family: inherit; white-space: nowrap;
}
.dc-ghost-btn--sm { padding: 4px 10px; font-size: 12px; }
.dc-add-btn { background: var(--accent); color: var(--text-inverse); border-color: transparent; }
.dc-add-btn:hover { background: var(--accent-hover); }
.dc-save-btn { background: var(--accent); color: var(--text-inverse); border-color: transparent; }
.dc-save-btn:hover { background: var(--accent-hover); }
.dc-ghost-btn:hover { background: #f5f5f5; }
.dc-add-btn:disabled, .dc-save-btn:disabled, .dc-ghost-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.dc-link-btn {
  border: none; background: transparent; color: var(--accent); cursor: pointer;
  font-size: 12px; font-family: inherit; padding: 0 2px;
}
.dc-link-btn:hover { text-decoration: underline; }

/* 危险动作按钮（批量删除）：hover 才变红，平时跟幽灵按钮同款 */
.dc-danger-btn:hover {
  background: var(--danger-muted, #fef2f2);
  border-color: rgba(220, 38, 38, 0.25);
  color: var(--danger, #dc2626);
}

/* 文件夹 */
.dc-folders { display: flex; flex-direction: column; gap: 8px; margin-bottom: 16px; }
.dc-folder {
  border: 1px solid rgba(0,0,0,0.06);
  border-radius: 12px;
  overflow: hidden;
  background: #fafafa;
}
.dc-folder-row {
  display: flex; align-items: center; justify-content: space-between;
  gap: 12px; flex-wrap: wrap; padding: 8px 12px;
}
.dc-folder-row.is-open { border-bottom: 1px solid rgba(0,0,0,0.06); }
.dc-folder-toggle {
  display: inline-flex; align-items: center; gap: 8px;
  border: none; background: transparent; cursor: pointer; padding: 4px 0;
  font-family: inherit; font-size: 13px; font-weight: 600; color: #1d1d1f;
}
.dc-folder-name { max-width: 320px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.dc-folder-count {
  padding: 1px 8px; border-radius: 999px; font-size: 11px; font-weight: 600;
  background: var(--bg-hover, #efefef); color: var(--text-muted, #666);
}
.dc-folder-matched {
  padding: 1px 8px; border-radius: 999px; font-size: 11px; font-weight: 600;
  background: var(--warning-muted, #fef3c7); color: var(--warning, #b45309);
}
.dc-folder-actions { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
/* issue #59：文件夹行只留一个勾选框 + 已勾选计数，按钮从 5 个降到 2 个图标 */
.dc-folder-check {
  width: 16px; height: 16px; cursor: pointer; accent-color: var(--accent, #0d9488);
  flex: 0 0 auto;
}
.dc-folder-check:disabled { cursor: not-allowed; opacity: 0.4; }
.dc-folder-picked {
  font-size: 11px; font-weight: 600; color: var(--accent, #0d9488);
  white-space: nowrap; font-variant-numeric: tabular-nums;
}
/* issue #59：文件夹内容内部滚动，展开多个也不会把整页拉成长蛇；
   60vh 的阈值保证一屏内还能看到下一个文件夹的标题行 */
.dc-folder-body {
  padding: 4px 8px 8px; background: #fff;
  max-height: 60vh; overflow-y: auto;
}
.dc-folder-tip { margin: 12px 8px; font-size: 12px; color: #999; }
.dc-table--folder { min-width: 820px; }

/* 批量操作条（issue #59）：勾选 > 0 时出现，吸附在顶栏下方常驻。
   top 值 = AdminLayout 顶栏高度（padding 12×2 + 内容 32 + 边框 1），
   z-index 低于顶栏（20），滚动时不会被顶栏盖住也不会盖住顶栏。 */
.dc-bulkbar {
  position: sticky;
  top: 57px;
  z-index: 10;
  display: flex; align-items: center; gap: 10px; flex-wrap: wrap;
  padding: 8px 12px; margin: 0 0 10px;
  border: 1px solid var(--accent-muted, rgba(13,148,136,0.24));
  border-radius: 10px;
  background: var(--card, #fff);
  box-shadow: 0 2px 10px -2px rgba(0,0,0,0.08);
}
.dc-bulkbar-info { font-size: 12px; font-weight: 700; color: var(--accent, #0d9488); }
.dc-bulkbar-actions { display: flex; align-items: center; gap: 8px; margin-left: auto; flex-wrap: wrap; }

/* 未归档平铺区标题条 */
.dc-flat-head {
  display: flex; align-items: center; gap: 10px; flex-wrap: wrap;
  margin: 4px 0 10px;
}
.dc-flat-title { font-size: 13px; font-weight: 700; color: #1d1d1f; }
.dc-selected-info { font-size: 12px; color: #666; display: inline-flex; align-items: center; gap: 4px; }
/* 右侧动作组：两个按钮必须贴在一起，只有一个 margin-left:auto（都写 auto 会把剩余空间平分，导出被挤到中间） */
.dc-flat-actions { display: flex; align-items: center; gap: 8px; margin-left: auto; }

/* 表格 */
.dc-table-wrap { overflow-x: auto; }
.dc-table { width: 100%; border-collapse: collapse; min-width: 900px; }
.dc-table th {
  height: 40px;
  padding: 8px 12px;
  text-align: left;
  font-size: 13px;
  font-weight: var(--font-medium);
  color: var(--text-secondary);
  background: var(--bg-elevated);
  border-bottom: 1px solid var(--border-strong);
  white-space: nowrap;
}
.dc-table td {
  padding: 8px 12px;
  font-size: 13px;
  color: var(--text-primary);
  border-bottom: 1px solid var(--border-subtle);
  vertical-align: middle;
}
.dc-table tbody tr:hover { background: var(--bg-hover); }
.dc-table tr:last-child td { border-bottom: none; }
/* 勾选列：窄，只放一个 checkbox */
.dc-check-cell { width: 36px; padding-right: 0; }
.dc-check-cell input[type="checkbox"] { width: 15px; height: 15px; cursor: pointer; accent-color: var(--accent); }
.dc-code {
  font-family: 'SF Mono', 'Fira Code', monospace; font-weight: 700;
  letter-spacing: 0.03em; white-space: nowrap;
}
.dc-owner { max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: #666; }
.dc-num { font-variant-numeric: tabular-nums; white-space: nowrap; }
.dc-dim { color: #999; font-size: 12px; white-space: nowrap; }
.dc-note { max-width: 180px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: #666; }
.dc-empty { text-align: center; color: #999; font-size: 13px; padding: 40px; }

/* 标签/状态一律走 tokens，浅色暗色自动跟随，不写死颜色 */
.dc-tag {
  padding: 2px 8px; border-radius: 999px; font-size: 11px; font-weight: 600;
  background: var(--bg-hover); color: var(--text-muted); white-space: nowrap;
}
.dc-tag--invite { background: var(--info-muted); color: var(--info); }
.dc-tag--redeem { background: var(--success-muted); color: var(--success); }
.dc-tag--admin { background: var(--warning-muted); color: var(--warning); }

.dc-status {
  padding: 2px 8px; border-radius: 999px; font-size: 11px; font-weight: 600;
  background: var(--bg-hover); color: var(--text-muted); white-space: nowrap;
}
.dc-status--active { background: var(--success-muted); color: var(--success); }
.dc-status--used_up { background: var(--bg-hover); color: var(--text-muted); }
.dc-status--expired { background: var(--danger-muted); color: var(--danger); }
.dc-status--refreshed { background: var(--info-muted); color: var(--info); }

.dc-actions { display: flex; align-items: center; gap: 4px; }
.dc-icon-btn {
  display: flex; align-items: center; justify-content: center;
  width: 30px; height: 30px; border-radius: 8px; border: none;
  background: transparent; color: #666; cursor: pointer; transition: all 0.2s;
}
.dc-icon-btn:hover { background: #f0f0f0; }
.dc-icon-btn--danger:hover { background: #fef2f2; color: #dc2626; }

.dc-pager {
  display: flex; align-items: center; justify-content: space-between;
  gap: 12px; flex-wrap: wrap; margin-top: 16px;
}
.dc-pager-info { font-size: 12px; color: #999; }
.dc-pager-btns { display: flex; gap: 8px; }

/* 弹窗表单 */
.dc-form { display: flex; flex-direction: column; gap: 14px; }
.dc-foot { display: flex; gap: 10px; justify-content: flex-end; width: 100%; }

/* 删除文件夹弹窗：码的去向二选一（issue #59） */
.dc-delete-tip { margin: 0; font-size: 13px; line-height: 1.7; color: #1d1d1f; }
.dc-radio {
  display: flex; align-items: flex-start; gap: 8px;
  padding: 9px 11px; font-size: 13px; line-height: 1.5; cursor: pointer;
  border: 1px solid rgba(0,0,0,0.08); border-radius: 10px;
}
.dc-radio input[type="radio"] { margin-top: 2px; flex: 0 0 auto; accent-color: var(--accent, #0d9488); }
.dc-radio.is-danger { border-color: var(--danger, #dc2626); background: var(--danger-muted, #fef2f2); }
.dc-radio em { font-style: normal; color: #999; }
.dc-danger-text { color: var(--danger, #dc2626); }
.dc-add-btn--danger { background: var(--danger, #dc2626); border-color: var(--danger, #dc2626); }

/* 窄屏（issue #59）：手机上原来整页 0 断点 —— 文件夹行 5 个按钮挤成两行、
   表格 10 列横滑很远、批量按钮在页面底部摸不到。 */
@media (max-width: 640px) {
  .dc-card { padding: 12px; border-radius: 12px; }
  .dc-list-head { margin-bottom: 12px; }
  .dc-tools { width: 100%; gap: 6px; }
  .dc-tools :deep(.ch-select) { width: calc(50% - 3px); }
  .dc-search { flex: 1 1 100%; }
  .dc-folder-name { max-width: 130px; }
  /* 手机上内滚与外滚会互相抢手势，文件夹内容交回整页滚动 */
  .dc-folder-body { max-height: none; overflow-y: visible; }
  /* 次要列收起：来源 / 持有用户 / 关联套餐 / 说明文字 */
  .dc-col-opt { display: none; }
  .dc-table, .dc-table--folder { min-width: 0; }
  .dc-table th, .dc-table td { padding: 8px 6px; font-size: 12px; }
  .dc-owner { max-width: 100px; }
  /* 批量操作条改吸底：拇指够得着，不会被页面划走 */
  .dc-bulkbar {
    position: fixed; left: 8px; right: 8px; top: auto;
    bottom: calc(8px + env(safe-area-inset-bottom, 0px));
    margin: 0; z-index: 60;
  }
  /* 吸底条固定后不会占文档流高度，给页面留出它的位置，避免盖住最后一行的操作按钮 */
  .dc:has(.dc-bulkbar) { padding-bottom: 76px; }
  .dc-foot { flex-wrap: wrap; }
  .dc-foot > * { flex: 1 1 auto; }
}

/* 暗色模式 */
html.dark .dc-card { background: rgba(20,20,20,0.8); border-color: rgba(255,255,255,0.06); }
html.dark .dc-card-title, html.dark .dc-title, html.dark .dc-flat-title, html.dark .dc-folder-toggle { color: #fff; }
html.dark .dc-label { color: #999; }
html.dark .dc-rate-tip { color: #777; }
html.dark .dc-input, html.dark .dc-search { background: rgba(255,255,255,0.04); border-color: rgba(255,255,255,0.08); color: #e5e5e5; }
html.dark .dc-ghost-btn { background: rgba(255,255,255,0.05); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
html.dark .dc-ghost-btn:hover { background: rgba(255,255,255,0.1); }
html.dark .dc-folder { background: rgba(255,255,255,0.03); border-color: rgba(255,255,255,0.08); }
html.dark .dc-folder-row.is-open { border-color: rgba(255,255,255,0.08); }
html.dark .dc-folder-body { background: transparent; }
html.dark .dc-selected-info { color: #aaa; }
html.dark .dc-owner, html.dark .dc-dim { color: #888; }
html.dark .dc-note { color: #aaa; }
html.dark .dc-icon-btn { color: #999; }
html.dark .dc-icon-btn:hover { background: rgba(255,255,255,0.08); }

/* 表单栅格对齐：标签统一占两行高度，长标签换行时不会把输入框挤下去 */
.dc-grid { align-items: start; }
.dc-grid > .dc-field > span:first-child { display: block; min-height: 2.6em; line-height: 1.35; }
</style>
