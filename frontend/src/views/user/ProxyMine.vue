<script setup>
/**
 * 用户端 · 我的代理。
 *
 * 展示已购买的代理权益：
 *  - 独享账号（webshare）：账号密码 + 已用/额度 + 实时 IP 列表
 *  - 共享 socks：socks 凭据 + 地址端口
 * 到期时间由后端下发；到期后账号已被上游停用，这里只显示状态。
 */
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'

const router = useRouter()
const toast = useToast()

const items = ref([])
const loading = ref(true)
const ipState = ref({})   // { [entitlementId]: { loading, list, error } }

const load = async () => {
  loading.value = true
  try {
    items.value = (await userApi.proxyEntitlements()) || []
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const daysLeft = (iso) => {
  if (!iso) return null
  const diff = new Date(iso).getTime() - Date.now()
  return Math.ceil(diff / 86400000)
}

const usageText = (item) => {
  if (!item.limit_bytes) return null
  const used = (item.used_bytes || 0) / 1024 ** 3
  const limit = item.limit_bytes / 1024 ** 3
  return `${used.toFixed(2)} / ${limit.toFixed(0)} GB`
}

const usagePercent = (item) => {
  if (!item.limit_bytes) return 0
  return Math.min(100, Math.round(((item.used_bytes || 0) / item.limit_bytes) * 100))
}

const copy = async (text, label) => {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`${label}已复制`)
  } catch (e) {
    toast.error('复制失败，请手动选择复制')
  }
}

const copyAll = (item) => {
  const c = item.credentials
  if (!c) return
  const text = `服务器：gateway.webshare.io\n端口：80\n用户名：${c.username}\n密码：${c.password}`
  copy(text, '凭据')
}

const loadIps = async (item) => {
  const id = item.id
  if (ipState.value[id]?.list?.length) {
    // 再点一次收起
    ipState.value = { ...ipState.value, [id]: { ...ipState.value[id], list: [] } }
    return
  }
  ipState.value = { ...ipState.value, [id]: { loading: true, list: [], error: '' } }
  try {
    const list = (await userApi.proxyIps(id)) || []
    ipState.value = { ...ipState.value, [id]: { loading: false, list, error: '' } }
  } catch (e) {
    ipState.value = { ...ipState.value, [id]: { loading: false, list: [], error: e.message || '获取失败' } }
  }
}

const statusLabel = (s) => ({ active: '生效中', expired: '已到期', disabled: '已停用' }[s] || s)
const statusVariant = (s) => ({ active: 'success', expired: 'warning', disabled: 'warning' }[s] || 'default')

onMounted(load)
</script>

