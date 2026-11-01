<script setup>
/**
 * Admin 域名管理：域名列表 + 添加 + 设主域 + 删除 + 应用 + 状态轮询。
 */
import { ref, onMounted, onActivated, onUnmounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

// 域名管理
const domains = ref([])
const domainsLoading = ref(false)
const newDomain = ref('')
const domainSaving = ref(false)
const busyIds = ref([]) // 正在应用/续签的域名 id
let pollTimer = null

const loadDomains = async () => {
  domainsLoading.value = true
  try {
    domains.value = await adminApi.listDomains()
  } catch (e) {
    domains.value = []
  } finally {
    domainsLoading.value = false
  }
}

// 静默刷新（轮询用，避免表格闪 loading）
const refreshDomains = async () => {
  try {
    domains.value = await adminApi.listDomains()
  } catch (e) {
    // ignore
  }
}

// 兼容 boolean / 字符串 / 数字的 is_primary
const isPrimary = (d) => d.is_primary === true || d.is_primary === '1' || d.is_primary === 1

const statusMap = {
  pending: { label: '待配置', cls: 'st-status--pending' },
  ok: { label: 'SSL正常', cls: 'st-status--ok' },
  none: { label: '未配置', cls: 'st-status--none' },
  expiring: { label: '即将到期', cls: 'st-status--expiring' },
  failed: { label: '失败', cls: 'st-status--failed' },
}
const isBusy = (d) => busyIds.value.includes(d.id)
const statusOf = (d) => {
  if (isBusy(d)) return { label: '配置中', cls: 'st-status--pending' }
  return statusMap[d.ssl_status] || statusMap.none
}

// 证书到期日期：显示为本地时间的「M月D日」
const fmtExpiry = (v) => {
  if (!v) return ''
  const dt = new Date(v)
  if (Number.isNaN(dt.getTime())) return ''
  return `${dt.getMonth() + 1}月${dt.getDate()}日`
}
// 完整到期时间（鼠标悬浮提示）
const fmtExpiryFull = (v) => {
  if (!v) return ''
  const dt = new Date(v)
  if (Number.isNaN(dt.getTime())) return ''
  const p = (n) => String(n).padStart(2, '0')
  return `${dt.getFullYear()}-${p(dt.getMonth() + 1)}-${p(dt.getDate())} ${p(dt.getHours())}:${p(dt.getMinutes())}`
}

const setBusy = (id, on) => {
  busyIds.value = on
    ? [...new Set([...busyIds.value, id])]
    : busyIds.value.filter((x) => x !== id)
}

// 轮询该域名状态（listDomains，间隔 2s，最多 60s）→ ok / failed
const pollDomainUntilSettled = async (id) => {
  const deadline = Date.now() + 60_000
  while (Date.now() < deadline) {
    await new Promise((r) => {
      pollTimer = setTimeout(r, 2000)
    })
    await refreshDomains()
    const d = domains.value.find((x) => x.id === id)
    if (!d) {
      setBusy(id, false)
      return
    }
    if (d.ssl_status === 'ok' || d.ssl_status === 'expiring') {
      toast.success(`${d.domain} SSL 配置完成`)
      setBusy(id, false)
      return
    }
    if (d.ssl_status === 'failed') {
      toast.error(d.last_error || `${d.domain} 配置失败`)
      setBusy(id, false)
      return
    }
  }
  setBusy(id, false)
  toast.error('配置超时，请稍后刷新查看')
}

const applyDomain = async (d) => {
  setBusy(d.id, true)
  try {
    await adminApi.applyDomain(d.id)
    toast.success(`已提交应用 ${d.domain}`)
    await pollDomainUntilSettled(d.id)
  } catch (e) {
    setBusy(d.id, false)
    toast.error(e.message || '提交失败')
  }
}

const addDomain = async () => {
  const val = newDomain.value.trim().toLowerCase()
  if (!val) {
    toast.error('请输入域名')
    return
  }
  domainSaving.value = true
  try {
    await adminApi.addDomain(val)
    toast.success('域名已添加')
    newDomain.value = ''
    await loadDomains()
  } catch (e) {
    toast.error(e.message || '添加失败')
  } finally {
    domainSaving.value = false
  }
}

const makePrimary = async (d) => {
  try {
    await adminApi.setDomainPrimary(d.id)
    toast.success(`已将 ${d.domain} 设为主域`)
    await loadDomains()
  } catch (e) {
    toast.error(e.message || '设置失败')
  }
}

const removeDomain = async (d) => {
  if (!confirm(`确定删除域名「${d.domain}」？`)) return
  try {
    await adminApi.deleteDomain(d.id)
    toast.success('已删除')
    await loadDomains()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

onMounted(() => {
  loadDomains()
})
onActivated(() => {
  loadDomains()
})
onUnmounted(() => {
  if (pollTimer) clearTimeout(pollTimer)
})
</script>

<template>
  <div class="st">
    <h1 class="st-title">域名</h1>

    <!-- 域名管理 -->
    <Card>
      <h2 class="st-section-title">域名管理</h2>

      <div class="st-domains-add">
        <Input v-model="newDomain" placeholder="example.zes.one" mono />
        <Button variant="primary" size="sm" :loading="domainSaving" @click="addDomain">
          <Icon name="plus" :size="16" /> 添加
        </Button>
      </div>

      <div class="st-domains-wrap">
        <table class="st-domains-table">
          <thead>
            <tr>
              <th>域名</th>
              <th>主域</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="d in domains" :key="d.id">
              <td class="st-domain-name">{{ d.domain }}</td>
              <td>
                <Badge v-if="isPrimary(d)" variant="info" dot>主域</Badge>
                <span v-else class="st-dim">—</span>
              </td>
              <td>
                <span
                  :class="['st-domain-status', statusOf(d).cls]"
                  :title="d.ssl_status === 'failed' ? (d.last_error || 'SSL 配置失败') : ''"
                >
                  <span class="st-status-dot"></span>{{ statusOf(d).label }}
                </span>
                <span
                  v-if="fmtExpiry(d.cert_expires_at)"
                  class="st-domain-expiry"
                  :title="`证书到期：${fmtExpiryFull(d.cert_expires_at)}`"
                >{{ fmtExpiry(d.cert_expires_at) }}到期</span>
              </td>
              <td>
                <div class="st-domain-actions">
                  <Button variant="primary" size="sm" :loading="isBusy(d)" @click="applyDomain(d)">应用</Button>
                  <Button v-if="!isPrimary(d)" variant="ghost" size="sm" @click="makePrimary(d)">设为主域</Button>
                  <Button variant="danger" size="sm" @click="removeDomain(d)">删除</Button>
                </div>
              </td>
            </tr>
            <tr v-if="!domains.length && !domainsLoading">
              <td colspan="4" class="st-domain-empty">暂无域名</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>
  </div>
</template>

<style scoped>
.st-title {
  margin: 0 0 var(--space-5);
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.st-section-title {
  margin: 0 0 var(--space-3);
  font-size: var(--text-md);
  font-weight: var(--font-semibold);
}

/* 域名管理 */
.st-domains-add {
  display: flex;
  gap: var(--space-2);
  align-items: center;
  margin-bottom: var(--space-4);
}
.st-domains-add .ch-input {
  max-width: 320px;
}
.st-domains-wrap {
  overflow-x: auto;
}
.st-domains-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.st-domains-table th,
.st-domains-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
.st-domains-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.st-domains-table tbody tr:hover {
  background: var(--bg-hover);
}
.st-domain-name {
  font-family: var(--font-mono);
  font-weight: var(--font-medium);
}
.st-dim {
  color: var(--text-muted);
}
.st-domain-status {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  font-size: var(--text-sm);
}
.st-status-dot {
  width: 6px;
  height: 6px;
  border-radius: var(--radius-full);
  background: currentColor;
}
.st-domain-status.st-status--pending { color: var(--warning); }
.st-domain-status.st-status--ok { color: var(--success); }
.st-domain-status.st-status--none { color: var(--text-muted); }
.st-domain-status.st-status--expiring { color: var(--warning); }
.st-domain-status.st-status--failed { color: var(--danger); cursor: help; }
.st-domain-expiry {
  margin-left: var(--space-2);
  color: var(--text-muted);
  font-size: var(--text-xs);
  white-space: nowrap;
}
.st-domain-actions {
  display: flex;
  gap: var(--space-1);
}
.st-domain-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
</style>
