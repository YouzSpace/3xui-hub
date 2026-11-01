<script setup>
/**
 * 备份管理：导出/导入数据库。
 *
 * 导入的真实语义（以后端为准，前端别加戏）：
 *   - .env 只搬 APP_KEY 一行 + 用户手动改的站点地址；
 *   - 备份里的旧域名（APP_URL）**永远不会**被自动写进本机 .env。域名迁移后常要改
 *     （支付回调 / storage 链接 / 邮件 EHLO 都看 APP_URL），所以留一个手动输入框，
 *     默认填本机当前主域名：用户不动它就一个字都不改，要迁到新域名就在它基础上改。
 *     注意「多域名」页管的是 nginx 绑定，跟 .env 里的 APP_URL 不是一回事；
 *   - 合并导入 = 只补备份里有、本机没有的；替换导入 = 整库清空替换；
 *   - 导入后后端会自动补跑迁移 + 重启队列 worker，结果都在响应的 notice 里；
 *   - 收尾失败时数据已经进库了，后端返回 500 + 原文，别用统一文案盖掉。
 */
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Modal from '@/components/ui/Modal.vue'
import Input from '@/components/ui/Input.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const exporting = ref(false)
const importing = ref(false)
// 哪一个模式在导入中（只让那一个按钮转圈）
const importingMode = ref('')
const fileInput = ref(null)
const diff = ref(null)
const hasEnv = ref(false)
const envDiff = ref([])
const restoreEnv = ref(true)
const diffModalOpen = ref(false)
const selectedFile = ref(null)
// 站点地址（APP_URL）：预填「本机当前主域名」，用户在这个基础上按需改成迁移后的新域名。
const siteUrl = ref('')
// 本机当前 APP_URL（后端预览时回传）：既是输入框的默认值，也是「用户到底改没改」的基准。
const currentAppUrl = ref('')
// 本机 .env 里没有 APP_URL 时的兜底默认值：域名列表里的主域（拼成 https://…）
const fallbackAppUrl = ref('')
// 替换导入的二次确认：点一次只翻到确认态，确认后才真的发请求
const confirmingReplace = ref(false)
// 最近一次导入的结果（后端原文）。留在页面上，不被几秒就消失的 toast 吞掉
const importResult = ref(null)

const MODE_LABEL = { merge: '合并导入', overwrite: '替换导入' }

// APP_KEY 不回传明文（后端已标记为「不同」），这里再兜一层
const envValue = (e, side) => (e.key === 'APP_KEY' ? '不同' : e[side])

// 7 张预览表本机计数全是 0 → 本机是空的，替换导入没有可丢的东西
const localEmpty = computed(
  () => Array.isArray(diff.value) && diff.value.length > 0 && diff.value.every((d) => d.current === 0),
)

// 备份里的 APP_KEY 与本机不同：此时关掉开关，导入的节点/支付/邮件凭据就永远解不开
const appKeyDiffers = computed(() => envDiff.value.some((e) => e.key === 'APP_KEY'))

// 站点地址允许留空（留空 = 不改本机域名）；填了就必须是带协议的完整 URL，
// 光写个域名后端会拿 422 拒掉，不如在这里当面说清楚。
const siteUrlError = computed(() => {
  const v = siteUrl.value.trim()
  if (!v) return ''
  return /^https?:\/\/.+/i.test(v) ? '' : '站点地址必须以 http:// 或 https:// 开头，例如 https://example.com'
})

// 输入框的基准值 = 本机当前主域名：.env 里有 APP_URL 就用它，没有则退回域名列表里的主域。
const siteUrlBaseline = computed(() => (currentAppUrl.value || fallbackAppUrl.value).trim())

// 只有用户把默认值改掉了才把 site_url 发给后端。值没动（哪怕它只是从域名列表推出来的）
// 就一个字都不提交 —— 免得白重写一次 .env 的 APP_URL 行。
const siteUrlChanged = computed(() => siteUrl.value.trim() !== siteUrlBaseline.value)

