<script setup>
/**
 * Admin · 操作审计。
 *
 * 从原「系统状态」页拆出来：这里只放历史操作记录，带翻页和筛选，**不轮询**
 * —— 轮询会把正在看第 3 页的人刷回第 1 页。
 * 实时状态（worker / 定时任务 / 异步任务）在「系统监控」页。
 */
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Pagination from '@/components/ui/Pagination.vue'
import ExpandText from '@/components/ui/ExpandText.vue'
import { adminApi } from '@/api/admin'
import { usePagedList } from '@/composables/usePagedList'

const route = useRoute()
const router = useRouter()

const typeFilter = ref(String(route.query.type ?? ''))
const statusFilter = ref(String(route.query.status ?? ''))
// 二级筛选只在「类型=系统」时出现（见 actionOptions），值也带进 URL，刷新不丢
const actionFilter = ref(route.query.type === 'system' ? String(route.query.action ?? '') : '')
const searchQuery = ref(String(route.query.q ?? ''))

const typeOptions = [
  { label: '全部类型', value: '' },
  { label: '管理员', value: 'admin' },
  { label: '用户', value: 'user' },
  { label: '系统', value: 'system' },
  // 地区功能的两个专属类型（后端 operation_logs.type 精确匹配）。
  // 注意：后端目前还没有地方写这两种 type，选中会返回空列表，等后端落库后自动有数据。
  { label: '节点迁移', value: 'node_migration' },
  { label: '模板同步', value: 'template_sync' },
]
const statusOptions = [
  { label: '全部结果', value: '' },
  { label: '成功', value: 'ok' },
  { label: '失败', value: 'error' },
]

/**
 * 系统事件的二级筛选（只在前端拆类，后端只认 type/status/search）。
 *
 * 漂移与 3x-ui 写回失败是**必须有人看**的两类系统审计（见 collector/reconcile.go），
 * 混在 agent 升级、证书签发之类里根本看不出来，所以单独拎出来：
 *   drift / writeback —— 给后端 search 传 action 字面量，LIKE 命中 action 列，
 *                        分页与总数都是后端算的，翻页看到的仍是完整结果集；
 *   other             —— 是「排除式」，LIKE 表达不了，只能在本页里剔掉这两类
 *                        （这类是浏览用，不是查证用）。
 */
const DRIFT_PREFIX = '对账漂移'
const WRITEBACK_ACTION = '3x-ui 写回失败'

const actionOptions = [
  { label: '全部系统事件', value: '' },
  { label: '对账漂移', value: 'drift' },
  { label: '3x-ui 写回失败', value: 'writeback' },
  { label: '其他系统事件', value: 'other' },
]

const isDrift = (l) => String(l?.action || '').startsWith(DRIFT_PREFIX)
const isWritebackFail = (l) => String(l?.action || '').startsWith(WRITEBACK_ACTION)

// 行级别：会提前掐人=节点比面板紧、用户会被提前断网 → 红；其余漂移方向 → 黄。
// 3x-ui 写回失败=面板想改节点没改成，节点还停在旧状态 → 红。
const EARLY_CUT_ACTION = `${DRIFT_PREFIX}·会提前掐人`
const rowLevel = (l) => {
  // 前缀匹配而不是全等：后端日后给 action 加后缀（如带上字段名）也不会掉档
  if (isDrift(l)) return String(l.action).startsWith(EARLY_CUT_ACTION) ? 'danger' : 'warning'
  if (isWritebackFail(l)) return 'danger'
  return ''
}

// 漂移方向角标（文字取自 action 的后半段，如「会提前掐人」「节点更宽松」「安全方向」）
const driftMark = (l) => {
  if (!isDrift(l)) return null
  return { text: String(l.action).split('·')[1] || DRIFT_PREFIX, variant: rowLevel(l) }
}

// 选了「对账漂移 / 3x-ui 写回失败」时后端 search 被占用：搜索框停用，避免输入了却没生效
const searchDisabled = computed(
  () => typeFilter.value === 'system' && (actionFilter.value === 'drift' || actionFilter.value === 'writeback'),
)

// page 是只读 computed（页码在 URL 里），pagination 只有元信息 → 列表数据自己存
const { page, loading, loadError, pagination, prev, next, fetchPage } = usePagedList()

const rows = ref([])

const typeVariant = (t) =>
  ({ admin: 'info', user: 'success', system: 'warning', node_migration: 'vless', template_sync: 'trojan' }[t] || 'info')
