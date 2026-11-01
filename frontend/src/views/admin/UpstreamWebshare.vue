<script setup>
/**
 * Admin 上游 · Webshare：把站长自己的 Webshare 账号接入为代理上游。
 *
 * 三块：
 *   1. 账号凭据   —— 多个 provider，Token 加密存储，只回 has_token；可测试连通性
 *   2. 套餐同步   —— 只展示 status=active 的套餐（上游会把已取消套餐也列出来，
 *                    且它们的 subusers_available 是假数字，后端已过滤）
 *   3. 用户代理   —— 开号（建子用户）、停用（改密码 + 降配额）、用量回采
 *
 * 关键约束（Webshare 实际接口行为，2026-10 实测）：
 *   - 子用户 API 需站长在 proxy.webshare.io/subuser/ 申请开通，否则 403
 *   - 初始密码**只在建号那一次响应里给**（只读详情不返回凭据），关掉弹窗就没了；
 *     停用会重置密码，新密码同样只显示一次
 *   - 子用户没有 enable/disable 开关：停用 = 改密码 + proxy_limit 压到 0.01 GB
 *   - 用量口径是 aggregate_stats.bandwidth_total（字节），按主账号账单周期统计
 */
import { ref, computed, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const providers = ref([])
const selectedId = ref(null)
const plans = ref([])
const accounts = ref([])

const loadingProviders = ref(false)
const loadingPlans = ref(false)
const loadingAccounts = ref(false)
const testing = ref(false)
const pullingAll = ref(false)
// 「同步上游」进行中（把 Webshare 后台已有的子用户拉回本地）
const syncing = ref(false)

// 代理 IP（出口）：来自上游 /proxy/list/，只展示 IP/端口/国家
const proxies = ref([])
const loadingProxies = ref(false)
const proxyPlanId = ref('')

const planFilterOptions = computed(() => [
  { label: '全部套餐', value: '' },
  ...plans.value.map((p) => ({
    label: `#${p.id} ${p.proxy_type ?? ''} ${p.bandwidth_limit ?? ''}GB`.trim(),
    value: p.id,
  })),
])

const selectedProvider = computed(() => providers.value.find((p) => p.id === selectedId.value) || null)

// 账号弹窗（新建 / 编辑共用；token 留空 = 不修改）
const providerOpen = ref(false)
const providerEditing = ref(null)
const providerForm = ref({ name: '', api_token: '', enabled: true })
const providerSaving = ref(false)
const providerSaved = ref(false)

// 开号弹窗
const accountOpen = ref(false)
const accountForm = ref({ label: '', proxy_limit: 5, max_thread_count: 50, user_id: '' })
const accountSaving = ref(false)
const accountSaved = ref(false)

// 凭据一次性展示（建号初始密码 / 停用后的新密码）
const credentialOpen = ref(false)
const credential = ref({ title: '', username: '', password: '', tip: '' })

const statusMeta = {
  success: { label: '连接正常', variant: 'success' },
  failed: { label: '连接失败', variant: 'danger' },
  pending: { label: '未测试', variant: 'warning' },
}

const accountStatusMeta = {
  active: { label: '使用中', variant: 'success' },
  disabled: { label: '已停用', variant: 'warning' },
  deleted: { label: '已删除', variant: 'danger' },
}

const fmtGB = (bytes) => {
  const n = Number(bytes || 0)
  if (!n) return '0 GB'
  return `${(n / 1024 ** 3).toFixed(2)} GB`
}

const fmtLimit = (gb) => {
  const n = Number(gb || 0)
  return n > 0 ? `${n} GB` : '不限量'
}

const fmtDate = (iso) => (iso ? String(iso).replace('T', ' ').slice(0, 16) : '—')

const countriesText = (map) => {
  if (!map || typeof map !== 'object') return '—'
  const keys = Object.keys(map)
  if (!keys.length) return '—'
  return keys.map((k) => `${k === 'ZZ' ? '全球' : k} ${map[k]}`).join(' / ')
}

/* ── provider ─────────────────────────────────────────── */

const loadProviders = async () => {
  loadingProviders.value = true
  try {
    const data = await adminApi.listWebshareProviders()
    providers.value = data?.items || []
    // 选中的账号被删掉时回落到第一个
    if (!providers.value.some((p) => p.id === selectedId.value)) {
      selectedId.value = providers.value[0]?.id ?? null
    }
  } catch (e) {
    providers.value = []
    toast.error(e.message || '加载账号失败')
  } finally {
    loadingProviders.value = false
  }
}

const selectProvider = async (p) => {
  if (selectedId.value === p.id) return
  selectedId.value = p.id
  plans.value = []
  accounts.value = []
  proxies.value = []
  proxyPlanId.value = ''
  await Promise.all([loadPlans(), loadAccounts(), loadProxies()])
}

const openCreateProvider = () => {
  providerEditing.value = null
  providerForm.value = { name: '', api_token: '', enabled: true }
  providerOpen.value = true
}

const openEditProvider = (p) => {
  providerEditing.value = p
  providerForm.value = { name: p.name, api_token: '', enabled: !!p.enabled }
  providerOpen.value = true
}

const saveProvider = async () => {
  const name = providerForm.value.name.trim()
  if (!name) {
    toast.error('请填写备注名')
    return
  }
  const token = providerForm.value.api_token.trim()
  if (!providerEditing.value && !token) {
    toast.error('请填写 API Token')
    return
  }
  providerSaving.value = true
  try {
    if (providerEditing.value) {
      const payload = { name, enabled: providerForm.value.enabled }
      // 留空 = 保持原 token 不变（后端 sometimes 校验，不传就不动）
      if (token) payload.api_token = token
      await adminApi.updateWebshareProvider(providerEditing.value.id, payload)
      toast.success('已保存')
    } else {
      const created = await adminApi.createWebshareProvider({ name, api_token: token, enabled: providerForm.value.enabled })
      toast.success('账号已添加')
      await loadProviders()
      if (created?.id) {
        selectedId.value = created.id
        await Promise.all([loadPlans(), loadAccounts(), loadProxies()])
      }
    }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    providerSaved.value = true
    setTimeout(() => {
      providerSaved.value = false
      providerOpen.value = false
    }, 900)
    await loadProviders()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    providerSaving.value = false
  }
}

const testProvider = async (p) => {
  testing.value = true
  try {
    const data = await adminApi.testWebshareProvider(p.id)
    toast.success(`连接正常，上游现有 ${data?.subuser_count ?? 0} 个子用户`)
    await loadProviders()
  } catch (e) {
    toast.error(e.message || '连接失败')
    await loadProviders()
  } finally {
    testing.value = false
  }
}

const removeProvider = async (p) => {
  if (!confirm(`删除账号「${p.name}」？\n只删本地记录和它下面的子用户记录，不会删除 Webshare 侧的子用户。`)) return
  try {
    await adminApi.deleteWebshareProvider(p.id)
    toast.success('已删除')
    if (selectedId.value === p.id) {
      selectedId.value = null
      plans.value = []
      accounts.value = []
    }
    await loadProviders()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

/* ── 套餐 ─────────────────────────────────────────────── */

const loadPlans = async () => {
  if (!selectedId.value) return
  loadingPlans.value = true
  try {
    const data = await adminApi.listWebsharePlans(selectedId.value)
    plans.value = data?.items || []
  } catch (e) {
    plans.value = []
    toast.error(e.message || '套餐同步失败')
  } finally {
    loadingPlans.value = false
  }
}

/* ── 子用户 ───────────────────────────────────────────── */

const loadAccounts = async () => {
  if (!selectedId.value) return
  loadingAccounts.value = true
  try {
    const data = await adminApi.listWebshareAccounts(selectedId.value)
    accounts.value = data?.items || []
  } catch (e) {
    accounts.value = []
    toast.error(e.message || '加载子用户失败')
  } finally {
    loadingAccounts.value = false
  }
}

/**
 * 拉代理 IP（出口）。只展示 IP / 端口 / 国家 —— 用户拿子用户账号密码 + 这里的 IP:端口 去连，
 * 主账号的代理凭据不经过面板。
 */
const loadProxies = async () => {
  if (!selectedId.value) return
  loadingProxies.value = true
  try {
    const params = proxyPlanId.value ? { plan_id: proxyPlanId.value } : {}
    const data = await adminApi.listWebshareProxies(selectedId.value, params)
    proxies.value = data?.items || []
  } catch (e) {
    proxies.value = []
    toast.error(e.message || '代理 IP 拉取失败')
  } finally {
    loadingProxies.value = false
  }
}

const copyProxy = async (p) => {
  try {
    await navigator.clipboard.writeText(`${p.address}:${p.port}`)
    toast.success('已复制')
  } catch (e) {
    toast.error('复制失败，请手动选择文本复制')
  }
}

const openCreateAccount = () => {
  if (!selectedId.value) {
    toast.error('请先添加并选中一个账号')
    return
  }
  accountForm.value = { label: '', proxy_limit: 5, max_thread_count: 50, user_id: '' }
  accountOpen.value = true
}

/**
 * 把上游已有子用户拉回本地。
 * 适用：站长在 Webshare 后台直接建过号、或换机器后本地表是空的。
 * 同步回来的记录没有凭据（上游只读接口不给 password），要交付给用户得先停用重置。
 */
const syncAccounts = async () => {
  if (!selectedId.value) return
  syncing.value = true
  try {
    const data = await adminApi.syncWebshareAccounts(selectedId.value)
    const created = data?.created ?? 0
    const updated = data?.updated ?? 0
    if (!created && !updated) {
      toast.info('上游没有子用户')
    } else {
      toast.success(`已同步：新增 ${created} 个，更新 ${updated} 个`)
    }
    await Promise.all([loadAccounts(), loadProviders()])
  } catch (e) {
    toast.error(e.message || '同步失败')
  } finally {
    syncing.value = false
  }
}

const saveAccount = async () => {
  const f = accountForm.value
  const label = String(f.label || '').trim()
  if (!label) {
    toast.error('请填写账号标识（label）')
    return
  }
  accountSaving.value = true
  try {
    const payload = {
      label,
      proxy_limit: Number(f.proxy_limit) || 0,
      max_thread_count: Number(f.max_thread_count) || 0,
    }
    if (String(f.user_id || '').trim()) payload.user_id = Number(f.user_id)
    const created = await adminApi.createWebshareAccount(selectedId.value, payload)
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再切到「初始密码只显示一次」的凭据弹窗
    accountSaved.value = true
    setTimeout(() => {
      accountSaved.value = false
      accountOpen.value = false
      // 初始密码只在建号响应里给一次，必须当场展示
      credential.value = {
        title: '子用户已创建',
        username: created?.username || '',
        password: created?.initial_password || '',
        tip: '初始密码只显示这一次（关闭后无法再查看，只能停用重置）。请现在复制并交付给用户。',
      }
      credentialOpen.value = true
    }, 900)
    await Promise.all([loadAccounts(), loadProviders()])
  } catch (e) {
    toast.error(e.message || '开号失败')
  } finally {
    accountSaving.value = false
  }
}

const disableAccount = async (a) => {
  if (!confirm(`停用「${a.label}」？\n会把上游密码改成随机串、配额压到 0.01 GB，用户手上的旧凭据立即失效。`)) return
  try {
    const data = await adminApi.disableWebshareAccount(a.id)
    credential.value = {
      title: '子用户已停用',
      username: data?.username || a.username || '',
      password: data?.new_password || '',
      tip: '这是上游重置后的新密码，只显示这一次。要恢复使用需把它交付给用户。',
    }
    credentialOpen.value = true
    await loadAccounts()
  } catch (e) {
    toast.error(e.message || '停用失败')
  }
}

const removeAccount = async (a) => {
  if (!confirm(`删除子用户「${a.label}」？\n会同时删除 Webshare 侧的子用户，不可恢复。`)) return
  try {
    await adminApi.deleteWebshareAccount(a.id)
    toast.success('已删除')
    await Promise.all([loadAccounts(), loadProviders()])
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

const pullUsage = async (a) => {
  try {
    const data = await adminApi.pullWebshareUsage(a.id)
    toast.success(`${a.label} 已回采：${fmtGB(data?.used_bytes)}`)
    await loadAccounts()
  } catch (e) {
    toast.error(e.message || '回采用量失败')
  }
}

const pullUsageAll = async () => {
  if (!selectedId.value) return
  pullingAll.value = true
  try {
    const data = await adminApi.pullWebshareUsageBatch(selectedId.value)
    const failed = Object.keys(data?.failed || {}).length
    if (failed) toast.error(`已回采 ${data?.updated ?? 0} 个，${failed} 个失败`)
    else toast.success(`已回采 ${data?.updated ?? 0} 个子用户用量`)
    await loadAccounts()
  } catch (e) {
    toast.error(e.message || '批量回采失败')
  } finally {
    pullingAll.value = false
  }
}

/**
 * 读取本地保存的子用户凭据。
 * 上游没有「读密码」接口（只读详情不返凭据），这里读的是我们建号/停用时加密存的副本；
 * 从上游同步来的记录没有副本，接口会 404 并提示去停用重置。
 */
const viewCredential = async (a) => {
  try {
    const data = await adminApi.getWebshareCredential(a.id)
    credential.value = {
      title: `子用户凭据 · ${data?.label || a.label}`,
      username: data?.username || '',
      password: data?.password || '',
      tip: '这是本面板建号/重置时保存的凭据（加密存储）。上游不提供读密码接口，请交付后自行妥善保管。',
    }
    credentialOpen.value = true
  } catch (e) {
    toast.error(e.message || '读取凭据失败')
  }
}

const copyCredential = async () => {
  const text = `服务器/代理地址见 Webshare 后台\n用户名：${credential.value.username}\n密码：${credential.value.password}`
  try {
    await navigator.clipboard.writeText(text)
    toast.success('已复制')
  } catch (e) {
    toast.error('复制失败，请手动选择文本复制')
  }
}

onMounted(async () => {
  await loadProviders()
  if (selectedId.value) await Promise.all([loadPlans(), loadAccounts(), loadProxies()])
})
</script>

<template>
  <div class="ws">
    <div class="ws-head">
      <h1 class="ws-title">Webshare</h1>
      <Button variant="primary" size="sm" @click="openCreateProvider">
        <Icon name="plus" :size="16" /> 添加账号
      </Button>
    </div>

    <p class="ws-hint">
      用你自己的 Webshare 账号作为代理上游：给用户下发独立代理凭据，按 GB 限流量、按并发限线程、按国家分 IP 池。
      代理不是翻墙节点，它给的是固定国家的出口 IP（电商、比价、养号这类用途）。
    </p>

    <!-- 账号列表 -->
    <Card class="ws-card" padding="false">
      <template #header>
        <div class="ws-card-head">
          <Icon name="lock" :size="16" />
          <span>账号凭据</span>
          <span class="ws-count">{{ providers.length }} 个</span>
        </div>
      </template>

      <div class="ws-table-wrap">
        <table class="ws-table">
          <thead>
            <tr>
              <th>备注名</th>
              <th>状态</th>
              <th>Token</th>
              <th>子用户</th>
              <th>最近检查</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="p in providers"
              :key="p.id"
              :class="{ 'is-selected': p.id === selectedId }"
              @click="selectProvider(p)"
            >
              <td class="ws-name">
                {{ p.name }}
                <Badge v-if="p.id === selectedId" variant="info">当前</Badge>
              </td>
              <td>
                <Badge :variant="statusMeta[p.last_status]?.variant || 'warning'" dot>
                  {{ statusMeta[p.last_status]?.label || p.last_status }}
                </Badge>
                <p v-if="p.last_error" class="ws-error" :title="p.last_error">{{ p.last_error }}</p>
              </td>
              <td>{{ p.has_token ? '已配置' : '未配置' }}</td>
              <td class="mono">{{ p.account_count }}</td>
              <td class="ws-dim">{{ fmtDate(p.last_checked_at) }}</td>
              <td>
                <div class="ws-actions">
                  <Button size="sm" variant="ghost" :loading="testing" @click.stop="testProvider(p)">测试连接</Button>
                  <Button size="sm" variant="ghost" @click.stop="openEditProvider(p)"><Icon name="edit" :size="15" /></Button>
                  <Button size="sm" variant="ghost" @click.stop="removeProvider(p)"><Icon name="trash" :size="15" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!providers.length && !loadingProviders">
              <td colspan="6" class="ws-empty">还没有账号 —— 点右上角「添加账号」填入 Webshare API Token</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 套餐 -->
    <Card class="ws-card" padding="false">
      <template #header>
        <div class="ws-card-head">
          <Icon name="package" :size="16" />
          <span>套餐</span>
          <Button size="sm" variant="ghost" :disabled="!selectedId || loadingPlans" @click="loadPlans">
            <Icon name="refresh" :size="14" /> {{ loadingPlans ? '同步中...' : '同步套餐' }}
          </Button>
        </div>
      </template>

      <div class="ws-table-wrap">
        <table class="ws-table">
          <thead>
            <tr>
              <th>套餐 ID</th>
              <th>类型</th>
              <th>带宽</th>
              <th>代理数</th>
              <th>国家池</th>
              <th>子用户额度</th>
              <th>月价</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="plan in plans" :key="plan.id">
              <td class="mono">{{ plan.id }}</td>
              <td>{{ plan.proxy_type }}{{ plan.proxy_subtype ? ` / ${plan.proxy_subtype}` : '' }}</td>
              <td class="mono">{{ fmtLimit(plan.bandwidth_limit) }}</td>
              <td class="mono">{{ plan.proxy_count ?? '—' }}</td>
              <td class="ws-dim">{{ countriesText(plan.countries) }}</td>
              <td>
                <span class="mono">可开 {{ plan.subusers_available ?? '—' }}</span>
                <span v-if="plan.subusers_available === 0" class="ws-warn">（本套餐开不了子用户）</span>
              </td>
              <td class="mono">{{ plan.monthly_price != null ? `$${plan.monthly_price}` : '—' }}</td>
            </tr>
            <tr v-if="!plans.length && !loadingPlans">
              <td colspan="7" class="ws-empty">
                {{ selectedId ? '没有可用的套餐（只显示 status=active 的套餐）' : '先在上方选择一个账号' }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 代理 IP（出口） -->
    <Card class="ws-card" padding="false">
      <template #header>
        <div class="ws-card-head">
          <Icon name="globe" :size="16" />
          <span>代理 IP（出口）</span>
          <span class="ws-count">{{ proxies.length }} 个</span>
          <div class="ws-head-tools">
            <Select v-model="proxyPlanId" :options="planFilterOptions" size="sm" @change="loadProxies" />
          </div>
          <Button size="sm" variant="ghost" :disabled="!selectedId || loadingProxies" @click="loadProxies">
            <Icon name="refresh" :size="14" /> {{ loadingProxies ? '拉取中...' : '刷新' }}
          </Button>
        </div>
      </template>

      <p class="ws-hint ws-hint--inline">
        用户用「子用户的用户名 / 密码 + 这里的 IP:端口」连接；主账号的代理凭据不会出现在面板里。
      </p>

      <div class="ws-table-wrap">
        <table class="ws-table">
          <thead>
            <tr>
              <th>地址:端口</th>
              <th>国家</th>
              <th>城市</th>
              <th>ASN</th>
              <th>状态</th>
              <th>最近校验</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in proxies" :key="p.id">
              <td class="mono">{{ p.address }}:{{ p.port }}</td>
              <td>{{ p.country || '—' }}</td>
              <td class="ws-dim">{{ p.city || '—' }}</td>
              <td class="ws-dim ws-asn" :title="p.asn || ''">{{ p.asn || '—' }}</td>
              <td>
                <Badge :variant="p.valid ? 'success' : 'danger'">{{ p.valid ? '可用' : '不可用' }}</Badge>
              </td>
              <td class="ws-dim">{{ fmtDate(p.last_verification) }}</td>
              <td>
                <CopyButton :get-text="() => `${p.address}:${p.port}`" label="复制地址" />
              </td>
            </tr>
            <tr v-if="!proxies.length && !loadingProxies">
              <td colspan="7" class="ws-empty">
                {{ selectedId ? '没有拉到代理 IP（套餐未分配代理，或上游暂无可用 IP）' : '先在上方选择一个账号' }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 子用户 -->
    <Card class="ws-card" padding="false">
      <template #header>
        <div class="ws-card-head">
          <Icon name="users" :size="16" />
          <span>子用户（用户代理凭据）</span>
          <Button size="sm" variant="ghost" :disabled="!selectedId" @click="openCreateAccount">
            <Icon name="plus" :size="14" /> 开号
          </Button>
          <Button size="sm" variant="ghost" :disabled="!selectedId || syncing" @click="syncAccounts">
            <Icon name="refresh" :size="14" /> {{ syncing ? '同步中...' : '同步上游' }}
          </Button>
          <Button size="sm" variant="ghost" :disabled="!selectedId || pullingAll" @click="pullUsageAll">
            <Icon name="refresh" :size="14" /> {{ pullingAll ? '回采中...' : '回采全部用量' }}
          </Button>
        </div>
      </template>

      <div class="ws-table-wrap">
        <table class="ws-table">
          <thead>
            <tr>
              <th>标识</th>
              <th>上游 ID</th>
              <th>用户名</th>
              <th>平台用户</th>
              <th>额度 / 线程</th>
              <th>已用</th>
              <th>到期</th>
              <th>状态</th>
              <th>最近同步</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="a in accounts" :key="a.id">
              <td class="ws-name">{{ a.label }}</td>
              <td class="mono ws-dim">{{ a.upstream_id }}</td>
              <td class="mono" :title="a.username ? '' : '上游只读接口不返回凭据；要交付给用户需先停用重置密码'">
                {{ a.username || '凭据未知' }}
              </td>
              <td class="ws-dim">{{ a.user_id ? `#${a.user_id}` : '未绑定' }}</td>
              <td class="mono">{{ fmtLimit(a.proxy_limit) }} / {{ a.max_thread_count || '—' }}</td>
              <td class="mono">{{ fmtGB(a.used_bytes) }}</td>
              <td class="ws-dim">{{ a.expires_at ? a.expires_at.slice(0, 10) : '—' }}</td>
              <td>
                <Badge :variant="accountStatusMeta[a.status]?.variant || 'info'">
                  {{ accountStatusMeta[a.status]?.label || a.status }}
                </Badge>
              </td>
              <td class="ws-dim">{{ fmtDate(a.last_synced_at) }}</td>
              <td>
                <div class="ws-actions">
                  <Button size="sm" variant="ghost" @click="viewCredential(a)">凭据</Button>
                  <Button size="sm" variant="ghost" @click="pullUsage(a)">回采</Button>
                  <Button v-if="a.status === 'active'" size="sm" variant="ghost" @click="disableAccount(a)">停用</Button>
                  <Button size="sm" variant="ghost" @click="removeAccount(a)"><Icon name="trash" :size="15" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!accounts.length && !loadingAccounts">
              <td colspan="9" class="ws-empty">{{ selectedId ? '该账号下还没有子用户' : '先在上方选择一个账号' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 前置条件 -->
    <Card class="ws-card">
      <template #header>
        <div class="ws-card-head">
          <Icon name="warning" :size="16" />
          <span>接入前置条件</span>
        </div>
      </template>

      <ul class="ws-list">
        <li>
          必须在
          <a class="ws-link" href="https://proxy.webshare.io/subuser/" target="_blank" rel="noopener">proxy.webshare.io/subuser</a>
          申请开通子用户 API 权限，未开通时接口一律 403。
        </li>
        <li>套餐自带 3 个免费子用户，第 4 个起需在 Webshare 侧加购，这是站长的成本项；套餐卡片里的「可开 N 个」是上游实时额度。</li>
        <li>子用户没有启用/禁用开关：停用 = 改密码 + 把配额压到 0.01 GB，恢复要重新交付新密码。</li>
        <li>建号返回的初始密码<b>只显示一次</b>（上游只读接口不返回凭据），关掉弹窗后只能停用重置。</li>
        <li>用量按主账号账单周期统计，与自己的售卖周期不同步时需要单独对齐。</li>
      </ul>
    </Card>

    <!-- 账号弹窗 -->
    <Modal v-model="providerOpen" :title="providerEditing ? `编辑账号 ${providerEditing.name}` : '添加 Webshare 账号'" width="480px">
      <div class="ws-form">
        <div class="ws-field">
          <label class="ws-label">备注名</label>
          <Input v-model="providerForm.name" placeholder="例如：主账号 / 备用账号" maxlength="64" />
        </div>
        <div class="ws-field">
          <label class="ws-label">API Token{{ providerEditing ? '（留空 = 不修改）' : '' }}</label>
          <Input v-model="providerForm.api_token" type="password" mono placeholder="在 proxy.webshare.io/userapi/keys 生成" maxlength="512" />
          <p class="ws-tip">加密存储，接口不回明文；调用走 <code>Authorization: Token</code>。</p>
        </div>
        <label class="ws-check">
          <input v-model="providerForm.enabled" type="checkbox" />
          <span>启用该账号</span>
        </label>
      </div>
      <template #footer>
        <div class="ws-foot">
          <Button variant="ghost" size="sm" @click="providerOpen = false">取消</Button>
          <Button variant="primary" size="sm" :loading="providerSaving" :success="providerSaved" @click="saveProvider">保存</Button>
        </div>
      </template>
    </Modal>

    <!-- 开号弹窗 -->
    <Modal v-model="accountOpen" title="开子用户" width="480px">
      <div class="ws-form">
        <div class="ws-field">
          <label class="ws-label">标识（label，上游展示用）</label>
          <Input v-model="accountForm.label" placeholder="例如：用户 7 的代理" maxlength="64" />
        </div>
        <div class="ws-field">
          <label class="ws-label">流量额度（GB，0 = 不限量）</label>
          <Input v-model.number="accountForm.proxy_limit" type="number" min="0" step="1" />
        </div>
        <div class="ws-field">
          <label class="ws-label">并发线程数</label>
          <Input v-model.number="accountForm.max_thread_count" type="number" min="0" step="10" />
          <p class="ws-tip">上游 max_thread_count，0 = 用账号默认值。</p>
        </div>
        <div class="ws-field">
          <label class="ws-label">绑定平台用户 ID（可选）</label>
          <Input v-model="accountForm.user_id" placeholder="留空 = 暂不绑定" />
          <p class="ws-tip">填了会把这条代理归到该用户名下，后续在用户侧展示。</p>
        </div>
      </div>
      <template #footer>
        <div class="ws-foot">
          <Button variant="ghost" size="sm" @click="accountOpen = false">取消</Button>
          <Button variant="primary" size="sm" :loading="accountSaving" :success="accountSaved" @click="saveAccount">开号</Button>
        </div>
      </template>
    </Modal>

    <!-- 凭据一次性展示 -->
    <Modal v-model="credentialOpen" :title="credential.title" width="480px">
      <div class="ws-form">
        <div class="ws-cred">
          <div class="ws-cred-row">
            <span class="ws-cred-key">用户名</span>
            <code>{{ credential.username || '—' }}</code>
          </div>
          <div class="ws-cred-row">
            <span class="ws-cred-key">密码</span>
            <code>{{ credential.password || '—' }}</code>
          </div>
        </div>
        <p class="ws-tip">{{ credential.tip }}</p>
      </div>
      <template #footer>
        <div class="ws-foot">
          <Button variant="ghost" size="sm" @click="credentialOpen = false">关闭</Button>
          <Button variant="primary" size="sm" @click="copyCredential">复制凭据</Button>
        </div>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.ws { padding: var(--space-1) 0 var(--space-6); max-width: 1100px; }
.ws-head {
  display: flex; align-items: center; justify-content: space-between;
  margin-bottom: var(--space-3); gap: var(--space-2); flex-wrap: wrap;
}
.ws-title { margin: 0; font-size: var(--text-xl); font-weight: var(--font-bold); }
.ws-hint {
  margin: 0 0 var(--space-4); color: var(--text-secondary); font-size: var(--text-sm);
  max-width: 760px; line-height: 1.6;
}
.ws-card { margin-bottom: var(--space-4); }
.ws-card-head {
  display: flex; align-items: center; gap: var(--space-2);
  font-size: var(--text-sm); font-weight: var(--font-bold); width: 100%;
}
.ws-card-head button { margin-left: auto; }
.ws-card-head button + button { margin-left: 0; }
.ws-count { color: var(--text-muted); font-weight: 400; font-size: var(--text-xs); }
/* header 里的筛选控件：靠右、限宽 */
.ws-head-tools { margin-left: auto; width: 200px; }
.ws-hint--inline {
  margin: 0; padding: var(--space-3) var(--space-4) 0;
  font-size: var(--text-xs); color: var(--text-muted); max-width: none;
}
.ws-asn { max-width: 220px; overflow: hidden; text-overflow: ellipsis; }

.ws-table-wrap { overflow-x: auto; }
.ws-table { width: 100%; border-collapse: collapse; font-size: var(--text-sm); }
.ws-table th, .ws-table td {
  padding: var(--space-3) var(--space-4); text-align: left;
  border-bottom: 1px solid var(--border-subtle); white-space: nowrap;
}
.ws-table th { color: var(--text-muted); font-weight: var(--font-medium); font-size: var(--text-xs); }
.ws-table tbody tr { cursor: pointer; }
.ws-table tbody tr:hover { background: var(--bg-hover); }
.ws-table tbody tr.is-selected { background: var(--bg-hover); }
.ws-table tbody tr:last-child td { border-bottom: none; }
.ws-name { font-weight: var(--font-medium); display: flex; align-items: center; gap: 6px; }
.ws-dim { color: var(--text-muted); font-size: var(--text-xs); }
.ws-error {
  margin: 4px 0 0; max-width: 260px; overflow: hidden; text-overflow: ellipsis;
  color: var(--danger); font-size: var(--text-xs);
}
.ws-warn { color: var(--warning); font-size: var(--text-xs); }
.ws-empty { text-align: center; color: var(--text-muted); font-size: var(--text-sm); padding: var(--space-6); }
.ws-actions { display: flex; align-items: center; gap: 4px; }

.ws-form { display: flex; flex-direction: column; gap: var(--space-4); }
.ws-field { display: flex; flex-direction: column; gap: 6px; }
.ws-label { font-size: var(--text-sm); color: var(--text-primary); }
.ws-tip { margin: 0; font-size: var(--text-xs); color: var(--text-muted); line-height: 1.6; }
.ws-check { display: inline-flex; align-items: center; gap: 8px; font-size: var(--text-sm); color: var(--text-primary); }
.ws-foot { display: flex; gap: var(--space-2); justify-content: flex-end; width: 100%; }

.ws-cred {
  display: flex; flex-direction: column; gap: 8px;
  padding: var(--space-3); border-radius: 10px;
  background: var(--bg-hover);
}
.ws-cred-row { display: flex; align-items: center; gap: 10px; font-size: var(--text-sm); }
.ws-cred-key { color: var(--text-muted); min-width: 56px; }

.ws-list {
  margin: 0; padding-left: 18px; color: var(--text-secondary);
  font-size: var(--text-sm); line-height: 1.9;
}
.ws-link { color: var(--text-primary); text-decoration: underline; text-underline-offset: 2px; }
code { font-family: var(--font-mono); font-size: 0.92em; color: var(--text-primary); }
</style>
