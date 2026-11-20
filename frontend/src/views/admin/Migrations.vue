<script setup>
/**
 * Admin · 迁移历史：谁在什么时候被从哪个节点搬到了哪个节点，以及按批次撤销。
 *
 * 数据来自 `GET /admin-api/migrations`（data = { items, total, page, per_page }），
 * 每行字段见 server/internal/handler/admin/migration_domain.go：
 *   { id, user_id, from_node_id, to_node_id, reason, operator, batch_id, created_at }
 *
 * 两点口径容易踩，这里写死：
 *  1. reason 只有三个值：manual（管理员手动改绑）/ balance（批量均衡）/ failover（故障迁移）。
 *  2. 撤销是**按批次**的，不是按行：同一 batch_id 的记录会一起反向回滚；
 *     且只有「24 小时内 + batch_id 非空」的记录可撤销（batch_id 为 NULL = 非接口产生，
 *     比如手动改绑，没有批次概念）。后端是最终裁判，这里只决定按钮是否可点。
 *
 * created_at 是后端 dateTimeStr 的 UTC 口径（"YYYY-MM-DD HH:mm:ss"，不带时区标记），
 * 直接交给 new Date() 会被当成本地时间，+08:00 机器上整整差 8 小时 —— 撤销窗口会算错，
 * 所以统一按 UTC 解析（见 parseServerTime）。
 */
