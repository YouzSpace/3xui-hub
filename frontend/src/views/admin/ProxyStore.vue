<script setup>
/**
 * Admin · 代理商品（代理商城后台）。
 *
 * 和节点套餐（Plans）完全独立：这张表只服务代理商城，不影响节点/订阅。
 *
 * 两类货源：
 *   webshare + exclusive  开上游子账号，一人一个，库存 = 上游剩余子账号数
 *   socks     + shared    多人共用一条手工 socks，库存 = 你填的份数
 *   socks     + exclusive 一人一条，下单时从池子里取一条未售出的（库存看池子）
 *
 * 流量不归平台管：webshare 建号时把额度传给上游，平台只读回用量；
 * socks 共享型不限流量（交给 socks 机器自己管）。
 */
import { ref, computed, onMounted } from 'vue'
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

const products = ref([])
const providers = ref([])
const socks = ref([])
const loading = ref(false)

// 代理商城总开关（默认关闭）：控制用户端是否出现「代理」入口 + 接口是否放行
const storeEnabled = ref(false)
const storeToggling = ref(false)

// 商品弹窗
const productOpen = ref(false)
const productEditing = ref(null)
const productForm = ref(blankProduct())
const productSaving = ref(false)

// 规格弹窗
const skuOpen = ref(false)
const skuEditing = ref(null)
const skuProductId = ref(null)
const skuForm = ref(blankSku())
const skuSaving = ref(false)

// socks 导入/编辑
const importOpen = ref(false)
const importText = ref('')
const importing = ref(false)
const sockOpen = ref(false)
const sockEditing = ref(null)
const sockForm = ref({})
const sockSaving = ref(false)

const sourceOptions = [
  { label: 'Webshare 子账号（独占）', value: 'webshare' },
  { label: '手工 socks（按分配方式）', value: 'socks' },
]

// 分配方式：共享 = 多人共用一条线路；独占 = 一条只卖一个人
const allocationOptions = [
  { label: '共享（多人共用一条）', value: 'shared' },
  { label: '独占（一人一条）', value: 'exclusive' },
]

function blankProduct() {
  return { name: '', subtitle: '', description: '', is_active: true, sort_order: 0 }
}

function blankSku() {
  return {
    name: '',
    source_type: 'webshare',
    // webshare 天然独占（一人一个上游子账号），socks 默认共享
    allocation: 'exclusive',
    source_id: '',
    region: '',
    traffic_gb: 100,
    duration_days: 30,
    max_thread_count: 50,
    price: 19.9,
    cost_price: 0,
    stock_total: 3,
    is_active: true,
    sort_order: 0,
  }
}

const providerOptions = computed(() => [
  { label: '自动（挑还有额度的）', value: '' },
  ...providers.value.map((p) => ({ label: `${p.name}（子用户 ${p.account_count}）`, value: p.id })),
])

// 规格的「实际」分配方式：webshare 天然独占（一人一个上游子账号），
// socks 看管理员选的（后端老数据可能没这字段 → 按共享兜底）
const skuAllocationOf = (ko) => {
  if (ko?.source_type === 'webshare') return 'exclusive'
  return ko?.allocation === 'exclusive' ? 'exclusive' : 'shared'
}
// 弹窗里正在编辑的这份
const skuAllocation = computed(() => skuAllocationOf(skuForm.value))

// 独占模式的货源可以「不指定」：下单时自动从独占池里挑一条没卖出的
const socksOptions = computed(() => [
  skuAllocation.value === 'exclusive'
    ? { label: '不指定（自动从独占池挑）', value: '' }
    : { label: '请选择一条 socks', value: '' },
  ...socks.value.map((s) => ({
    label: `${s.host}:${s.port}${s.username ? ` · ${s.username}` : ''}${s.country ? ` · ${s.country}` : ''}`,
    value: s.id,
  })),
])

const sourceLabel = (sku) => {
  if (sku.source_type !== 'socks') return 'Webshare · 独占账号'
  return skuAllocationOf(sku) === 'exclusive' ? 'socks · 独占' : 'socks · 多人共用'
}

// 还能卖几份：后端给了 stock_available 就用它，老接口没有就自己算（总数-占用-已售）
const stockAvailable = (sku) =>
  sku.stock_available ??
  Math.max(0, (sku.stock_total ?? 0) - (sku.stock_locked ?? 0) - (sku.stock_sold ?? 0))

