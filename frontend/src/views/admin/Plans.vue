<script setup>
/**
 * Admin 套餐管理：表格 + 创建/编辑 Modal + 删除。
 * 两种类型：周期套餐（period）、总量套餐（total）。
 * 列表分页：每页 50 条，页码走 ?page=（刷新/切页回来不丢页）。
 */
import { ref, watch, computed, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import Pagination from '@/components/ui/Pagination.vue'
import Skeleton from '@/components/ui/Skeleton.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { usePagedList } from '@/composables/usePagedList'

const toast = useToast()
const plans = ref([])
const thirdSubs = ref([]) // 第三方订阅选项（勾选进套餐用）

const { page, loading, loadError, pagination, hasPrev, hasNext, prev, next, fetchPage } = usePagedList()

const modalOpen = ref(false)
const editing = ref(null)
const saving = ref(false)
const saved = ref(false)
const form = ref({
  name: '',
  price: 0,
  reset_price: 0,
  type: 'period',
  months: 1,
  monthly_traffic_gb: 0,
  period_traffic_gb: 0,
  total_traffic_gb: 0,
  include_local: true,
  third_ids: [],
})
const GB = 1024 * 1024 * 1024
const typeOptions = [
  { label: '周期套餐', value: 'period' },
  { label: '总量套餐', value: 'total' },
]

// 纯第三方套餐（没勾本地节点、勾了第三方线路）：第三方不管流量只管时间
//  → 锁定为周期套餐，流量字段藏掉归零，只改天数
const pureThird = computed(() => !form.value.include_local && form.value.third_ids.length > 0)

// 纯第三方时联动：强制周期 + 流量归零；取消纯第三方时恢复默认流量字段
watch(pureThird, (on) => {
  if (on) {
    form.value.type = 'period'
    form.value.monthly_traffic_gb = 0
    form.value.total_traffic_gb = 0
  }
})

const fmtBytes = (b) => {
  if (!b || b < 0 || !Number.isFinite(b)) return '0'
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB']
  const raw = Math.floor(Math.log(b || 1) / Math.log(1024))
  const i = Math.max(0, Math.min(raw, u.length - 1))
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)} ${u[i]}`
}

const fmtPrice = (p) => {
  if (!p && p !== 0) return '—'
  return p > 0 ? `¥${p}` : '免费'
}

const load = async () => {
  const res = await fetchPage((params, config) => adminApi.listPlans(params, config))
  if (res) plans.value = res
}

// 第三方订阅选项（套餐弹窗里勾选用）；没有接入过第三方时列表为空，弹窗自动隐藏该栏
const loadThirdSubs = async () => {
  try {
    const data = await adminApi.listThirdParty()
    thirdSubs.value = (data?.items ?? []).filter((s) => s.enabled)
  } catch {
    // 第三方接口挂了不影响套餐页本身
  }
}

watch(page, load)

const openCreate = () => {
  editing.value = null
  form.value = {
    name: '',
    price: 0,
    reset_price: 0,
    type: 'period',
    months: 1,
    monthly_traffic_gb: 0,
    period_traffic_gb: 0,
    total_traffic_gb: 0,
    include_local: true,
    third_ids: [],
  }
  loadThirdSubs()
  modalOpen.value = true
}

const openEdit = (p) => {
  editing.value = p
  form.value = {
    name: p.name,
    price: p.price || 0,
    reset_price: p.reset_price || 0,
    type: p.type,
    months: p.months || 1,
    monthly_traffic_gb: p.monthly_traffic ? Math.round((p.monthly_traffic / GB) * 100) / 100 : 0,
    period_traffic_gb: p.period_traffic ? Math.round((p.period_traffic / GB) * 100) / 100 : 0,
    total_traffic_gb: p.total_traffic ? Math.round((p.total_traffic / GB) * 100) / 100 : 0,
    include_local: p.include_local !== false,
    third_ids: [...(p.third_party_sub_ids ?? [])],
  }
  loadThirdSubs()
  modalOpen.value = true
}

const save = async () => {
  if (saving.value || saved.value) return
  saving.value = true
  try {
    const payload = {
      name: form.value.name,
      price: Number(form.value.price),
      reset_price: Number(form.value.reset_price),
      type: form.value.type,
      include_local: !!form.value.include_local,
      third_party_sub_ids: [...form.value.third_ids],
    }
    if (form.value.type === 'period') {
      payload.months = Number(form.value.months)
      payload.monthly_traffic = Math.round(Number(form.value.monthly_traffic_gb) * GB)
      payload.period_traffic = Math.round(Number(form.value.monthly_traffic_gb) * Number(form.value.months) * GB)
    } else {
      payload.total_traffic = Math.round(Number(form.value.total_traffic_gb) * GB)
    }

    if (editing.value) {
      await adminApi.updatePlan(editing.value.id, payload)
      toast.success('已更新')
    } else {
      await adminApi.createPlan(payload)
      toast.success('已创建')
    }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    saved.value = true
    setTimeout(() => {
      saved.value = false
      modalOpen.value = false
    }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const deactivate = async (p) => {
  if (!confirm(`下架套餐「${p.name}」？已购用户可继续正常使用。`)) return
  try {
    await adminApi.deactivatePlan(p.id)
    // 撤销：重新上架（下架可反悔）
    toast.success(`已下架「${p.name}」`, 4000, async () => {
      try {
        await adminApi.activatePlan(p.id)
        await load()
        toast.success('已恢复上架')
      } catch (e) {
        toast.error(e.message || '恢复失败')
      }
    })
    await load()
  } catch (e) {
    toast.error(e.message || '下架失败')
  }
}

const activate = async (p) => {
  try {
    await adminApi.activatePlan(p.id)
    toast.success('已上架')
    await load()
  } catch (e) {
    toast.error(e.message || '上架失败')
  }
}

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="pl">
    <div class="pl-head">
      <h1 class="pl-title">套餐</h1>
      <Button variant="primary" size="sm" @click="openCreate"><Icon name="plus" :size="16" /> 新建套餐</Button>
    </div>

    <Card padding="false">
      <div class="pl-table-wrap">
        <table class="pl-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>价格</th>
              <th>类型</th>
              <th>状态</th>
              <th>规则</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in plans" :key="p.id">
              <td>{{ p.name }}</td>
              <td class="pl-price">{{ fmtPrice(p.price) }}</td>
              <td>
                <Badge :variant="p.type === 'period' ? 'vless' : 'trojan'">
                  {{ p.type === 'period' ? '周期' : '总量' }}
                </Badge>
              </td>
              <td>
                <Badge :variant="p.is_active ? 'success' : 'warning'" dot>
                  {{ p.is_active ? '已上架' : '已下架' }}
                </Badge>
              </td>
              <td class="pl-rules">
                <template v-if="p.type === 'period'">
                  {{ p.months }}个月 / 月限 {{ fmtBytes(p.monthly_traffic) }} / 周期总量 {{ fmtBytes(p.period_traffic) }}
                </template>
                <template v-else>
                  总量 {{ fmtBytes(p.total_traffic) }} / 永久有效
                </template>
              </td>
              <td>
                <div class="pl-actions">
                  <Button variant="ghost" size="sm" @click="openEdit(p)"><Icon name="edit" :size="14" /></Button>
                  <Button v-if="p.is_active" variant="ghost" size="sm" title="下架" @click="deactivate(p)"><Icon name="eye-off" :size="14" /></Button>
                  <Button v-else variant="ghost" size="sm" title="上架" @click="activate(p)"><Icon name="refresh" :size="14" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="6" class="pl-empty">
                加载失败：{{ loadError }}
                <Button variant="secondary" size="sm" @click="load">重试</Button>
              </td>
            </tr>
            <tr v-else-if="!plans.length">
              <td colspan="6" class="pl-empty">
                <Skeleton v-if="loading" :rows="5" height="32px" />
                <template v-else>暂无套餐</template>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Pagination
      :page="pagination.current_page"
      :last-page="pagination.last_page"
      :total="pagination.total"
      :loading="loading"
      @prev="prev"
      @next="next"
    />

    <Modal v-model="modalOpen" :title="editing ? '编辑套餐' : '新建套餐'" width="520px">
      <div class="pl-form">
        <label class="pl-field">
          <span>套餐名称</span>
          <Input v-model="form.name" placeholder="如：月度100G" />
        </label>
        <label class="pl-field">
          <span>价格（元，0 表示免费）</span>
          <Input v-model="form.price" type="number" mono placeholder="0" />
        </label>
        <label v-if="form.type === 'period' && !pureThird" class="pl-field">
          <span>重置流量价格（元，0 表示免费）</span>
          <Input v-model="form.reset_price" type="number" mono placeholder="0" />
        </label>
        <label class="pl-field">
          <span>类型</span>
          <Select v-model="form.type" :options="typeOptions" :disabled="!!editing || pureThird" />
          <span v-if="editing && !pureThird" class="pl-hint">类型不可修改</span>
          <span v-if="pureThird" class="pl-hint">纯第三方套餐锁定为周期（只计天数）</span>
        </label>

        <template v-if="form.type === 'period'">
          <label class="pl-field">
            <span>周期月数</span>
            <Input v-model="form.months" type="number" mono placeholder="3" />
          </label>
          <label v-if="!pureThird" class="pl-field">
            <span>每月流量上限（GB）</span>
            <Input v-model="form.monthly_traffic_gb" type="number" mono placeholder="100" />
          </label>
          <label v-if="!pureThird" class="pl-field">
            <span>周期总流量上限（GB）</span>
            <div class="pl-auto-value">{{ (Number(form.monthly_traffic_gb) || 0) * (Number(form.months) || 1) }} GB（自动计算）</div>
          </label>
        </template>

        <template v-else>
          <label class="pl-field">
            <span>总流量上限（GB）</span>
            <Input v-model="form.total_traffic_gb" type="number" mono placeholder="500" />
          </label>
        </template>

        <!-- 节点来源：本地节点开关 + 第三方线路（想加几条加几条） -->
        <div class="pl-nodes">
          <span class="pl-nodes-title">节点来源</span>
          <label class="pl-nodes-row">
            <input type="checkbox" v-model="form.include_local" />
            <span>本地节点（面板自有）</span>
          </label>
          <template v-if="thirdSubs.length">
            <p class="pl-sub-label">第三方线路（勾选即注入该套餐用户订阅，节点名对用户显示为地区+序号）</p>
            <label v-for="s in thirdSubs" :key="s.id" class="pl-nodes-row">
              <input
                type="checkbox"
                :value="s.id"
                v-model="form.third_ids"
              />
              <span>{{ s.name }}（{{ s.node_count ?? 0 }} 节点）</span>
            </label>
          </template>
          <p v-else class="pl-hint">
            <router-link to="/admin/upstream/third-party" class="pl-link">去「上游 · 第三方」页</router-link> 接入线路后，可在此勾选进套餐。
          </p>
          <p v-if="!form.include_local && !form.third_ids.length" class="pl-warn">
            当前该套餐不含任何节点（本地与第三方都没勾）——用户订阅将为空。
          </p>
        </div>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="modalOpen = false">取消</Button>
        <Button variant="primary" size="sm" :disabled="saving" :success="saved" @click="save">{{ saving ? '保存中...' : '保存' }}</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.pl-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-4);
}
.pl-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.pl-table-wrap {
  overflow-x: auto;
}
.pl-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.pl-table th,
.pl-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
.pl-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.pl-table tbody tr:hover {
  background: var(--bg-hover);
}
.pl-price {
  font-family: var(--font-mono);
  font-weight: var(--font-medium);
  color: var(--accent);
}
.pl-rules {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
}
.pl-actions {
  display: flex;
  gap: var(--space-1);
}
.pl-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
.pl-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.pl-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}

.pl-hint {
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.pl-auto-value {
  background: var(--bg-input);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  color: var(--text-muted);
  padding: 10px 14px;
  font-size: var(--text-base);
  font-family: var(--font-mono);
}
/* 节点来源：本地开关 + 第三方多选，分区与流量字段隔开 */
.pl-nodes {
  border-top: 1px solid var(--border-subtle);
  padding-top: var(--space-3);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.pl-nodes-title {
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  color: var(--text-primary);
}
.pl-sub-label {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.pl-nodes-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--text-secondary);
  cursor: pointer;
}
.pl-nodes-row input[type='checkbox'] {
  cursor: pointer;
}
.pl-link {
  color: var(--accent);
  text-decoration: none;
}
.pl-link:hover {
  text-decoration: underline;
}
.pl-warn {
  margin: 0;
  font-size: var(--text-xs);
  color: #f87171;
}
</style>
