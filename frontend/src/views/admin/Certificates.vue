<script setup>
/**
 * Admin · 证书管理：TLS 证书库（HY2 与 TLS 入站的证书来源）。
 *
 * 接口口径见 server/internal/handler/admin/xray_certificates*.go：
 *   GET  /admin-api/xray-certificates            → { certificates: [{ id, name, domain, source,
 *                                                    has_cert, has_key, cert_pem_chars, not_after }] }
 *   POST /admin-api/xray-certificates/renew      → { dry_run:true } 只出计划；
 *                                                  { dry_run:false } 真跑，回 checked/due/renewed/failed/bumped_nodes
 *   POST /admin-api/xray-certificates/sync-local → { dir, scanned, created, updated, skipped, reasons }
 *   GET  /admin-api/xray-certificates/:id/pem    → { cert_pem, key_pem, ... }（私钥唯一出口）
 *   DELETE /admin-api/xray-certificates/:id
 *
 * 两处口径写死在这里，避免页面自己编：
 *  1. 来源 source 是后端派生的：local-dir = 能对上面板机证书目录，manual = 手工导入。
 *  2. 「自动续期」没有独立开关，责任方由来源决定（见 xray_certificates_renew.go）：
 *     local-dir 归面板机 acme.sh（root crontab --cron + 每 6 小时目录同步）；
 *     manual 归每日远程续签任务，但它只处理「有入站引用」的证书。
 */
