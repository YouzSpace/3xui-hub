<script setup>
/**
 * Admin · 操作审计。
 *
 * 从原「系统状态」页拆出来：这里只放历史操作记录，带翻页和筛选，**不轮询**
 * —— 轮询会把正在看第 3 页的人刷回第 1 页。
 * 实时状态（worker / 定时任务 / 异步任务）在「系统监控」页。
 */
import { ref, watch, onMounted, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Pagination from '@/components/ui/Pagination.vue'
import { adminApi } from '@/api/admin'
import { usePagedList } from '@/composables/usePagedList'

const route = useRoute()
const router = useRouter()

const typeFilter = ref(String(route.query.type ?? ''))
const statusFilter = ref(String(route.query.status ?? ''))
const searchQuery = ref(String(route.query.q ?? ''))

const typeOptions = [
  { label: '全部类型', value: '' },
  { label: '管理员', value: 'admin' },
  { label: '用户', value: 'user' },
  { label: '系统', value: 'system' },
]
const statusOptions = [
  { label: '全部结果', value: '' },
  { label: '成功', value: 'ok' },
  { label: '失败', value: 'error' },
]

// page 是只读 computed（页码在 URL 里），pagination 只有元信息 → 列表数据自己存
const { page, loading, loadError, pagination, prev, next, fetchPage } = usePagedList()

const rows = ref([])

const typeVariant = (t) => ({ admin: 'info', user: 'success', system: 'warning' }[t] || 'info')
const typeText = (t) => ({ admin: '管理员', user: '用户', system: '系统' }[t] || t)

const fmtTime = (v) => {
  if (!v) return '—'
  const d = new Date(v)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

const load = async () => {
  const extra = {}
  if (typeFilter.value) extra.type = typeFilter.value
  if (statusFilter.value) extra.status = statusFilter.value
  const q = searchQuery.value.trim()
  if (q) extra.search = q

  const res = await fetchPage((params, config) => adminApi.auditLogs({ ...params, ...extra }), null)
  if (res) rows.value = res
}

/** 改筛选：条件进 URL，页码回 1（已在第 1 页就直接重拉） */
const applyFilters = () => {
  const query = {}
  if (typeFilter.value) query.type = typeFilter.value
  if (statusFilter.value) query.status = statusFilter.value
  if (searchQuery.value.trim()) query.q = searchQuery.value.trim()

  if (page.value !== 1) {
    router.push({ query }).catch(() => {})
  } else {
    load()
  }
}

// 页码变化（筛选归 1 / 点翻页）都要重拉
watch(page, load)

let searchTimer = null
watch([typeFilter, statusFilter], applyFilters)
watch(searchQuery, () => {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(applyFilters, 350)
})
onUnmounted(() => clearTimeout(searchTimer))

onMounted(load)
</script>

<template>
  <div class="al">
    <div class="al-head">
      <div>
        <h1 class="al-title">操作审计</h1>
        <p class="al-sub">谁在什么时候做了什么、结果如何。历史记录，不自动刷新。</p>
      </div>
      <div class="al-tools">
        <Select v-model="typeFilter" :options="typeOptions" class="al-sel" />
        <Select v-model="statusFilter" :options="statusOptions" class="al-sel" />
        <Input v-model="searchQuery" placeholder="搜索 操作/路径/操作者/报错" class="al-search" />
        <Button variant="secondary" size="sm" :loading="loading" @click="load">
          <Icon name="refresh" :size="14" /> 刷新
        </Button>
      </div>
    </div>

    <Card :padding="false">
      <div class="al-table-wrap">
        <table class="al-table">
          <thead>
            <tr>
              <th>时间</th>
              <th>类型</th>
              <th>操作者</th>
              <th>IP</th>
              <th>操作</th>
              <th>结果</th>
              <th>报错</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="l in rows" :key="l.id">
              <td class="mono">{{ fmtTime(l.created_at) }}</td>
              <td><Badge :variant="typeVariant(l.type)" dot>{{ typeText(l.type) }}</Badge></td>
              <td class="mono">{{ l.actor_name || '—' }}</td>
              <td class="mono al-dim">{{ l.ip || '—' }}</td>
              <td class="mono al-action-cell" :title="`${l.method} ${l.path}`">
                {{ l.action }}
              </td>
              <td>
                <Badge :variant="l.status >= 500 ? 'danger' : (l.status >= 400 ? 'warning' : 'success')" dot>
                  {{ l.status || '—' }}
                </Badge>
              </td>
              <td class="al-err-cell">
                <span v-if="l.error" class="al-err" :title="l.error">{{ l.error }}</span>
                <span v-else class="al-dim">—</span>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="7" class="al-empty">加载失败：{{ loadError }}</td>
            </tr>
            <tr v-else-if="!rows.length && !loading">
              <td colspan="7" class="al-empty">没有符合条件的记录</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Pagination
      :page="pagination.current_page || 1"
      :last-page="pagination.last_page || 1"
      :total="pagination.total || 0"
      :loading="loading"
      @prev="prev"
      @next="next"
    />
  </div>
</template>

<style scoped>
.al { max-width: 1280px; }
.al-head {
  display: flex; align-items: flex-start; justify-content: space-between;
  gap: var(--space-3); margin-bottom: var(--space-4); flex-wrap: wrap;
}
.al-title { margin: 0 0 4px; font-size: var(--text-xl); font-weight: var(--font-bold); }
.al-sub { margin: 0; color: var(--text-muted); font-size: var(--text-sm); }
.al-tools { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
.al-sel { width: 120px; }
.al-search { width: 220px; }

.al-table-wrap { overflow-x: auto; }
.al-table { width: 100%; border-collapse: collapse; font-size: var(--text-sm); }
.al-table th, .al-table td {
  padding: var(--space-3) var(--space-4); text-align: left;
  border-bottom: 1px solid var(--border-subtle); white-space: nowrap;
}
.al-table th { color: var(--text-muted); font-weight: var(--font-medium); font-size: var(--text-xs); }
.al-table tbody tr:hover { background: var(--bg-hover); }
/* 报错列：给足宽度 + 允许折行。与用户列表同款口径 —— 列宽由这一列自己定，
   屏幕放不下时整表横向滚动（.al-table-wrap 已 overflow-x:auto），
   不再用 max-width:320px 把长报错挤成竖排。 */
.al-table td.al-err-cell { width: 420px; min-width: 420px; max-width: 480px; white-space: normal; }
.al-table td.al-action-cell { max-width: 360px; overflow: hidden; text-overflow: ellipsis; }
.al-path { color: var(--text-muted); font-size: var(--text-xs); margin-left: 6px; }
.al-dim { color: var(--text-muted); }
.al-err { display: inline-block; color: #dc2626; font-size: var(--text-xs); word-break: break-word; }
.al-empty { text-align: center; color: var(--text-muted); padding: var(--space-6); }
</style>