<template>
  <div class="pm">
    <div class="pm-head">
      <div>
        <h1 class="pm-title">我的代理</h1>
        <p class="pm-sub">这里是你购买的代理出口，和节点订阅互不影响。</p>
      </div>
      <Button variant="ghost" size="sm" @click="router.push('/dashboard/proxy')">
        <Icon name="plus" :size="15" /> 购买代理
      </Button>
    </div>

    <Card v-for="item in items" :key="item.id" class="pm-card">
      <div class="pm-chead">
        <div class="pm-name">
          {{ item.sku_name }}
          <Badge variant="default">{{ item.kind === 'socks' ? '共享 socks' : '独享账号' }}</Badge>
        </div>
        <Badge :variant="statusVariant(item.status)">{{ statusLabel(item.status) }}</Badge>
      </div>

      <div class="pm-rows">
        <div class="pm-row">
          <span class="pm-label">到期</span>
          <span>
            {{ item.expires_at ? new Date(item.expires_at).toLocaleDateString('zh-CN') : '—' }}
            <span v-if="daysLeft(item.expires_at) > 0" class="pm-dim">（还剩 {{ daysLeft(item.expires_at) }} 天）</span>
            <span v-else-if="item.status === 'expired'" class="pm-dim">（已到期，续费后恢复）</span>
          </span>
        </div>

        <div v-if="item.limit_bytes" class="pm-row">
          <span class="pm-label">流量</span>
          <span>
            {{ usageText(item) }}
            <span class="pm-bar"><i :style="{ width: usagePercent(item) + '%' }" /></span>
          </span>
        </div>

        <div v-if="item.credentials" class="pm-row">
          <span class="pm-label">用户名</span>
          <span class="pm-mono">
            {{ item.credentials.username }}
            <button class="pm-copy" type="button" @click="copy(item.credentials.username, '用户名')">
              <Icon name="copy" :size="13" />
            </button>
          </span>
        </div>

        <div v-if="item.credentials" class="pm-row">
          <span class="pm-label">密码</span>
          <span class="pm-mono">
            {{ item.credentials.password }}
            <button class="pm-copy" type="button" @click="copy(item.credentials.password, '密码')">
              <Icon name="copy" :size="13" />
            </button>
          </span>
        </div>

        <div v-if="item.credentials" class="pm-row">
          <span class="pm-label">连接</span>
          <span class="pm-dim">
            {{ item.kind === 'socks' ? '用上面账号密码连接下方 IP 与端口（socks5）' : 'gateway.webshare.io : 80（用上面账号密码 + 任一 IP）' }}
          </span>
        </div>
      </div>

      <div class="pm-actions">
        <Button v-if="item.credentials" variant="ghost" size="sm" @click="copyAll(item)">
          <Icon name="copy" :size="14" /> 复制全部凭据
        </Button>
        <Button v-if="item.status === 'active'" variant="ghost" size="sm" @click="loadIps(item)">
          <Icon name="globe" :size="14" />
          {{ ipState[item.id]?.list?.length ? '收起 IP' : '查看 IP 列表' }}
        </Button>
        <Button variant="ghost" size="sm" :disabled="item.status === 'active' && daysLeft(item.expires_at) > 7" @click="router.push('/dashboard/proxy')">
          <Icon name="refresh" :size="14" /> 续费 / 换套餐
        </Button>
      </div>

      <div v-if="ipState[item.id]" class="pm-ips">
        <p v-if="ipState[item.id].loading" class="pm-dim">加载中…</p>
        <p v-else-if="ipState[item.id].error" class="pm-dim">{{ ipState[item.id].error }}</p>
        <div v-else-if="ipState[item.id].list.length" class="pm-ip-list">
          <div v-for="(ip, i) in ipState[item.id].list" :key="i" class="pm-ip">
            <span class="pm-mono">{{ ip.address }}:{{ ip.port }}</span>
            <span class="pm-dim">{{ ip.country }}{{ ip.city ? ' · ' + ip.city : '' }}</span>
            <span class="pm-dim">{{ ip.valid ? '可用' : '不可用' }}</span>
          </div>
        </div>
        <p v-else class="pm-dim">没有拿到 IP，稍后再试或联系客服。</p>
      </div>
    </Card>

    <Card v-if="!items.length && !loading" class="pm-card">
      <p class="pm-empty">你还没有购买代理。<RouterLink to="/dashboard/proxy" class="pm-link">去看看代理商城</RouterLink></p>
    </Card>
  </div>
</template>

<style scoped>
.pm { max-width: 900px; }
.pm-head { display: flex; align-items: flex-start; justify-content: space-between; gap: var(--space-3); margin-bottom: var(--space-4); }
.pm-title { margin: 0 0 4px; font-size: var(--text-xl); font-weight: var(--font-bold); }
.pm-sub { margin: 0; color: var(--text-muted); font-size: var(--text-sm); }
.pm-card { margin-bottom: var(--space-4); }

.pm-chead { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); }
.pm-name { display: flex; align-items: center; gap: 8px; font-weight: var(--font-bold); font-size: var(--text-base); }

.pm-rows { display: flex; flex-direction: column; gap: 8px; margin-top: var(--space-3); }
.pm-row { display: flex; align-items: baseline; gap: var(--space-3); font-size: var(--text-sm); }
.pm-label { flex: 0 0 56px; color: var(--text-muted); font-size: var(--text-sm); }
.pm-mono { font-family: var(--font-mono); font-size: var(--text-sm); display: inline-flex; align-items: center; gap: 6px; word-break: break-all; }
.pm-dim { color: var(--text-muted); font-size: var(--text-xs); }
.pm-copy {
  border: none; background: transparent; cursor: pointer; color: var(--text-muted);
  display: inline-flex; align-items: center; padding: 2px;
}
.pm-copy:hover { color: var(--accent); }

.pm-bar { display: inline-block; width: 90px; height: 4px; border-radius: 2px; background: var(--bg-hover); margin-left: 8px; vertical-align: middle; overflow: hidden; }
.pm-bar i { display: block; height: 100%; background: var(--accent); }

.pm-actions { display: flex; flex-wrap: wrap; gap: 6px; margin-top: var(--space-3); padding-top: var(--space-3); border-top: 1px solid var(--border-subtle); }

.pm-ips { margin-top: var(--space-3); border-top: 1px dashed var(--border-subtle); padding-top: var(--space-3); }
.pm-ip-list { display: flex; flex-direction: column; gap: 6px; max-height: 240px; overflow-y: auto; }
.pm-ip { display: flex; align-items: center; gap: var(--space-3); font-size: var(--text-xs); }

.pm-empty { margin: 0; color: var(--text-muted); font-size: var(--text-sm); text-align: center; padding: var(--space-4) 0; }
.pm-link { color: var(--accent); text-decoration: none; margin-left: 4px; }
</style>