/** 本机域名列表里的主域（拼成 https://…）；拿不到就返回空串。 */
const primaryDomainUrl = async () => {
  try {
    const list = await adminApi.listDomains()
    // 兼容 boolean / 字符串 / 数字的 is_primary（跟 Domains 页一个口径）
    const primary = (list || []).find(
      (d) => d.is_primary === true || d.is_primary === '1' || d.is_primary === 1,
    )
    return primary?.domain ? `https://${primary.domain}` : ''
  } catch {
    return ''
  }
}

// 锁定：发现不同就把开关按回开启（模板里同时 disabled，不让再关掉）
watch(appKeyDiffers, (differs) => {
  if (differs) restoreEnv.value = true
})

// 导入中不许关弹窗。Modal 的右上角「×」在公共组件里管不了，只能挂个 body 钩子临时藏掉
// （样式在文件末尾的非 scoped 块里），背景点击交给 :persistent，取消按钮直接 disabled。
watch(importing, (v) => document.body.classList.toggle('bk-importing', v))
onBeforeUnmount(() => document.body.classList.remove('bk-importing'))

const exportDb = async () => {
  exporting.value = true
  try {
    const blob = await adminApi.backupExport()
    const url = window.URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `controlhub-backup-${new Date().toISOString().slice(0, 19).replace(/[:-]/g, '')}.zip`
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    window.URL.revokeObjectURL(url)
    toast.success('导出成功')
  } catch (e) {
    toast.error(e.message || '导出失败')
  } finally {
    exporting.value = false
  }
}

const onFileSelect = async (e) => {
  const file = e.target.files[0]
  if (!file) return

  selectedFile.value = file
  const formData = new FormData()
  formData.append('file', file)

  try {
    const result = await adminApi.backupPreview(formData)
    diff.value = result.diff
    hasEnv.value = result.has_env
    envDiff.value = result.env_diff || []
    // 每次都从「恢复 APP_KEY」开始；两份 APP_KEY 不同的话下面会把它锁在开启位
    restoreEnv.value = true
    // 站点地址默认填本机当前主域名（后端 .env 里的 APP_URL）。用户不动它，
    // 提交时就不会带 site_url —— 本机域名一个字都不改。
    currentAppUrl.value = result.current_app_url || ''
    fallbackAppUrl.value = ''
    if (!currentAppUrl.value) {
      // .env 里没有 APP_URL：退回域名列表里的本机主域当默认值（同样不动就不提交）
      fallbackAppUrl.value = await primaryDomainUrl()
    }
    siteUrl.value = siteUrlBaseline.value
    confirmingReplace.value = false
    importResult.value = null
    diffModalOpen.value = true
  } catch (e) {
    toast.error(e.message || '无法读取备份文件')
  } finally {
    if (fileInput.value) fileInput.value.value = ''
  }
}

/** 关掉预览弹窗并清干净这一轮的预览状态（下次导入必须重新选文件、重新预览）。 */
const resetModal = () => {
  diffModalOpen.value = false
  diff.value = null
  hasEnv.value = false
  envDiff.value = []
  restoreEnv.value = true
  siteUrl.value = ''
  currentAppUrl.value = ''
  fallbackAppUrl.value = ''
  confirmingReplace.value = false
  selectedFile.value = null
}