import { ref, computed, watch, onMounted, onActivated, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Pagination from '@/components/ui/Pagination.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { useConfirm } from '@/composables/useConfirm'
import { usePagedList } from '@/composables/usePagedList'

const route = useRoute()
const router = useRouter()
const toast = useToast()
const { confirm } = useConfirm()

// 筛选初值取自 URL：刷新 / 分享链接回来条件还在
const userFilter = ref(String(route.query.user_id ?? ''))
const nodeFilter = ref(String(route.query.node_id ?? ''))
const reasonFilter = ref(String(route.query.reason ?? ''))

// 后端 reason 枚举（migration_domain.go / scheduler/move.go），值与文案一一对应
const reasonOptions = [
  { label: '全部原因', value: '' },
  { label: '手动改绑', value: 'manual' },
  { label: '批量均衡', value: 'balance' },
  { label: '故障迁移', value: 'failover' },
]
const reasonText = (r) =>
  ({ manual: '手动改绑', balance: '批量均衡', failover: '故障迁移' }[r] || r || '—')
const reasonVariant = (r) => ({ manual: 'info', balance: 'success', failover: 'warning' }[r] || 'info')

const { page, loading, loadError, pagination, prev, next, fetchPage } = usePagedList()
const rows = ref([])

/**
 * 后端这个接口的分页元信息在 data 里（items/total/page/per_page），
 * 与 usePagedList 认的 `res.pagination` 不同 —— 在这里抹平成它要的形状，
 * 页码 / 越界夹回 / 刷新不丢页这些能力都还能复用。
 */
const fetchMigrations = async (params, config) => {
  const res = await adminApi.listMigrationsPage(params, config)
  const items = Array.isArray(res?.items) ? res.items : []
  const total = Number(res?.total) || 0
  const perPage = Number(res?.per_page) || 20
  return Object.assign(items, {
    pagination: {
      current_page: Number(res?.page) || 1,
      last_page: Math.max(1, Math.ceil(total / perPage)),
      total,
      per_page: perPage,
    },
  })
}

/** 当前筛选条件（空值不带，避免后端把空串当成精确匹配） */
const extraParams = () => {
  const extra = {}
  const uid = userFilter.value.trim()
  const nid = nodeFilter.value.trim()
  if (uid) extra.user_id = uid
  if (nid) extra.node_id = nid
  if (reasonFilter.value) extra.reason = reasonFilter.value
  return extra
}

const load = async () => {
  const extra = extraParams()
  const list = await fetchPage(
    (params, config) => fetchMigrations({ ...params, ...extra }, config),
    Object.keys(extra).length ? extra : null,
  )
  if (!list) return
  rows.value = list
}

/** 改筛选：条件进 URL，页码回 1（已在第 1 页就直接重拉） */
const applyFilters = () => {
  const next = { ...route.query }
  delete next.page
  const put = (key, value) => {
    if (value) next[key] = value
    else delete next[key]
  }
  put('user_id', userFilter.value.trim())
  put('node_id', nodeFilter.value.trim())
  put('reason', reasonFilter.value)

  if (page.value !== 1) {
    router.push({ query: next })
    return
  }
  router.replace({ query: next })
  load()
}

// 手动输入的两个筛选做防抖，避免每敲一位就打一次接口；下拉是点选，立即生效
let filterTimer = null
const applyFiltersDebounced = () => {
  clearTimeout(filterTimer)
  filterTimer = setTimeout(applyFilters, 350)
}
watch([userFilter, nodeFilter], applyFiltersDebounced)
watch(reasonFilter, applyFilters)
watch(page, load)
onUnmounted(() => clearTimeout(filterTimer))

// ===== 节点名：接口只给 id，尽量换成名字；拿不到就退回 #id，不影响历史本身 =====
const nodeNames = ref({})
const loadNodeNames = async () => {
  try {
    const list = await adminApi.listNodes({ all: 1 })
    const map = {}
    for (const n of Array.isArray(list) ? list : []) {
      if (n && n.id != null) map[n.id] = n.name
    }
    nodeNames.value = map
  } catch {
    // 节点接口不可用（或后端未合入）：只显示 #id，不弹错误打扰管理员
  }
}
const nodeLabel = (id) => (id == null ? '' : nodeNames.value[id] || `#${id}`)

// ===== 时间与撤销窗口 =====
const UNDO_WINDOW_MS = 24 * 60 * 60 * 1000

/** 后端 UTC 时间串按 UTC 解析；已带时区（ISO）的原样交给 Date */
const parseServerTime = (v) => {
  if (!v) return null
  const s = String(v)
  const d = /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(s)
    ? new Date(`${s.replace(' ', 'T')}Z`)
    : new Date(s)
  return Number.isNaN(d.getTime()) ? null : d
}
const fmtTime = (v) => {
  const d = parseServerTime(v)
  return d ? d.toLocaleString() : '—'
}

/** 只有 24 小时内且带批次号的记录能撤销（batch_id 为 NULL = 非接口产生） */
const canUndo = (row) => {
  if (!row?.batch_id) return false
  const at = parseServerTime(row.created_at)
  return !!at && Date.now() - at.getTime() <= UNDO_WINDOW_MS
}

const undoBusy = ref('')

/**
 * 撤销整个批次（不是这一行）：确认文案必须说清楚，
 * 否则管理员以为只回滚当前用户，实际整批都被搬回去了。
 */
const undo = async (row) => {
  if (!row?.batch_id || undoBusy.value) return
  const ok = await confirm({
    title: '撤销迁移批次',
    message:
      `将把批次 ${row.batch_id} 的全部迁移记录一起反向回滚，` +
      '相关用户会被搬回来源节点（首次分配的记录则解绑）。\n' +
      '只允许撤销 24 小时内的批次，确定继续？',
    confirmText: '撤销批次',
    danger: true,
  })
  if (!ok) return

  undoBusy.value = row.batch_id
  try {
    const res = await adminApi.undoMigration(row.batch_id)
    const undone = Number(res?.undone) || 0
    toast.success(`已撤销 ${undone} 条迁移记录`)
    await load()
  } catch (e) {
    toast.error(e?.message || '撤销失败')
  } finally {
    undoBusy.value = ''
  }
}

const totalText = computed(() => pagination.value.total ?? 0)

onMounted(() => {
  load()
  loadNodeNames()
})
// keep-alive 回来时刷新：用户可能刚在「地区与调度」里迁过人，历史要跟着变
onActivated(load)
</script>

<template>
  <div class="mg">
    <div class="mg-head">
      <div>
        <h1 class="ch-page-title--mb">迁移历史</h1>
        <p class="mg-sub">节点的用户搬迁记录。24 小时内、带批次号的记录可以整批撤销。</p>
      </div>
      <div class="mg-tools">
        <Input v-model="userFilter" placeholder="用户 ID" class="mg-search" mono />
        <Input v-model="nodeFilter" placeholder="节点 ID" class="mg-search" mono />
        <Select v-model="reasonFilter" :options="reasonOptions" class="mg-sel" />
        <Button variant="secondary" size="sm" :loading="loading" @click="load">
          <Icon name="refresh" :size="14" /> 刷新
        </Button>
      </div>
    </div>

    <Card :padding="false">
      <div class="mg-table-wrap">
        <table class="ch-table">
          <thead>
            <tr>
              <th>时间</th>
              <th>用户</th>
              <th>节点（从 → 到）</th>
              <th>原因</th>
              <th>批次</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="m in rows" :key="m.id">
              <td class="mono">{{ fmtTime(m.created_at) }}</td>
              <td class="mono">#{{ m.user_id }}</td>
              <td class="mg-nodes">
                <template v-if="m.from_node_id == null">
                  <span class="mg-muted">首次分配</span>
                </template>
                <template v-else>
                  <span class="mono">{{ nodeLabel(m.from_node_id) }}</span>
                </template>
                <span class="mg-arrow">→</span>
                <span class="mono">{{ nodeLabel(m.to_node_id) }}</span>
              </td>
              <td>
                <Badge :variant="reasonVariant(m.reason)" dot>{{ reasonText(m.reason) }}</Badge>
              </td>
              <td class="mg-batch">
                <span v-if="m.batch_id" class="mono">{{ m.batch_id }}</span>
                <span v-else class="mg-muted">— 非接口产生</span>
              </td>
              <td>
                <Button
                  v-if="canUndo(m)"
                  variant="secondary"
                  size="sm"
                  :loading="undoBusy === m.batch_id"
                  @click="undo(m)"
                >
                  撤销
                </Button>
                <span v-else-if="!m.batch_id" class="mg-muted" title="没有批次号，无法按批次回滚">
                  不可撤销
                </span>
                <span v-else class="mg-muted" title="只允许撤销最近 24 小时内的批次">
                  已超 24 小时
                </span>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="6" class="ch-empty">加载失败：{{ loadError }}</td>
            </tr>
            <tr v-else-if="!rows.length && !loading">
              <td colspan="6" class="ch-empty">没有符合条件的迁移记录</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Pagination
      :page="pagination.current_page || 1"
      :last-page="pagination.last_page || 1"
      :total="totalText"
      :loading="loading"
      @prev="prev"
      @next="next"
    />
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Migrations.css"></style>
