<script setup>
/**
 * Admin 套餐管理：表格 + 创建/编辑 Modal + 删除。
 * 两种类型：周期套餐（period）、总量套餐（total）。
 */
import { ref, computed, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const plans = ref([])
const loading = ref(false)

const modalOpen = ref(false)
const editing = ref(null)
const saving = ref(false)
const form = ref({
  name: '',
  price: 0,
  reset_price: 0,
  type: 'period',
  months: 1,
  monthly_traffic_gb: 0,
  period_traffic_gb: 0,
  total_traffic_gb: 0,
})
const GB = 1024 * 1024 * 1024

const fmtBytes = (b) => {
  if (!b) return '0'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(b || 1) / Math.log(1024))
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)} ${u[i]}`
}

const fmtPrice = (p) => {
  if (!p && p !== 0) return '—'
  return p > 0 ? `¥${p}` : '免费'
}

const load = async () => {
  loading.value = true
  try {
    plans.value = await adminApi.listPlans()
  } catch (e) {
    toast.error('加载失败')
  } finally {
    loading.value = false
  }
}

const openCreate = () => {
  editing.value = null
  form.value = { name: '', price: 0, reset_price: 0, type: 'period', months: 1, monthly_traffic_gb: 0, period_traffic_gb: 0, total_traffic_gb: 0 }
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
  }
  modalOpen.value = true
}

const save = async () => {
  if (saving.value) return
  saving.value = true
  try {
    const payload = {
      name: form.value.name,
      price: Number(form.value.price),
      reset_price: Number(form.value.reset_price),
      type: form.value.type,
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
    modalOpen.value = false
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const remove = async (p) => {
  if (!confirm(`删除套餐「${p.name}」？已绑定的用户不受影响。`)) return
  try {
    await adminApi.deletePlan(p.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

onMounted(load)
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
                  <Button variant="ghost" size="sm" @click="remove(p)"><Icon name="trash" :size="14" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!plans.length">
              <td colspan="5" class="pl-empty">暂无套餐</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

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
        <label v-if="form.type === 'period'" class="pl-field">
          <span>重置流量价格（元，0 表示免费）</span>
          <Input v-model="form.reset_price" type="number" mono placeholder="0" />
        </label>
        <label class="pl-field">
          <span>类型</span>
          <select v-model="form.type" class="pl-select" :disabled="!!editing">
            <option value="period">周期套餐</option>
            <option value="total">总量套餐</option>
          </select>
          <span v-if="editing" class="pl-hint">类型不可修改</span>
        </label>

        <template v-if="form.type === 'period'">
          <label class="pl-field">
            <span>周期月数</span>
            <Input v-model="form.months" type="number" mono placeholder="3" />
          </label>
          <label class="pl-field">
            <span>每月流量上限（GB）</span>
            <Input v-model="form.monthly_traffic_gb" type="number" mono placeholder="100" />
          </label>
          <label class="pl-field">
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
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="modalOpen = false">取消</Button>
        <Button variant="primary" size="sm" :disabled="saving" @click="save">{{ saving ? '保存中...' : '保存' }}</Button>
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
.pl-select {
  background: var(--bg-input);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  color: var(--text-primary);
  padding: 10px 14px;
  font-size: var(--text-base);
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
</style>