const typeText = (t) =>
  ({ admin: '管理员', user: '用户', system: '系统', node_migration: '节点迁移', template_sync: '模板同步' }[t] || t)

const fmtTime = (v) => {
  if (!v) return '—'
  const d = new Date(v)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

const load = async () => {
  const extra = {}
  const systemScope = typeFilter.value === 'system'
  if (typeFilter.value) extra.type = typeFilter.value
  if (statusFilter.value) extra.status = statusFilter.value

  const q = searchQuery.value.trim()
  if (systemScope && actionFilter.value === 'drift') {
    extra.search = DRIFT_PREFIX
  } else if (systemScope && actionFilter.value === 'writeback') {
    extra.search = WRITEBACK_ACTION
  } else if (q) {
    extra.search = q
  }

  const res = await fetchPage((params, config) => adminApi.auditLogs({ ...params, ...extra }), null)
  if (!res) return
  // 「其他系统事件」是排除式：后端没办法表达，落地时把两类高危记录剔掉
  rows.value =
    systemScope && actionFilter.value === 'other'
      ? res.filter((l) => !isDrift(l) && !isWritebackFail(l))
      : res
}

/** 改筛选：条件进 URL，页码回 1（已在第 1 页就直接重拉） */
const applyFilters = () => {
  const query = {}
  if (typeFilter.value) query.type = typeFilter.value
  if (statusFilter.value) query.status = statusFilter.value
  if (typeFilter.value === 'system' && actionFilter.value) query.action = actionFilter.value
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
watch([statusFilter, actionFilter], applyFilters)
// 切换类型：离开「系统」时二级筛选自动清空（它只对系统事件有意义）
watch(typeFilter, () => {
  if (typeFilter.value !== 'system' && actionFilter.value) {
    actionFilter.value = '' // 由 actionFilter 的 watch 触发重拉
    return
  }
  applyFilters()
})
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
        <h1 class="ch-page-title--mb">操作审计</h1>
        <p class="al-sub">谁在什么时候做了什么、结果如何。历史记录，不自动刷新。</p>
      </div>
      <div class="al-tools">
        <Select v-model="typeFilter" :options="typeOptions" class="al-sel" />
        <Select
          v-if="typeFilter === 'system'"
          v-model="actionFilter"
          :options="actionOptions"
          class="al-sel al-sel-action"
        />
        <Select v-model="statusFilter" :options="statusOptions" class="al-sel" />
        <Input
          v-model="searchQuery"
          :disabled="searchDisabled"
          :placeholder="searchDisabled ? '已按动作筛选，清空动作后可搜索' : '搜索 操作/路径/操作者/报错'"
          class="al-search"
        />
        <Button variant="secondary" size="sm" :loading="loading" @click="load">
          <Icon name="refresh" :size="14" /> 刷新
        </Button>
      </div>
    </div>

    <!-- 排除式筛选只能在本页剔除：说清楚，免得把「共 N 条」当成过滤后的条数 -->
    <p v-if="typeFilter === 'system' && actionFilter === 'other'" class="al-note">
      「其他系统事件」只从当前页里剔掉对账漂移与 3x-ui 写回失败记录；总数与翻页仍按全部系统事件计算。
    </p>

    <Card :padding="false">
      <div class="al-table-wrap">
        <table class="ch-table ch-table--nolasthide">
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
            <tr
              v-for="l in rows"
              :key="l.id"
              :class="rowLevel(l) ? `al-row-${rowLevel(l)}` : ''"
            >
              <td class="mono">{{ fmtTime(l.created_at) }}</td>
              <td>
                <div class="al-type-cell">
                  <Badge :variant="typeVariant(l.type)" dot>{{ typeText(l.type) }}</Badge>
                  <Badge v-if="driftMark(l)" :variant="driftMark(l).variant">{{ driftMark(l).text }}</Badge>
                </div>
              </td>
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
                <ExpandText v-if="l.error" :text="l.error" :limit="80" danger />
                <span v-else class="al-dim">—</span>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="7" class="ch-empty">加载失败：{{ loadError }}</td>
            </tr>
            <tr v-else-if="!rows.length && !loading">
              <td colspan="7" class="ch-empty">没有符合条件的记录</td>
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

<style scoped src="@/assets/styles/pages/admin/AuditLogs.css"></style>
