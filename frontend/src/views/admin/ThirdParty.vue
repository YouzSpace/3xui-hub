<script setup>
/**
 * Admin 第三方线路：接入外部机场订阅，后台定时拉取缓存节点，注入套餐用户订阅。
 * - 总开关：一键从所有用户订阅里摘掉第三方（SiteConfig third_party_enabled）
 * - 每条订阅：备注名 + 链接（打码显示，含机场 token 属敏感）+ 启用开关
 * - 拉取：后台每 2 小时自动；页面可「立即刷新」；失败保留上次成功缓存并标红原因
 * - 节点在用户端统一命名「地区+序号」，不暴露机场原名
 */
import { ref, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const items = ref([])
const masterEnabled = ref(false)
const masterSaving = ref(false)

const modalOpen = ref(false)
const editing = ref(null)
const saving = ref(false)
const saved = ref(false)
const form = ref({ name: '', url: '', enabled: true })

const load = async () => {
  try {
    const data = await adminApi.listThirdParty()
    items.value = data?.items ?? []
    masterEnabled.value = !!data?.enabled
  } catch (e) {
    toast.error('加载失败')
  }
}

// 主开关：从 change 事件读 DOM 新值（不依赖 v-model 的执行顺序），发送成功才同步 ref；失败回滚 DOM
const toggleMaster = async (event) => {
  const next = event.target.checked
  masterSaving.value = true
  try {
    await adminApi.setThirdPartyEnabled(next)
    masterEnabled.value = next
    toast.success(next ? '第三方已开启，将注入套餐用户订阅' : '已关闭：所有用户订阅不再返回第三方节点')
  } catch (e) {
    event.target.checked = !next
    toast.error(e.message || '保存失败')
  } finally {
    masterSaving.value = false
  }
}

const openCreate = () => {
  editing.value = null
  form.value = { name: '', url: '', enabled: true }
  modalOpen.value = true
}

const openEdit = (s) => {
  editing.value = s
  form.value = { name: s.name, url: s.url, enabled: s.enabled }
  modalOpen.value = true
}

const save = async () => {
  const url = form.value.url.trim()
  if (!form.value.name.trim()) {
    toast.error('请填写备注名')
    return
  }
  if (!url) {
    toast.error('请填写订阅链接')
    return
  }
  saving.value = true
  try {
    if (editing.value) {
      await adminApi.updateThirdParty(editing.value.id, { name: form.value.name.trim(), url, enabled: form.value.enabled })
      toast.success('已更新')
    } else {
      await adminApi.createThirdParty({ name: form.value.name.trim(), url, enabled: form.value.enabled })
      toast.success('已创建，已尝试拉取（失败不影响保存，可稍后点立即刷新）')
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

// 行启用开关：从事件读 DOM 新值；失败回滚 DOM，避免「看着开、实际没改」
const toggleEnabled = async (s, event) => {
  const next = event.target.checked
  try {
    await adminApi.updateThirdParty(s.id, { enabled: next })
    s.enabled = next
  } catch (e) {
    event.target.checked = !next
    toast.error(e.message || '操作失败')
  }
}

const refreshNow = async (s) => {
  try {
    await adminApi.refreshThirdParty(s.id)
    toast.success('已拉取')
    await load()
  } catch (e) {
    toast.error(e.message || '拉取失败')
    await load()
  }
}

const remove = async (s) => {
  if (!confirm(`删除「${s.name}」？引用它的套餐会自动摘除该条线路。`)) return
  try {
    await adminApi.deleteThirdParty(s.id)
    // 撤销：按原 name/url/enabled 重建（第三方订阅是简单数据，可恢复）
    toast.success(`已删除「${s.name}」`, 4000, async () => {
      try {
        await adminApi.createThirdParty({ name: s.name, url: s.url, enabled: s.enabled })
        await load()
        toast.success('已恢复')
      } catch (e) {
        toast.error(e.message || '恢复失败')
      }
    })
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

const statusVariant = (s) => ({ success: 'success', failed: 'danger', pending: 'info' }[s] || 'info')
const statusText = (s) => ({ success: '成功', failed: '失败', pending: '待拉取' }[s] || s)
const fmtTime = (iso) => (iso ? new Date(iso).toLocaleString() : '—')

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="tp">
    <div class="tp-head">
      <h1 class="tp-title">第三方线路</h1>
      <div class="tp-head-actions">
        <label class="tp-master" :class="{ off: !masterEnabled }">
          <span class="tp-switch" :class="{ on: masterEnabled, saving: masterSaving }">
            <input type="checkbox" :checked="masterEnabled" :disabled="masterSaving" @change="toggleMaster" />
            <span class="tp-switch-track"><span class="tp-switch-knob"></span></span>
          </span>
          <span>总开关</span>
        </label>
        <Button variant="primary" size="sm" @click="openCreate">
          <Icon name="plus" :size="16" /> 新增线路
        </Button>
      </div>
    </div>

    <p class="tp-hint">
      接入外部机场订阅，节点会注入「套餐里勾选了该线路」的用户订阅；节点名对用户统一显示为「地区+序号」（如 香港1、日本2），不暴露机场原名。
      不管流量、只管时间：套餐失效或总开关关闭后，用户更新订阅即不再返回这些节点。
      拉取失败时保留上次成功缓存，后台每 2 小时自动拉取一次。
    </p>

    <Card :padding="false" class="tp-table-card">
      <div class="tp-table-wrap">
        <table class="tp-table">
          <thead>
            <tr>
              <th>备注名</th>
              <th>链接</th>
              <th>节点数</th>
              <th>上次拉取</th>
              <th>状态</th>
              <th>启用</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="s in items" :key="s.id">
              <td>{{ s.name }}</td>
              <td class="tp-url" :title="s.url">{{ s.url_masked }}</td>
              <td class="mono">{{ s.last_status === 'success' ? s.node_count : '—' }}</td>
              <td>{{ fmtTime(s.last_fetched_at) }}</td>
              <td>
                <div>
                  <Badge :variant="statusVariant(s.last_status)" dot>{{ statusText(s.last_status) }}</Badge>
                  <p v-if="s.last_error" class="tp-err" :title="s.last_error">{{ s.last_error }}</p>
                </div>
              </td>
              <td>
                <input type="checkbox" :checked="s.enabled" @change="(e) => toggleEnabled(s, e)" />
              </td>
              <td>
                <div class="tp-actions">
                  <Button variant="ghost" size="sm" title="立即拉取" @click="refreshNow(s)">
                    <Icon name="refresh" :size="16" />
                  </Button>
                  <Button variant="ghost" size="sm" title="编辑" @click="openEdit(s)">
                    <Icon name="edit" :size="16" />
                  </Button>
                  <Button variant="ghost" size="sm" title="删除" @click="remove(s)">
                    <Icon name="trash" :size="16" />
                  </Button>
                </div>
              </td>
            </tr>
            <tr v-if="!items.length">
              <td colspan="7" class="tp-empty">暂无第三方线路，点右上角「新增线路」接入</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Modal v-model="modalOpen" :title="editing ? `编辑线路 · ${editing.name}` : '新增第三方线路'" width="480px">
      <div class="tp-form">
        <label class="tp-field">
          <span>备注名</span>
          <Input v-model="form.name" placeholder="如：机场A" />
        </label>
        <label class="tp-field">
          <span>订阅链接</span>
          <Input v-model="form.url" placeholder="https://…（订阅地址，含机场 token）" />
          <span class="tp-muted">链接含机场 token，仅管理端可见；拉取与节点注入均在本面板内完成。</span>
        </label>
        <label class="tp-field-row">
          <input type="checkbox" v-model="form.enabled" />
          <span>启用（参与拉取与注入）</span>
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="modalOpen = false">取消</Button>
        <Button variant="primary" size="sm" :disabled="saving" :success="saved" @click="save">
          {{ saving ? '保存中…' : '保存' }}
        </Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.tp {
  max-width: 1100px;
}
.tp-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-3);
  gap: var(--space-2);
  flex-wrap: wrap;
}
.tp-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.tp-head-actions {
  display: flex;
  align-items: center;
  gap: var(--space-4);
}
.tp-master {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--text-primary);
  cursor: pointer;
  user-select: none;
}
.tp-master.off {
  color: var(--text-muted);
}
/* 总开关：iOS 风格 toggle（原生 checkbox 视觉隐藏，track/knob 纯 CSS 绘制） */
.tp-switch {
  position: relative;
  display: inline-block;
  width: 40px;
  height: 22px;
  flex: 0 0 auto;
}
.tp-switch input[type='checkbox'] {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  margin: 0;
  opacity: 0;
  cursor: pointer;
}
.tp-switch input[type='checkbox']:disabled {
  cursor: not-allowed;
}
.tp-switch-track {
  position: absolute;
  inset: 0;
  border-radius: 999px;
  background: var(--switch-off);
  transition: background 0.2s ease;
  pointer-events: none;
}
.tp-switch-knob {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 18px;
  height: 18px;
  border-radius: 50%;
  background: var(--switch-knob);
  box-shadow: 0 1px 2px rgba(0, 0, 0, 0.2);
  transition: transform 0.2s ease;
}
.tp-switch.on .tp-switch-track {
  background: var(--switch-on);
}
.tp-switch.on .tp-switch-knob {
  transform: translateX(18px);
}
.tp-switch.saving {
  opacity: 0.6;
}
.tp-hint {
  margin: 0 0 var(--space-4);
  color: var(--text-secondary);
  font-size: var(--text-sm);
  max-width: 720px;
  line-height: 1.6;
}
.tp-table-card {
  margin-top: 0;
}
.tp-table-wrap {
  overflow-x: auto;
}
.tp-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}
.tp-table th,
.tp-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  vertical-align: middle;
  border-bottom: 1px solid var(--border-subtle);
}
.tp-table th {
  color: var(--text-muted);
  font-weight: var(--font-medium);
  font-size: var(--text-xs);
  white-space: nowrap;
}
.tp-table tbody tr:hover {
  background: var(--bg-hover);
}
.tp-table tbody tr:last-child td {
  border-bottom: none;
}
.tp-url {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--text-secondary);
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tp-err {
  margin: var(--space-1) 0 0;
  font-size: var(--text-xs);
  color: #f87171;
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tp-table td:last-child {
  text-align: right;
}
.tp-table td input[type='checkbox'] {
  cursor: pointer;
}
.tp-actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-1);
}
.tp-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
.tp-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.tp-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-sm);
  color: var(--text-secondary);
}
.tp-field-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--text-primary);
}
.tp-field-row input[type='checkbox'] {
  cursor: pointer;
}
.tp-muted {
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.mono {
  font-family: var(--font-mono);
}
</style>