import { ref, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import Modal from '@/components/ui/Modal.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import { useConfirm } from '@/composables/useConfirm'

const toast = useToast()
const { confirm } = useConfirm()

// 与后端 XRAY_CERT_RENEW_BEFORE_DAYS 默认值一致：进入续签窗口的天数
const RENEW_WINDOW_DAYS = 21

const certs = ref([])
const loading = ref(false)
const loadError = ref('')
const busyIds = ref([]) // 正在下载/删除的证书 id

const checking = ref(false)
const renewing = ref(false)
const syncing = ref(false)

const checkOpen = ref(false)
const plan = ref(null)

const setBusy = (id, on) => {
  busyIds.value = on
    ? [...new Set([...busyIds.value, id])]
    : busyIds.value.filter((x) => x !== id)
}

const load = async () => {
  loading.value = true
  loadError.value = ''
  try {
    const res = await adminApi.listXrayCertificates()
    certs.value = Array.isArray(res?.certificates) ? res.certificates : []
  } catch (e) {
    certs.value = []
    loadError.value = e?.message || '加载失败'
  } finally {
    loading.value = false
  }
}

// ===== 展示口径 =====
const SOURCES = {
  'local-dir': { label: '本机目录', variant: 'info', tip: '来源：面板机证书目录（acme.sh 维护）' },
  manual: { label: '手工导入', variant: 'info', tip: '来源：手工添加或节点远程签发后写回' },
}
const sourceOf = (row) => SOURCES[row.source] || { label: '未知', variant: 'info', tip: '' }

/**
 * 自动续期责任方。manual 证书由每日续签任务交给引用它的节点重签，
 * 没有入站引用时任务会跳过 —— 这一点在悬浮提示里说清楚，别让管理员误以为全自动。
 */
const autoRenewOf = (row) =>
  row.source === 'local-dir'
    ? {
        label: '本机 acme.sh',
        tip: '面板机 acme.sh 定时续签，每 6 小时自动同步进证书库',
      }
    : {
        label: '节点自动续签',
        tip: `每天自动检查：到期前 ${RENEW_WINDOW_DAYS} 天交给引用它的节点重新签发（需有入站引用）`,
      }

/** not_after 是后端 isoTime 的 UTC 口径（…+00:00），交给 Date 解析即可 */
const parseTime = (v) => {
  if (!v) return null
  const d = new Date(v)
  return Number.isNaN(d.getTime()) ? null : d
}

const fmtDate = (v) => {
  const d = parseTime(v)
  if (!d) return '—'
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/** 到期状态：无时间 / 已过期 / 续签窗口内 / 正常 */
const expiryOf = (row) => {
  const d = parseTime(row.not_after)
  if (!d) return { label: '无到期时间', variant: 'info', days: null }
  const days = Math.ceil((d.getTime() - Date.now()) / 86400000)
  if (days <= 0) return { label: '已过期', variant: 'danger', days }
  if (days <= RENEW_WINDOW_DAYS) return { label: '即将到期', variant: 'warning', days }
  return { label: '正常', variant: 'success', days }
}

const daysText = (row) => {
  const { days } = expiryOf(row)
  if (days === null) return ''
  if (days <= 0) return `已过期 ${Math.abs(days)} 天`
  return `剩 ${days} 天`
}

// ===== 顶部操作 =====
const runCheck = async () => {
  if (checking.value) return
  checking.value = true
  try {
    plan.value = await adminApi.renewXrayCertificates(true)
    checkOpen.value = true
  } catch (e) {
    toast.error(e?.message || '续期检查失败')
  } finally {
    checking.value = false
  }
}

const runRenew = async () => {
  if (renewing.value) return
  const ok = await confirm({
    title: '执行续期',
    message:
      '将让各证书所在节点立即重新签发到期的证书（真实 ACME 请求，可能耗时数分钟）。\n' +
      '面板机 acme.sh 维护的证书不在本任务范围内。确定继续？',
    confirmText: '开始续期',
  })
  if (!ok) return

  renewing.value = true
  try {
    const res = await adminApi.renewXrayCertificates(false)
    toast.success(
      `续期完成：检查 ${Number(res?.checked) || 0} · 到期 ${Number(res?.due) || 0} · ` +
        `成功 ${Number(res?.renewed) || 0} · 失败 ${Number(res?.failed) || 0}`,
    )
    if (Number(res?.failed) > 0) {
      const first = Array.isArray(res?.reasons) ? res.reasons[0] : ''
      toast.error(first || '部分证书续期失败，请查看运行提示')
    }
    await load()
  } catch (e) {
    toast.error(e?.message || '续期失败')
  } finally {
    renewing.value = false
  }
}

const syncLocal = async () => {
  if (syncing.value) return
  const ok = await confirm({
    title: '同步本机证书',
    message:
      '将扫描面板机的证书目录（默认 /root/cert，可用 XRAY_CERT_DIR 覆盖），' +
      '把 acme.sh 签好/续好的证书写进证书库。确定继续？',
    confirmText: '开始同步',
  })
  if (!ok) return

  syncing.value = true
  try {
    const res = await adminApi.syncLocalXrayCertificates()
    toast.success(
      `同步完成：扫描 ${Number(res?.scanned) || 0} · 新增 ${Number(res?.created) || 0} · ` +
        `更新 ${Number(res?.updated) || 0}`,
    )
    // 目录不存在 / 读不了时后端只回 reason，不报错：这里把第一条原因显出来
    if (!Number(res?.scanned) && Array.isArray(res?.reasons) && res.reasons.length) {
      toast.error(res.reasons[0])
    }
    await load()
  } catch (e) {
    toast.error(e?.message || '同步失败')
  } finally {
    syncing.value = false
  }
}

// ===== 行内操作 =====
/** 文件名只留字母数字/点/横线下划线，避免域名里的 * 等字符进文件名 */
const pemFileName = (row) => {
  const base = String(row.domain || row.name || 'certificate').replace(/[^\w.-]+/g, '_')
  return `${base}.pem`
}

/**
 * 下载证书链（仅 cert_pem）。私钥明文虽然接口会一起返回，但不落浏览器下载目录：
 * 页面没有使用私钥的场景，少一次明文副本。
 */
const downloadPem = async (row) => {
  if (busyIds.value.includes(row.id)) return
  setBusy(row.id, true)
  try {
    const pem = await adminApi.getXrayCertificatePem(row.id)
    if (!pem?.cert_pem) {
      toast.error('这条记录没有证书内容')
      return
    }
    const blob = new Blob([pem.cert_pem], { type: 'application/x-pem-file' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = pemFileName(row)
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    URL.revokeObjectURL(url)
    toast.success(`已下载 ${a.download}`)
  } catch (e) {
    toast.error(e?.message || '下载失败')
  } finally {
    setBusy(row.id, false)
  }
}

const removeCert = async (row) => {
  if (busyIds.value.includes(row.id)) return
  const ok = await confirm({
    title: '删除证书',
    message: `确定删除证书「${row.name}」？引用它的入站将拿不到证书，直到重新选择或续签。`,
    confirmText: '删除',
    danger: true,
  })
  if (!ok) return

  setBusy(row.id, true)
  try {
    await adminApi.deleteXrayCertificate(row.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e?.message || '删除失败')
  } finally {
    setBusy(row.id, false)
  }
}

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="ct">
    <div class="ct-head">
      <div>
        <h1 class="ch-page-title--mb">证书管理</h1>
        <p class="ct-sub">TLS 证书库：Hysteria2 与 TLS 入站都从这里引用证书。私钥加密保存，列表不回显。</p>
      </div>
      <div class="ct-tools">
        <Button variant="secondary" size="sm" :loading="checking" @click="runCheck">
          <Icon name="search" :size="14" /> 续期检查
        </Button>
        <Button variant="primary" size="sm" :loading="renewing" @click="runRenew">
          <Icon name="refresh" :size="14" /> 执行续期
        </Button>
        <Button variant="secondary" size="sm" :loading="syncing" @click="syncLocal">
          <Icon name="inbox" :size="14" /> 同步本机证书
        </Button>
        <Button variant="ghost" size="sm" :loading="loading" @click="load">
          <Icon name="refresh" :size="14" /> 刷新
        </Button>
      </div>
    </div>

    <Card :padding="false">
      <div class="ct-table-wrap">
        <table class="ch-table">
          <thead>
            <tr>
              <th>域名</th>
              <th>到期时间</th>
              <th>到期状态</th>
              <th>来源</th>
              <th>自动续期</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in certs" :key="c.id">
              <td>
                <span class="ct-domain">{{ c.domain || c.name }}</span>
                <span v-if="c.domain && c.name !== c.domain" class="ct-name">{{ c.name }}</span>
              </td>
              <td class="mono">{{ fmtDate(c.not_after) }}</td>
              <td>
                <Badge :variant="expiryOf(c).variant" dot>{{ expiryOf(c).label }}</Badge>
                <span v-if="daysText(c)" class="ct-days">{{ daysText(c) }}</span>
              </td>
              <td>
                <span :title="sourceOf(c).tip">{{ sourceOf(c).label }}</span>
                <span v-if="!c.has_cert || !c.has_key" class="ct-warn">缺证书或私钥</span>
              </td>
              <td>
                <span :title="autoRenewOf(c).tip">{{ autoRenewOf(c).label }}</span>
              </td>
              <td>
                <div class="ct-actions">
                  <Button
                    variant="secondary"
                    size="sm"
                    :loading="busyIds.includes(c.id)"
                    title="下载证书链（不含私钥）"
                    @click="downloadPem(c)"
                  >
                    下载 PEM
                  </Button>
                  <Button variant="danger" size="sm" @click="removeCert(c)">删除</Button>
                </div>
              </td>
            </tr>
            <tr v-if="loadError">
              <td colspan="6" class="ch-empty">加载失败：{{ loadError }}</td>
            </tr>
            <tr v-else-if="!certs.length && !loading">
              <td colspan="6" class="ch-empty">
                证书库还是空的。可先点上方「同步本机证书」把面板机的证书导入；
                手工证书在入站表单里添加后也会出现在这里。
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- 续期检查：只展示计划，不签发 -->
    <Modal v-model="checkOpen" title="续期检查" width="720px">
      <template v-if="plan">
        <p class="ct-plan-summary">
          检查 {{ plan.checked ?? 0 }} 张 · 到期 {{ plan.due ?? 0 }} 张 · 跳过 {{ plan.skipped ?? 0 }} 张 ·
          续签窗口 {{ plan.window_days ?? RENEW_WINDOW_DAYS }} 天
        </p>

        <div v-if="plan.plan?.length" class="ct-plan-wrap">
          <table class="ch-table">
            <thead>
              <tr>
                <th>证书</th>
                <th>到期时间</th>
                <th>续签节点</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="p in plan.plan" :key="p.id">
                <td>{{ p.name }}</td>
                <td class="mono">{{ fmtDate(p.not_after) }}</td>
                <td>
                  <span v-if="p.target_node">{{ p.target_node }}</span>
                  <span v-else class="ct-warn">没有可用的续签节点</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="ct-plan-empty">当前没有需要续签的证书。</p>

        <ul v-if="plan.reasons?.length" class="ct-reasons">
          <li v-for="(r, i) in plan.reasons" :key="i">{{ r }}</li>
        </ul>
      </template>

      <template #footer>
        <Button variant="secondary" size="sm" @click="checkOpen = false">关闭</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Certificates.css"></style>