// socks 货源的分配方式文案（后端未返回 allocation 时按共享兜底，不猜「独占」）
const sockAllocLabel = (s) => (s.allocation === 'exclusive' ? '独占' : '共享')
// 已售给谁：只有独占且确实卖了才显示用户 id
const sockSoldTo = (s) =>
  s.allocation === 'exclusive' && s.sold_to ? `用户 #${s.sold_to}` : '—'

const loadSettings = async () => {
  try {
    const d = await adminApi.proxyStoreSettings()
    storeEnabled.value = !!d?.enabled
  } catch (e) {
    storeEnabled.value = false
  }
}

const toggleStore = async () => {
  if (storeToggling.value) return
  storeToggling.value = true
  try {
    const d = await adminApi.updateProxyStoreSettings(!storeEnabled.value)
    storeEnabled.value = !!d?.enabled
    toast.success(storeEnabled.value ? '代理商城已开启：用户端能看到入口了' : '代理商城已关闭：用户端入口已隐藏')
  } catch (e) {
    toast.error(e.message || '切换失败')
  } finally {
    storeToggling.value = false
  }
}

const loadAll = async () => {
  loading.value = true
  try {
    const [p, s] = await Promise.all([adminApi.listProxyProducts(), adminApi.listProxySocks()])
    products.value = p?.items || []
    socks.value = s?.items || []
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
  try {
    const d = await adminApi.listWebshareProviders()
    providers.value = d?.items || []
  } catch (e) {
    providers.value = []
  }
}

/* ── 商品 ─────────────────────────────── */

const openCreateProduct = () => {
  productEditing.value = null
  productForm.value = blankProduct()
  productOpen.value = true
}

const openEditProduct = (p) => {
  productEditing.value = p
  productForm.value = {
    name: p.name,
    subtitle: p.subtitle || '',
    description: p.description || '',
    is_active: p.is_active,
    sort_order: p.sort_order,
  }
  productOpen.value = true
}

const saveProduct = async () => {
  const f = productForm.value
  if (!f.name.trim()) {
    toast.error('请填写商品名')
    return
  }
  productSaving.value = true
  try {
    const payload = {
      name: f.name.trim(),
      subtitle: f.subtitle?.trim() || null,
      description: f.description?.trim() || null,
      is_active: !!f.is_active,
      sort_order: Number(f.sort_order) || 0,
    }
    if (productEditing.value) await adminApi.updateProxyProduct(productEditing.value.id, payload)
    else await adminApi.createProxyProduct(payload)
    toast.success('已保存')
    productOpen.value = false
    await loadAll()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    productSaving.value = false
  }
}

const removeProduct = async (p) => {
  if (!confirm(`删除商品「${p.name}」？\n已有售出的商品不能删，只能关掉上架。`)) return
  try {
    await adminApi.deleteProxyProduct(p.id)
    toast.success('已删除')
    await loadAll()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

/* ── 规格 ─────────────────────────────── */

const openCreateSku = (p) => {
  skuEditing.value = null
  skuProductId.value = p.id
  skuForm.value = blankSku()
  skuOpen.value = true
}

const openEditSku = (sku) => {
  skuEditing.value = sku
  skuProductId.value = sku.product_id
  skuForm.value = {
    name: sku.name,
    source_type: sku.source_type || 'webshare',
    allocation: sku.allocation ?? 'shared',
    source_id: sku.source_id ?? '',
    region: sku.region || '',
    traffic_gb: sku.traffic_gb ?? 0,
    duration_days: sku.duration_days ?? 30,
    max_thread_count: sku.max_thread_count ?? 0,
    price: Number(sku.price),
    cost_price: Number(sku.cost_price),
    stock_total: sku.stock_total ?? 0,
    is_active: !!sku.is_active,
    sort_order: sku.sort_order ?? 0,
  }
  skuOpen.value = true
}

const saveSku = async () => {
  const f = skuForm.value
  if (!f.name.trim()) {
    toast.error('请填写规格名')
    return
  }
  if (f.source_type === 'socks' && !f.source_id && skuAllocation.value !== 'exclusive') {
    toast.error('socks 货源必须选择一条已录入的 socks')
    return
  }
  skuSaving.value = true
  try {
    const payload = {
      name: f.name.trim(),
      source_type: f.source_type,
      allocation: skuAllocation.value,
      source_id: f.source_id === '' ? null : Number(f.source_id),
      region: f.region?.trim() || null,
      traffic_gb: Number(f.traffic_gb) || 0,
      duration_days: Number(f.duration_days) || 30,
      max_thread_count: Number(f.max_thread_count) || 0,
      price: Number(f.price) || 0,
      cost_price: Number(f.cost_price) || 0,
      stock_total: Number(f.stock_total) || 0,
      is_active: !!f.is_active,
      sort_order: Number(f.sort_order) || 0,
    }
    if (skuEditing.value) await adminApi.updateProxySku(skuEditing.value.id, payload)
    else await adminApi.createProxySku(skuProductId.value, payload)
    toast.success('已保存')
    skuOpen.value = false
    await loadAll()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    skuSaving.value = false
  }
}

const removeSku = async (sku) => {
  if (!confirm(`删除规格「${sku.name}」？\n已售出或有人正在付款的规格不能删。`)) return
  try {
    await adminApi.deleteProxySku(sku.id)
    toast.success('已删除')
    await loadAll()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

/* ── socks 货源 ───────────────────────── */

const openImport = () => {
  importText.value = ''
  importOpen.value = true
}

const submitImport = async () => {
  if (!importText.value.trim()) {
    toast.error('请粘贴 socks 列表')
    return
  }
  importing.value = true
  try {
    const data = await adminApi.importProxySocks(importText.value)
    toast.success(`导入完成：新增 ${data?.created ?? 0}，重复 ${data?.skipped ?? 0}，无法识别 ${data?.invalid ?? 0}`)
    importOpen.value = false
    await loadAll()
  } catch (e) {
    toast.error(e.message || '导入失败')
  } finally {
    importing.value = false
  }
}

const openEditSock = (s) => {
  sockEditing.value = s
  sockForm.value = {
    name: s.name,
    host: s.host,
    port: s.port,
    username: s.username || '',
    password: '',
    country: s.country || '',
    note: s.note || '',
    // 后端还没返回 allocation 时按共享兜底：老线路本来就是多人共用
    allocation: s.allocation ?? 'shared',
    is_active: s.is_active,
  }
  sockOpen.value = true
}

const saveSock = async () => {
  const f = sockForm.value
  sockSaving.value = true
  try {
    const payload = {
      name: f.name?.trim() || `${f.host}:${f.port}`,
      host: f.host?.trim(),
      port: Number(f.port) || 0,
      username: f.username?.trim() || null,
      country: f.country?.trim() || null,
      note: f.note?.trim() || null,
      allocation: f.allocation === 'exclusive' ? 'exclusive' : 'shared',
      is_active: !!f.is_active,
    }
    // 密码留空 = 不修改
    if (f.password) payload.password = f.password
    await adminApi.updateProxySock(sockEditing.value.id, payload)
    toast.success('已保存')
    sockOpen.value = false
    await loadAll()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    sockSaving.value = false
  }
}

const removeSock = async (s) => {
  if (!confirm(`删除 socks「${s.host}:${s.port}」？\n被代理规格引用时不能删。`)) return
  try {
    await adminApi.deleteProxySock(s.id)
    toast.success('已删除')
    await loadAll()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

onMounted(() => {
  loadSettings()
  loadAll()
})
</script>

<template>
  <div class="ps">
    <div class="ps-head">
      <h1 class="ch-page-title">代理商品</h1>
      <Button variant="primary" size="sm" @click="openCreateProduct">
        <Icon name="plus" :size="16" /> 新建商品
      </Button>
    </div>
    <p class="ps-hint">
      这里卖的是代理（住宅 / 手工 socks），和节点套餐完全分开。Webshare 走独占账号（一人一个子账号，流量额度传给上游）；
      手工 socks 可选共享（多人共用一条，适合批量小套餐）或独占（一人一条，下单时从池子里取未售出的那条）。
    </p>

    <!-- 总开关：默认关闭，关闭时用户端不显示「代理」，用户接口也拒绝 -->
    <div class="ps-switch-bar" :class="{ 'is-on': storeEnabled }">
      <div class="ps-switch-text">
        <strong>{{ storeEnabled ? '代理商城已开启' : '代理商城已关闭（默认）' }}</strong>
        <span class="ps-dim">
          {{ storeEnabled
            ? '用户端侧栏会显示「代理」入口，可以下单购买'
            : '用户端不显示入口；直接访问 /dashboard/proxy 会被弹回仪表盘，接口也会拒绝下单' }}
        </span>
      </div>
      <Button
        :variant="storeEnabled ? 'secondary' : 'primary'"
        size="sm"
        :loading="storeToggling"
        @click="toggleStore"
      >
        {{ storeEnabled ? '关闭商城' : '开启商城' }}
      </Button>
    </div>

    <!-- 商品列表 -->
    <Card v-for="p in products" :key="p.id" class="ps-card" padding="false">
      <template #header>
        <div class="ps-card-head">
          <Icon name="package" :size="16" />
          <span>{{ p.name }}</span>
          <Badge :variant="p.is_active ? 'success' : 'warning'">{{ p.is_active ? '已上架' : '已下架' }}</Badge>
          <div class="ps-head-tools">
            <Button size="sm" variant="ghost" @click="openCreateSku(p)"><Icon name="plus" :size="14" /> 加规格</Button>
            <Button size="sm" variant="ghost" @click="openEditProduct(p)"><Icon name="edit" :size="15" /></Button>
            <Button size="sm" variant="ghost" @click="removeProduct(p)"><Icon name="trash" :size="15" /></Button>
          </div>
        </div>
      </template>

      <p v-if="p.subtitle" class="ps-sub">{{ p.subtitle }}</p>

      <div class="ps-table-wrap">
        <table class="ch-table ch-table--flat">
          <thead>
            <tr>
              <th>规格</th>
              <th>货源</th>
              <th>流量</th>
              <th>时长</th>
              <th>并发</th>
              <th>售价</th>
              <th>成本</th>
              <th>库存（可售/总）</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="sku in p.skus" :key="sku.id">
              <td class="ps-name">{{ sku.name }}<span v-if="sku.region" class="ps-region">{{ sku.region }}</span></td>
              <td class="ps-dim">{{ sourceLabel(sku) }}</td>
              <td class="mono">{{ sku.traffic_gb > 0 ? `${sku.traffic_gb} GB` : '不限' }}</td>
              <td class="mono">{{ sku.duration_days }} 天</td>
              <td class="mono">{{ sku.max_thread_count || '默认' }}</td>
              <td class="mono">¥{{ sku.price }}</td>
              <td class="mono ps-dim">¥{{ sku.cost_price }}</td>
              <td class="mono">
                <template v-if="skuAllocationOf(sku) === 'exclusive'">可用 {{ stockAvailable(sku) }} 条</template>
                <template v-else>
                  {{ stockAvailable(sku) }} / {{ sku.stock_total }}
                  <span v-if="sku.stock_locked" class="ps-dim">（占用 {{ sku.stock_locked }}）</span>
                </template>
              </td>
              <td>
                <Badge :variant="sku.is_active ? 'success' : 'warning'">{{ sku.is_active ? '启用' : '停用' }}</Badge>
              </td>
              <td>
                <div class="ch-actions-4">
                  <Button size="sm" variant="ghost" @click="openEditSku(sku)"><Icon name="edit" :size="15" /></Button>
                  <Button size="sm" variant="ghost" @click="removeSku(sku)"><Icon name="trash" :size="15" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!p.skus?.length">
              <td colspan="10" class="ch-empty-sm">还没有规格 —— 点右上「加规格」填价格和库存</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Card v-if="!products.length && !loading" class="ps-card">
      <p class="ch-empty-sm">还没有代理商品 —— 点右上角「新建商品」开始</p>
    </Card>

    <!-- 手工 socks 货源 -->
    <Card class="ps-card" padding="false">
      <template #header>
        <div class="ps-card-head">
          <Icon name="server" :size="16" />
          <span>手工 socks 货源</span>
          <span class="ps-count">{{ socks.length }} 条</span>
          <div class="ps-head-tools">
            <Button size="sm" variant="ghost" @click="openImport"><Icon name="plus" :size="14" /> 批量导入</Button>
            <Button size="sm" variant="ghost" @click="loadAll"><Icon name="refresh" :size="14" /> 刷新</Button>
          </div>
        </div>
      </template>

      <p class="ps-tip">
        一行一条，支持 <code>地址:端口:账号:密码:国家</code> 或 <code>socks5://账号:密码@地址:端口</code>；
        密码加密存储，列表不回显。
      </p>

      <div class="ps-table-wrap">
        <table class="ch-table ch-table--flat">
          <thead>
            <tr>
              <th>备注</th>
              <th>地址:端口</th>
              <th>账号</th>
              <th>密码</th>
              <th>国家</th>
              <th>分配方式</th>
              <th>已售给</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="s in socks" :key="s.id">
              <td class="ps-name">{{ s.name }}</td>
              <td class="mono">{{ s.host }}:{{ s.port }}</td>
              <td class="mono">{{ s.username || '—' }}</td>
              <td class="ps-dim">{{ s.has_password ? '已设置' : '无' }}</td>
              <td class="ps-dim">{{ s.country || '—' }}</td>
              <td class="ps-dim">{{ sockAllocLabel(s) }}</td>
              <td class="ps-dim" :title="s.sold_at || ''">{{ sockSoldTo(s) }}</td>
              <td>
                <Badge :variant="s.is_active ? 'success' : 'warning'">{{ s.is_active ? '启用' : '停用' }}</Badge>
              </td>
              <td>
                <div class="ch-actions-4">
                  <Button size="sm" variant="ghost" @click="openEditSock(s)">编辑</Button>
                  <Button size="sm" variant="ghost" @click="removeSock(s)"><Icon name="trash" :size="15" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!socks.length">
              <td colspan="9" class="ch-empty-sm">还没有 socks 货源 —— 点右上「批量导入」粘贴</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 商品弹窗 -->
    <Modal v-model="productOpen" :title="productEditing ? '编辑商品' : '新建代理商品'" width="520px">
      <div class="ch-form">
        <label class="ps-field">
          <span class="ps-label">商品名（对外展示）</span>
          <Input v-model="productForm.name" placeholder="例如：美国住宅代理" maxlength="100" />
        </label>
        <label class="ps-field">
          <span class="ps-label">一句话卖点（可选）</span>
          <Input v-model="productForm.subtitle" placeholder="例如：固定美国出口 IP，适合电商养号" maxlength="200" />
        </label>
        <label class="ps-field">
          <span class="ps-label">详情（可选）</span>
          <textarea v-model="productForm.description" class="ps-textarea" rows="3" maxlength="5000" />
        </label>
        <label class="ps-field">
          <span class="ps-label">排序（小的在前）</span>
          <Input v-model.number="productForm.sort_order" type="number" min="0" />
        </label>
        <label class="ps-check">
          <input v-model="productForm.is_active" type="checkbox" />
          <span>上架（用户端可见）</span>
        </label>
      </div>
      <template #footer>
        <div class="ps-foot">
          <Button variant="ghost" size="sm" @click="productOpen = false">取消</Button>
          <Button variant="primary" size="sm" :loading="productSaving" @click="saveProduct">保存</Button>
        </div>
      </template>
    </Modal>

    <!-- 规格弹窗 -->
    <Modal v-model="skuOpen" :title="skuEditing ? '编辑规格' : '新增规格'" width="560px">
      <div class="ch-form">
        <label class="ps-field">
          <span class="ps-label">规格名</span>
          <Input v-model="skuForm.name" placeholder="例如：100G / 30天" maxlength="100" />
        </label>

        <div class="ps-grid">
          <label class="ps-field">
            <span class="ps-label">货源 / 分配方式</span>
            <Select v-model="skuForm.source_type" :options="sourceOptions" />
          </label>
          <label class="ps-field">
            <span class="ps-label">{{ skuForm.source_type === 'socks' ? '选择 socks' : '用哪个 Webshare 账号' }}</span>
            <Select
              v-model="skuForm.source_id"
              :options="skuForm.source_type === 'socks' ? socksOptions : providerOptions"
            />
          </label>
        </div>

        <!-- 分配方式只在 socks 下可选（webshare 一人一个子账号，天生独占） -->
        <label v-if="skuForm.source_type === 'socks'" class="ps-field">
          <span class="ps-label">分配方式（一条线路卖给几个人）</span>
          <Select v-model="skuForm.allocation" :options="allocationOptions" />
          <p class="ps-tip">
            <template v-if="skuAllocation === 'exclusive'">
              独占模式：卖出后自动从池子里取未售出的一条，池空则无法下单。货源选「不指定」即可，不用手工挑。
            </template>
            <template v-else>
              共享模式：所有买家共用同一条线路，库存 = 你填的可售份数，不限流量。
            </template>
          </p>
        </label>

        <div class="ps-grid">
          <label class="ps-field">
            <span class="ps-label">地区（US/GB，空 = 不限）</span>
            <Input v-model="skuForm.region" placeholder="US" maxlength="32" />
          </label>
          <label class="ps-field">
            <span class="ps-label">流量额度 GB（0 = 不限）</span>
            <Input v-model.number="skuForm.traffic_gb" type="number" min="0" />
          </label>
        </div>

        <div class="ps-grid">
          <label class="ps-field">
            <span class="ps-label">有效期（天）</span>
            <Input v-model.number="skuForm.duration_days" type="number" min="1" />
          </label>
          <label class="ps-field">
            <span class="ps-label">并发线程（0 = 上游默认）</span>
            <Input v-model.number="skuForm.max_thread_count" type="number" min="0" />
          </label>
        </div>

        <div class="ps-grid">
          <label class="ps-field">
            <span class="ps-label">售价（元）</span>
            <Input v-model.number="skuForm.price" type="number" min="0" step="0.01" />
          </label>
          <label class="ps-field">
            <span class="ps-label">成本价（元，仅内部看）</span>
            <Input v-model.number="skuForm.cost_price" type="number" min="0" step="0.01" />
          </label>
        </div>

        <label class="ps-field">
          <span class="ps-label">可售份数</span>
          <Input v-model.number="skuForm.stock_total" type="number" min="0" />
          <p class="ps-tip">
            webshare 独占型 = 上游还能开几个子账号（现在是「可开几个」）；socks 共享型 = 你打算卖几份。
          </p>
        </label>

        <label class="ps-check">
          <input v-model="skuForm.is_active" type="checkbox" />
          <span>启用该规格</span>
        </label>
      </div>
      <template #footer>
        <div class="ps-foot">
          <Button variant="ghost" size="sm" @click="skuOpen = false">取消</Button>
          <Button variant="primary" size="sm" :loading="skuSaving" @click="saveSku">保存</Button>
        </div>
      </template>
    </Modal>

    <!-- 导入 socks -->
    <Modal v-model="importOpen" title="批量导入 socks" width="560px">
      <div class="ch-form">
        <label class="ps-field">
          <span class="ps-label">粘贴列表（一行一条）</span>
          <textarea
            v-model="importText"
            class="ps-textarea mono"
            rows="8"
            placeholder="9.142.31.145:5303:youzadmin:youzadmin:US
socks5://user2:pass2@1.2.3.4:1080"
          />
        </label>
        <p class="ps-tip">重复的（地址+端口+账号相同）会自动跳过；识别不了的行会在结果里告诉你第几行。</p>
      </div>
      <template #footer>
        <div class="ps-foot">
          <Button variant="ghost" size="sm" @click="importOpen = false">取消</Button>
          <Button variant="primary" size="sm" :loading="importing" @click="submitImport">导入</Button>
        </div>
      </template>
    </Modal>

    <!-- 编辑 socks -->
    <Modal v-model="sockOpen" title="编辑 socks" width="520px">
      <div class="ch-form">
        <div class="ps-grid">
          <label class="ps-field">
            <span class="ps-label">备注名</span>
            <Input v-model="sockForm.name" maxlength="100" />
          </label>
          <label class="ps-field">
            <span class="ps-label">国家</span>
            <Input v-model="sockForm.country" placeholder="US" maxlength="16" />
          </label>
        </div>
        <div class="ps-grid">
          <label class="ps-field">
            <span class="ps-label">地址</span>
            <Input v-model="sockForm.host" maxlength="190" />
          </label>
          <label class="ps-field">
            <span class="ps-label">端口</span>
            <Input v-model.number="sockForm.port" type="number" min="1" max="65535" />
          </label>
        </div>
        <div class="ps-grid">
          <label class="ps-field">
            <span class="ps-label">账号</span>
            <Input v-model="sockForm.username" maxlength="190" />
          </label>
          <label class="ps-field">
            <span class="ps-label">密码（留空 = 不修改）</span>
            <Input v-model="sockForm.password" type="password" />
          </label>
        </div>
        <div class="ps-field">
          <span class="ps-label">分配方式</span>
          <div class="ps-radios">
            <label class="ps-check">
              <input v-model="sockForm.allocation" type="radio" value="shared" />
              <span>共享（多人共用一条）</span>
            </label>
            <label class="ps-check">
              <input v-model="sockForm.allocation" type="radio" value="exclusive" />
              <span>独占（一人一条）</span>
            </label>
          </div>
          <p class="ps-tip">
            共享：多个买家共用这条线路，适合批量小套餐；独占：一条只卖一个人，卖出后会记下买家。
          </p>
        </div>
        <label class="ps-check">
          <input v-model="sockForm.is_active" type="checkbox" />
          <span>启用</span>
        </label>
      </div>
      <template #footer>
        <div class="ps-foot">
          <Button variant="ghost" size="sm" @click="sockOpen = false">取消</Button>
          <Button variant="primary" size="sm" :loading="sockSaving" @click="saveSock">保存</Button>
        </div>
      </template>
    </Modal>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/ProxyStore.css"></style>
