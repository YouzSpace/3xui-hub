<script setup>
/**
 * Admin 支付配置：表格 + 创建/编辑 Modal + 删除。
 * 协议字段由后台 /admin-api/payments/protocols 下发，按所选协议动态渲染；
 * 通用字段（名称/回调地址/启用）保持现有交互。
 */
import { ref, computed, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const payments = ref([])
const protocols = ref([])
const protocolOptions = computed(() => protocols.value.map((p) => ({ label: p.label, value: p.key })))
const loading = ref(false)

const modalOpen = ref(false)
const editing = ref(null)
const form = ref({
  name: '',
  driver_type: 'payindex',
  fields: {},
  notify_url: '',
  enabled: true,
})

// 旧列字段名（与 payment_configs 表列名一致）：保存时镜像一份，老读取路径零影响
const LEGACY_KEYS = ['gateway', 'query_gateway', 'member_id', 'api_key', 'bank_code']

// 当前协议定义（老数据 driver_type 为空按 payindex 处理）
const currentProtocol = computed(() => {
  const key = form.value.driver_type || 'payindex'
  return protocols.value.find((p) => p.key === key) || protocols.value[0] || null
})

const protocolLabel = (t) => {
  const key = t || 'payindex'
  return protocols.value.find((p) => p.key === key)?.label || key
}

const load = async () => {
  loading.value = true
  try {
    payments.value = await adminApi.listPayments()
  } catch (e) {
    toast.error('加载失败')
  } finally {
    loading.value = false
  }
}

const loadProtocols = async () => {
  try {
    const list = await adminApi.listPaymentProtocols()
    protocols.value = Array.isArray(list) ? list : []
  } catch (e) {
    protocols.value = []
    toast.error('协议清单加载失败')
  }
}

const resetForm = () => {
  form.value = {
    name: '',
    driver_type: protocols.value[0]?.key || 'payindex',
    fields: {},
    notify_url: '',
    enabled: true,
  }
}

const openCreate = () => {
  editing.value = null
  resetForm()
  modalOpen.value = true
}

const openEdit = async (p) => {
  editing.value = p
  resetForm()
  try {
    const detail = await adminApi.getPayment(p.id)
    // 老数据的字段在旧列里，新数据在 driver_config 里：driver_config 优先，合并回显
    const fields = {}
    for (const k of LEGACY_KEYS) fields[k] = detail[k] || ''
    Object.assign(fields, detail.driver_config || {})
    fields.api_key = '' // 密钥不回显，留空表示不修改
    form.value = {
      name: detail.name,
      driver_type: detail.driver_type || 'payindex',
      fields,
      notify_url: detail.notify_url || '',
      enabled: detail.enabled,
    }
  } catch (e) {
    toast.error('加载配置详情失败')
  }
  modalOpen.value = true
}

const save = async () => {
  if (!form.value.name) {
    toast.error('请填写配置名称')
    return
  }

  // 必填校验按当前协议定义来（编辑时密文字段留空 = 不修改，跳过校验）
  const isEdit = !!editing.value
  for (const f of currentProtocol.value?.fields || []) {
    const v = String(form.value.fields[f.name] ?? '').trim()
    if (f.required && !v && !(isEdit && f.isSecret)) {
      toast.error(`请填写${f.label}`)
      return
    }
  }

  // 编辑时密文留空 = 不修改：整批按 isSecret 通用处理（api_key/应用私钥/支付宝公钥等）
  const fields = { ...form.value.fields }
  if (isEdit) {
    for (const f of currentProtocol.value?.fields || []) {
      if (f.isSecret && !String(fields[f.name] ?? '').trim()) {
        delete fields[f.name]
      }
    }
  }

  const payload = {
    name: form.value.name,
    enabled: form.value.enabled,
    notify_url: form.value.notify_url,
    driver_type: form.value.driver_type,
    driver_config: fields,
  }
  // 同名旧列镜像一份（两种协议的字段名都落在旧列上，列表/老代码读旧列不受影响）
  for (const k of LEGACY_KEYS) {
    if (fields[k] !== undefined && fields[k] !== '') payload[k] = fields[k]
    else if (k !== 'api_key') payload[k] = fields[k] ?? ''
  }

  try {
    if (isEdit) {
      await adminApi.updatePayment(editing.value.id, payload)
      toast.success('已更新')
    } else {
      await adminApi.createPayment(payload)
      toast.success('已创建')
    }
    modalOpen.value = false
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  }
}

const remove = async (p) => {
  if (!confirm(`删除支付配置「${p.name}」？`)) return
  try {
    await adminApi.deletePayment(p.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

onMounted(() => {
  load()
  loadProtocols()
})
onActivated(load)
</script>

<template>
  <div class="pay">
    <div class="pay-head">
      <h1 class="pay-title">支付配置</h1>
      <Button variant="primary" size="sm" @click="openCreate"><Icon name="plus" :size="16" /> 新建配置</Button>
    </div>

    <Card padding="false">
      <div class="pay-table-wrap">
        <table class="pay-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>协议</th>
              <th>商户号</th>
              <th>银行编码</th>
              <th>网关地址</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in payments" :key="p.id">
              <td>{{ p.name }}</td>
              <td>{{ protocolLabel(p.driver_type) }}</td>
              <td class="mono">{{ p.member_id }}</td>
              <td class="mono">{{ p.bank_code }}</td>
              <td class="pay-url">{{ p.gateway }}</td>
              <td><Badge :variant="p.enabled ? 'success' : 'danger'" dot>{{ p.enabled ? '启用' : '禁用' }}</Badge></td>
              <td>
                <div class="pay-actions">
                  <Button variant="ghost" size="sm" @click="openEdit(p)"><Icon name="edit" :size="14" /></Button>
                  <Button variant="ghost" size="sm" @click="remove(p)"><Icon name="trash" :size="14" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!payments.length">
              <td colspan="7" class="pay-empty">暂无支付配置</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Modal v-model="modalOpen" :title="editing ? '编辑支付配置' : '新建支付配置'" width="560px">
      <div class="pay-form">
        <label class="pay-field">
          <span>配置名称</span>
          <Input v-model="form.name" placeholder="如：微信支付" />
        </label>

        <label class="pay-field">
          <span>支付协议</span>
          <Select v-model="form.driver_type" :options="protocolOptions" />
        </label>
        <p v-if="currentProtocol?.description" class="pay-desc">{{ currentProtocol.description }}</p>

        <!-- 协议字段：label / placeholder / required / 密文框全部来自后台协议定义 -->
        <label v-for="f in currentProtocol?.fields || []" :key="f.name" class="pay-field">
          <span>
            {{ f.label }}<template v-if="editing && f.isSecret">（留空不修改）</template><template v-else-if="!f.required">（选填）</template>
          </span>
          <Input
            v-model="form.fields[f.name]"
            :type="f.isSecret ? 'password' : 'text'"
            mono
            :placeholder="editing && f.isSecret ? '留空则不修改' : f.placeholder"
          />
        </label>

        <label class="pay-field">
          <span>回调地址（留空使用系统默认）</span>
          <Input v-model="form.notify_url" mono placeholder="留空则自动生成" />
        </label>
        <label class="pay-field pay-field-row">
          <input v-model="form.enabled" type="checkbox" />
          <span>启用</span>
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="modalOpen = false">取消</Button>
        <Button variant="primary" size="sm" @click="save">保存</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.pay-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-4);
}
.pay-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.pay-table-wrap {
  overflow-x: auto;
}
.pay-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.pay-table th,
.pay-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
.pay-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.pay-table tbody tr:hover {
  background: var(--bg-hover);
}
.pay-url {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  max-width: 200px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.pay-actions {
  display: flex;
  gap: var(--space-1);
}
.pay-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
.pay-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.pay-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-3);
}
/* 窄屏（手机）：两列表单压得太窄，单列显示。 */
@media (max-width: 767px) {
  .pay-grid {
    grid-template-columns: 1fr;
  }
}
.pay-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.pay-field-row {
  flex-direction: row;
  align-items: center;
}
.pay-desc {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
  line-height: 1.6;
}
.mono {
  font-family: var(--font-mono);
}

/* 表单栅格对齐：标签统一占两行高度，长标签换行时不会把输入框挤下去 */
.pay-grid { align-items: start; }
.pay-grid > .pay-field > span:first-child { display: block; min-height: 2.6em; line-height: 1.35; }
</style>