const doImport = async (mode) => {
  // 填了但不合法就别发请求（按钮那边也 disable 了，这里是兜底）
  if (siteUrlError.value) return

  importing.value = true
  importingMode.value = mode
  importResult.value = null

  // 记下这次提交的站点地址：resetModal() 会把输入框清空，result 里要留住它做回执。
  // 与默认值（本机当前主域名）相同 → 传空 = 不提交，本机 APP_URL 一个字都不动。
  const baseline = siteUrlBaseline.value
  const appliedSiteUrl = siteUrlChanged.value ? siteUrl.value.trim() : ''

  let result
  try {
    const res = await adminApi.backupImport(mode, restoreEnv.value, appliedSiteUrl)
    // notice 是后端完整文案：APP_KEY 恢复情况 + 补跑了几个迁移 + 队列是否重启
    result = {
      type: 'success',
      title: `${MODE_LABEL[mode]}完成`,
      message: res?.notice || res?.env_notice || `${MODE_LABEL[mode]}完成`,
      // 后端 notice 里不含 APP_URL 这一项，这里单独回执一句，让人知道域名到底改没改
      detail: appliedSiteUrl
        ? `本机 APP_URL 已改为 ${appliedSiteUrl}`
        : (baseline ? `本机 APP_URL 保持 ${baseline} 不变` : ''),
    }
  } catch (e) {
    // 后端原文照搬，不做任何包装
    const message = e.message || `${MODE_LABEL[mode]}失败`
    // 收尾失败时后端原文是「数据已导入，但…。已导入的数据未回滚。」——数据其实已经在库里了，
    // 别用「导入失败」这种统一文案盖过去。这类要额外醒目：用户以为白导了会再传一次。
    const partial = /^数据已导入/.test(message)
    result = {
      type: 'error',
      partial,
      title: partial ? '数据已导入，但收尾未完成' : `${MODE_LABEL[mode]}失败`,
      message,
      // 400 = 这份上传的预览记录已失效（超过 30 分钟 / 换过文件 / 预览后放弃过），只能重新选文件；
      // 没有 HTTP 状态码 = 超时或网络断了。导库那步可能已经跑完（收尾的迁移/队列重启最慢），
      // 直接再传一遍只会拿到 400，所以先让人刷新确认。
      hint: e.code === 400
        ? '请重新选择备份文件'
        : (e.code ? '' : '若是超时或网络中断：请先刷新页面确认数据是否已导入，不要直接重复上传（后端会拒绝陈旧上传）。'),
    }
  }

  importing.value = false
  importingMode.value = ''
  importResult.value = result
  resetModal()
  if (result.type === 'success') {
    toast.success(result.title)
  } else {
    toast.error(result.title)
  }
}

const tableLabel = (t) => ({
  users: '用户',
  nodes: '节点',
  plans: '套餐',
  orders: '订单',
  payment_configs: '支付配置',
  admins: '管理员',
  node_inbounds: '节点入站',
}[t] || t)
</script>

<template>
  <div class="bk">
    <h1 class="bk-title">备份</h1>

    <Card>
      <h2 class="bk-section-title">导出数据库</h2>
      <p class="bk-info">下载当前数据库和 APP_KEY（zip），用于备份或迁移到其他服务器。备份里不含本机的数据库密码、队列、邮件等配置。</p>
      <Button variant="primary" size="sm" :loading="exporting" @click="exportDb">
        <Icon name="order" :size="16" /> 导出备份
      </Button>
    </Card>

    <Card style="margin-top: var(--space-4);">
      <h2 class="bk-section-title">导入数据库</h2>
      <p class="bk-info">上传之前导出的 .zip 或 .sql 备份文件，系统会自动分析差异。跨机迁移时用 zip：它带着 APP_KEY，节点凭据才解得开。</p>
      <input ref="fileInput" type="file" accept=".zip,.sql" style="display: none;" @change="onFileSelect" />
      <Button variant="secondary" size="sm" :loading="importing" @click="$refs.fileInput.click()">
        <Icon name="plus" :size="16" /> 选择备份文件
      </Button>

      <!-- 导入结果（后端原文）：收尾失败时也在这儿，不会被 toast 的几秒寿命吞掉 -->
      <div
        v-if="importResult"
        class="bk-result"
        :class="importResult.partial ? 'bk-result--partial' : `bk-result--${importResult.type}`"
      >
        <div class="bk-result-head">
          <Icon :name="importResult.type === 'success' ? 'check' : 'warning'" :size="16" />
          <strong>{{ importResult.title }}</strong>
          <button class="bk-result-close" type="button" aria-label="关闭" @click="importResult = null">
            <Icon name="x" :size="16" />
          </button>
        </div>
        <!-- 后端原文，一字不改；收尾失败时加粗，别被当成一句普通报错划过去 -->
        <p class="bk-result-msg" :class="{ 'bk-result-msg--strong': importResult.partial }">{{ importResult.message }}</p>
        <p v-if="importResult.detail" class="bk-result-detail">{{ importResult.detail }}</p>
        <p v-if="importResult.hint" class="bk-result-hint">{{ importResult.hint }}</p>
      </div>
    </Card>

    <!-- 差异预览弹窗 -->
    <Modal v-model="diffModalOpen" title="导入预览 - 数据差异" width="560px" :persistent="importing">
      <div v-if="diff" class="bk-diff">
        <div v-if="importing" class="bk-importing">
          <Icon name="activity" :size="16" /> 正在导入，请勿关闭页面（大备份可能要几分钟）
        </div>

        <p v-if="selectedFile" class="bk-diff-file">备份文件：{{ selectedFile.name }}</p>

        <!-- 按本机现状给推荐：空的就替换，有数据的就合并 -->
        <div class="bk-recommend" :class="localEmpty ? 'bk-recommend--overwrite' : 'bk-recommend--merge'">
          <Icon name="globe" :size="16" />
          <span v-if="localEmpty">本机 7 张表都没有数据（本机为空），推荐<strong>替换导入</strong>。</span>
          <span v-else>本机已有数据，推荐<strong>合并导入</strong>（只补缺失，已存在的一律不动）。</span>
        </div>

        <template v-if="hasEnv">
          <div class="bk-env-status">
            <label class="bk-env-check">
              <input v-model="restoreEnv" type="checkbox" :disabled="appKeyDiffers" />
              <Icon name="check" :size="16" /> 恢复 APP_KEY（导入的节点凭据需要它）
            </label>
            <p v-if="appKeyDiffers" class="bk-env-warn">
              备份里的 APP_KEY 与本机不同，已锁定为开启：关掉它，导入的节点 / 支付 / 邮件凭据就永远解不开。
            </p>
          </div>
        </template>
        <!-- .sql 直接上传没有 .env，跨机迁移会缺 APP_KEY -->
        <div v-else class="bk-env-missing">
          <Icon name="warning" :size="16" />
          <span>这份备份里没有 .env（.sql 直接上传）：用 .sql 迁移不会带 APP_KEY，导入的节点凭据会解不开。跨机迁移请用导出的 .zip。</span>
        </div>

        <!-- 站点地址（APP_URL）：默认填「本机当前主域名」，用户在这个基础上改成迁移后的新域名。
             保持不动 = 本机 .env 的 APP_URL 一个字都不改；备份里的旧域名永远不会自动写进来。
             迁移后域名常要改 —— 支付回调地址、storage 链接前缀、邮件 EHLO 域名都看它。 -->
        <div class="bk-site-url">
          <label class="bk-site-url-label" for="bk-site-url">站点地址（APP_URL）</label>
          <Input
            id="bk-site-url"
            v-model="siteUrl"
            size="sm"
            mono
            :disabled="importing"
            placeholder="https://example.com"
          />
          <p class="bk-site-url-note">
            默认是本机当前主域名；保持不动则本机域名不被修改。备份里的旧域名永远不会自动写入。
          </p>
          <p v-if="siteUrlError" class="bk-site-url-error">
            <Icon name="warning" :size="14" /> {{ siteUrlError }}
          </p>
          <p v-else-if="siteUrlChanged && siteUrl.trim()" class="bk-site-url-ok">
            <Icon name="check" :size="14" /> 导入后本机 APP_URL 将改为 {{ siteUrl.trim() }}（支付回调、storage 链接、邮件 EHLO 域名都会跟着变）
          </p>
          <p v-else-if="siteUrlChanged" class="bk-site-url-keep">
            <Icon name="check" :size="14" /> 留空：本机 APP_URL 保持不动
          </p>
          <p v-else class="bk-site-url-keep">
            <Icon name="check" :size="14" /> 未修改，本机 APP_URL 保持不动
          </p>
        </div>

        <div v-if="restoreEnv && envDiff.length" class="bk-env-diff">
          <p class="bk-env-diff-title"><Icon name="warning" :size="16" /> 以下配置与备份不同（导入只替换 APP_KEY，其余本机配置不会被改动）：</p>
          <table class="bk-env-diff-table">
            <thead><tr><th>配置项</th><th>当前</th><th>备份</th></tr></thead>
            <tbody>
              <tr v-for="e in envDiff" :key="e.key">
                <td class="mono">{{ e.key }}</td>
                <td class="mono">{{ envValue(e, 'current') }}</td>
                <td class="mono">{{ envValue(e, 'backup') }}</td>
              </tr>
            </tbody>
          </table>
        </div>

        <div class="bk-table-wrap">
        <table class="bk-diff-table">
          <thead>
            <tr>
              <th>数据表</th>
              <th>当前</th>
              <th>备份</th>
              <th>新增</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="d in diff" :key="d.table">
              <td>{{ tableLabel(d.table) }}</td>
              <td class="mono">{{ d.current }}</td>
              <td class="mono">{{ d.import }}</td>
              <td>
                <Badge v-if="d.new > 0" variant="success" size="sm">+{{ d.new }}</Badge>
                <span v-else class="bk-muted">—</span>
              </td>
            </tr>
          </tbody>
        </table>
        </div>
        <p class="bk-diff-note">上表只列了 7 张关键表；备份里其余的表（domains、tutorials、site_settings 等）也会按所选模式一起导入。</p>

        <div class="bk-import-hint">
          <p><strong>合并导入</strong>：只补备份里有、本机没有的数据（用户/节点/套餐/支付配置按业务键去重）；已存在的一律不动。</p>
          <p><strong>替换导入</strong>：清空并替换整个数据库，现有数据将被替换，且不可撤销。</p>
          <p><strong>本机 .env</strong>：只会被替换 APP_KEY 一行（替换前存一份 .env.bak），数据库密码、队列、邮件等其余配置不会被改动。另外，「站点地址（APP_URL）」默认就是本机当前主域名，不动它 APP_URL 就不会被改。</p>
        </div>

        <!-- 替换导入的二次确认：后果写清楚再动手 -->
        <div v-if="confirmingReplace" class="bk-confirm">
          <p class="bk-confirm-title"><Icon name="warning" :size="16" /> 确认替换导入？</p>
          <p class="bk-confirm-body">
            本机数据库将被清空并替换成这份备份：现有数据（用户 / 节点 / 套餐 / 订单 / 支付配置 …）<strong>将被替换，且不可撤销</strong>。
          </p>
          <p v-if="!localEmpty" class="bk-confirm-body">
            本机已有数据，更稳的做法是「合并导入」——它只补缺失，不动现有数据。
          </p>
        </div>
      </div>
      <template #footer>
        <div class="bk-modal-footer">
          <Button v-if="confirmingReplace" variant="ghost" size="sm" :disabled="importing" @click="confirmingReplace = false">
            返回
          </Button>
          <Button v-else variant="ghost" size="sm" :disabled="importing" @click="resetModal">取消</Button>
          <div class="bk-modal-actions">
            <Button
              v-if="confirmingReplace"
              variant="danger"
              size="sm"
              :loading="importing"
              :disabled="!!siteUrlError"
              @click="doImport('overwrite')"
            >
              确认替换导入（不可撤销）
            </Button>
            <template v-else>
              <Button
                variant="secondary"
                size="sm"
                :loading="importing && importingMode === 'merge'"
                :disabled="importing || !!siteUrlError"
                @click="doImport('merge')"
              >
                合并导入
              </Button>
              <Button
                variant="danger"
                size="sm"
                :loading="importing && importingMode === 'overwrite'"
                :disabled="importing || !!siteUrlError"
                @click="confirmingReplace = true"
              >
                替换导入
              </Button>
            </template>
          </div>
        </div>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.bk-title {
  margin: 0 0 var(--space-5);
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.bk-section-title {
  margin: 0 0 var(--space-2);
  font-size: var(--text-md);
  font-weight: var(--font-semibold);
}
.bk-info {
  margin: 0 0 var(--space-4);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.bk-diff-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
  margin-bottom: var(--space-3);
}
.bk-diff-table th,
.bk-diff-table td {
  padding: var(--space-2) var(--space-3);
  text-align: left;
  border-bottom: 1px solid var(--border-subtle);
}
.bk-diff-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
}
.bk-diff-note {
  margin: 0 0 var(--space-4);
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.bk-diff-file {
  margin: 0 0 var(--space-3);
  font-size: var(--text-xs);
  color: var(--text-muted);
  font-family: var(--font-mono);
  word-break: break-all;
}
.bk-importing {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin-bottom: var(--space-3);
  padding: var(--space-2) var(--space-3);
  background: var(--accent-muted);
  color: var(--accent);
  border-radius: var(--radius-md);
  font-size: var(--text-sm);
  font-weight: var(--font-semibold);
}
.bk-recommend {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin-bottom: var(--space-3);
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-md);
  font-size: var(--text-xs);
}
/* 本机为空 → 推荐替换；本机有数据 → 推荐合并 */
.bk-recommend--overwrite {
  background: var(--warning-muted);
  color: var(--warning);
}
.bk-recommend--merge {
  background: var(--info-muted);
  color: var(--info);
}
.bk-env-status {
  margin-bottom: var(--space-3);
  padding: var(--space-2) var(--space-3);
  background: var(--success-muted);
  color: var(--success);
  border-radius: var(--radius-md);
  font-size: var(--text-xs);
  font-weight: var(--font-medium);
}
.bk-env-check {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  cursor: pointer;
  user-select: none;
}
.bk-env-check input:disabled + svg {
  opacity: 0.6;
}
.bk-env-check:has(input:disabled) {
  cursor: not-allowed;
}
.bk-env-warn {
  margin: var(--space-2) 0 0;
  color: var(--danger);
  font-weight: var(--font-semibold);
  line-height: 1.6;
}
.bk-env-missing {
  display: flex;
  align-items: flex-start;
  gap: var(--space-2);
  margin-bottom: var(--space-3);
  padding: var(--space-2) var(--space-3);
  background: var(--warning-muted);
  color: var(--warning);
  border-radius: var(--radius-md);
  font-size: var(--text-xs);
  font-weight: var(--font-medium);
  line-height: 1.6;
}
/* 站点地址（APP_URL）：输入框默认填本机当前主域名 */
.bk-site-url {
  margin-bottom: var(--space-3);
  padding: var(--space-2) var(--space-3);
  background: var(--bg-hover);
  border-radius: var(--radius-md);
}
.bk-site-url-label {
  display: block;
  margin-bottom: var(--space-2);
  font-size: var(--text-xs);
  font-weight: var(--font-semibold);
  color: var(--text-primary);
}
.bk-site-url-note {
  margin: var(--space-2) 0 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
  line-height: 1.6;
}
.bk-site-url-ok,
.bk-site-url-keep,
.bk-site-url-error {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  margin: var(--space-2) 0 0;
  font-size: var(--text-xs);
  line-height: 1.6;
}
.bk-site-url-ok {
  color: var(--success);
  font-weight: var(--font-medium);
}
/* 值没动 / 留空：中性提示，别跟「将要改动」抢眼 */
.bk-site-url-keep {
  color: var(--text-muted);
}
.bk-site-url-error {
  color: var(--danger);
  font-weight: var(--font-semibold);
}
.bk-env-diff {
  margin-bottom: var(--space-3);
  padding: var(--space-2) var(--space-3);
  background: var(--warning-muted);
  border-radius: var(--radius-md);
  font-size: var(--text-xs);
  overflow-x: auto; /* 窄屏：3 列配置对比在 360px 下会撑破 */
}
.bk-env-diff-title {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  margin: 0 0 var(--space-2);
  color: var(--warning);
  font-weight: var(--font-medium);
}
.bk-env-diff-table {
  width: 100%;
  border-collapse: collapse;
}
.bk-env-diff-table th,
.bk-env-diff-table td {
  padding: var(--space-1) var(--space-2);
  text-align: left;
  border-bottom: 1px solid var(--border-subtle);
}
.bk-env-diff-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
}
.bk-muted {
  color: var(--text-muted);
}
.mono {
  font-family: var(--font-mono);
}
.bk-import-hint {
  background: var(--bg-hover);
  border-radius: var(--radius-md);
  padding: var(--space-3);
  font-size: var(--text-xs);
  color: var(--text-secondary);
  line-height: 1.8;
}
.bk-import-hint p {
  margin: 0;
}
.bk-confirm {
  margin-top: var(--space-3);
  padding: var(--space-3);
  background: var(--danger-muted);
  border: 1px solid var(--danger);
  border-radius: var(--radius-md);
  font-size: var(--text-xs);
  line-height: 1.7;
}
.bk-confirm-title {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  margin: 0 0 var(--space-2);
  color: var(--danger);
  font-size: var(--text-sm);
  font-weight: var(--font-bold);
}
.bk-confirm-body {
  margin: 0;
  color: var(--text-primary);
}
.bk-confirm-body + .bk-confirm-body {
  margin-top: var(--space-2);
}
/* 导入结果卡：完整显示后端原文（notice / 收尾失败的那句），不随 toast 消失 */
.bk-result {
  margin-top: var(--space-4);
  padding: var(--space-3);
  border-radius: var(--radius-md);
  border-left: 3px solid transparent;
  font-size: var(--text-sm);
}
.bk-result--success {
  background: var(--success-muted);
  border-left-color: var(--success);
}
.bk-result--error,
/* 收尾失败：数据其实已经进库了，比普通报错更需要抓眼（红色照旧） */
.bk-result--partial {
  background: var(--danger-muted);
  border-left-color: var(--danger);
}
.bk-result-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: var(--font-semibold);
}
.bk-result--success .bk-result-head {
  color: var(--success);
}
.bk-result--error .bk-result-head,
.bk-result--partial .bk-result-head {
  color: var(--danger);
}
.bk-result-close {
  margin-left: auto;
  background: transparent;
  border: none;
  color: var(--text-muted);
  padding: 2px;
  display: inline-flex;
  border-radius: var(--radius-sm);
}
.bk-result-close:hover {
  color: var(--text-primary);
  background: var(--bg-hover);
}
.bk-result-msg {
  margin: var(--space-2) 0 0;
  color: var(--text-primary);
  line-height: 1.7;
  word-break: break-word;
}
/* 收尾失败时后端原文加粗 + 深色描边：别让人当成一句普通报错划过去 */
.bk-result-msg--strong {
  font-weight: var(--font-bold);
  color: var(--danger);
  border-left: 3px solid var(--danger);
  padding-left: var(--space-3);
}
.bk-result-detail {
  margin: var(--space-2) 0 0;
  color: var(--text-secondary);
  line-height: 1.7;
  word-break: break-word;
}
.bk-result-hint {
  margin: var(--space-2) 0 0;
  color: var(--danger);
  font-weight: var(--font-semibold);
}
.bk-modal-footer {
  display: flex;
  justify-content: space-between;
  align-items: center;
  width: 100%;
}
.bk-modal-actions {
  display: flex;
  gap: var(--space-2);
}
</style>

<!-- 非 scoped：导入中藏掉公共 Modal 的右上角「×」（组件改不动，只能从 body 挂钩子）。
     钩子只在 Backup 页 importing 为 true 时存在，离开页面会摘掉。 -->
<style>
body.bk-importing .ch-modal-close {
  display: none;
}
</style>
