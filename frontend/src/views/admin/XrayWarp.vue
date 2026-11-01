<script setup>
/**
 * xray 节点配置 · WARP（Cloudflare 出口）。
 *
 * 流程：注册并应用（面板调 CF API 后自动生成 wireguard 出站下发）
 * → 一键换 IP / 设置 WARP+ license / 定时换 IP。
 * 应用后用「路由」把需要的流量导向 warp 出站即可。
 */
import { ref, watch } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Icon from '@/components/ui/Icon.vue'
import XrayNodePicker from '@/components/admin/XrayNodePicker.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const nodeId = ref(null)
const state = ref(null)
const loading = ref(false)
const busy = ref('')
const saved = ref(false)

const licenseInput = ref('')
const rotateOptions = [
  { label: '关闭定时换 IP', value: 0 },
  { label: '每 1 小时', value: 1 },
  { label: '每 6 小时', value: 6 },
  { label: '每 12 小时', value: 12 },
  { label: '每 24 小时', value: 24 },
  { label: '每 72 小时', value: 72 },
]
const rotateHours = ref(0)

const load = async () => {
  if (!nodeId.value) {
    state.value = null
    return
  }
  loading.value = true
  try {
    const res = await adminApi.getXrayWarp(nodeId.value)
    state.value = res || null
    rotateHours.value = res?.auto_rotate_hours ?? 0
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}
watch(nodeId, load)

const act = async (action, fn, okMsg) => {
  busy.value = action
  try {
    const res = await fn(nodeId.value)
    if (res && res.registered !== undefined) {
      state.value = res
      rotateHours.value = res?.auto_rotate_hours ?? rotateHours.value
    } else {
      await load()
    }
    // 注册已成功但自动应用失败：如实提示（账户已就绪，可手动「重新应用」）
    if (res && res.apply_error) {
      toast.error(`已注册，但应用到节点失败：${res.apply_error}`)
    } else {
      toast.success(okMsg)
    }
  } catch (e) {
    toast.error(e.message || '操作失败')
  } finally {
    busy.value = ''
  }
}

const register = () => act('register', adminApi.registerXrayWarp, '已注册并应用 WARP（wireguard 出站已下发，秒级生效）')
const apply = () => act('apply', adminApi.applyXrayWarp, '已应用到节点（wireguard 出站已下发，秒级生效）')
const rotate = () => act('rotate', adminApi.rotateXrayWarp, '已换 IP（新出口已下发）')
const remove = async () => {
  if (!confirm('删除 WARP？将同时移除节点的 warp 出站（路由中引用它的规则会失效）')) return
  busy.value = 'remove'
  try {
    await adminApi.deleteXrayWarp(nodeId.value)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  } finally {
    busy.value = ''
  }
}

const saveLicense = async () => {
  if (!licenseInput.value) return
  busy.value = 'license'
  try {
    await adminApi.updateXrayWarp(nodeId.value, { license: licenseInput.value })
    licenseInput.value = ''
    toast.success('license 已设置')
    await load()
  } catch (e) {
    toast.error(e.message || '设置失败')
  } finally {
    busy.value = ''
  }
}

const saveRotate = async () => {
  busy.value = 'rotate-hours'
  try {
    await adminApi.updateXrayWarp(nodeId.value, { auto_rotate_hours: rotateHours.value })
    toast.success(rotateHours.value > 0 ? `已开启定时换 IP（每 ${rotateHours.value} 小时）` : '已关闭定时换 IP')
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段）
    saved.value = true
    setTimeout(() => { saved.value = false }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    busy.value = ''
  }
}
</script>

<template>
  <div class="an">
    <div class="an-head">
      <h1 class="an-title">xray WARP</h1>
    </div>
    <p class="an-desc">
      给节点加一个 Cloudflare WARP 出口：注册并应用（自动生成 wireguard 出站）→ 在「路由」里把需要的流量导向 <b>warp</b> 出站。
    </p>

    <div class="an-toolbar">
      <XrayNodePicker v-model="nodeId" />
    </div>

    <Card v-if="nodeId" :padding="true">
      <div v-if="loading" class="an-empty">加载中…</div>
      <div v-else-if="state" class="an-warp">
        <div class="an-warp-rows">
          <div class="an-warp-row">
            <span>设备状态</span>
            <Badge v-if="state.registered" variant="success" dot>已注册</Badge>
            <Badge v-else variant="warning" dot>未注册</Badge>
          </div>
          <div class="an-warp-row">
            <span>节点应用</span>
            <Badge v-if="state.applied" variant="success" dot>已应用</Badge>
            <Badge v-else variant="info" dot>未应用</Badge>
          </div>
          <div class="an-warp-row">
            <span>设备 ID</span><b class="mono">{{ state.device_id || '—' }}</b>
          </div>
          <div class="an-warp-row">
            <span>Peer 端点</span><b class="mono">{{ state.peer_endpoint || '—' }}</b>
          </div>
          <div class="an-warp-row">
            <span>接口地址</span><b class="mono">{{ (state.addresses || []).join(' , ') || '—' }}</b>
          </div>
          <div class="an-warp-row">
            <span>WARP+ License</span>
            <Badge :variant="state.license_set ? 'success' : 'info'">{{ state.license_set ? '已设置' : '未设置（免费版）' }}</Badge>
          </div>
          <div class="an-warp-row">
            <span>最近换 IP</span><b class="mono">{{ state.last_rotate_at ? new Date(state.last_rotate_at).toLocaleString() : '—' }}</b>
          </div>
        </div>

        <div class="an-warp-actions">
          <Button v-if="!state.registered" variant="primary" size="sm" :loading="busy === 'register'" @click="register">
            <Icon name="globe" :size="16" /> 注册并应用
          </Button>
          <template v-else>
            <Button variant="primary" size="sm" :loading="busy === 'apply'" @click="apply">
              <Icon name="check" :size="16" /> {{ state.applied ? '重新应用' : '应用到节点' }}
            </Button>
            <Button variant="secondary" size="sm" :loading="busy === 'rotate'" @click="rotate">
              <Icon name="refresh" :size="16" /> 一键换 IP
            </Button>
            <Button variant="danger" size="sm" :loading="busy === 'remove'" @click="remove">删除</Button>
          </template>
        </div>

        <div v-if="state.registered" class="an-warp-settings">
          <div class="an-setting">
            <span class="an-setting-label">WARP+ License（可选）</span>
            <div class="an-setting-row">
              <Input v-model="licenseInput" placeholder="粘贴 license key" size="sm" />
              <Button size="sm" variant="secondary" :loading="busy === 'license'" @click="saveLicense">设置</Button>
            </div>
          </div>
          <div class="an-setting">
            <span class="an-setting-label">定时换 IP</span>
            <div class="an-setting-row">
              <Select v-model="rotateHours" :options="rotateOptions" size="sm" />
              <Button size="sm" variant="secondary" :loading="busy === 'rotate-hours'" :success="saved" @click="saveRotate">保存</Button>
            </div>
          </div>
        </div>
      </div>
      <div v-else class="an-empty">加载失败</div>
    </Card>
  </div>
</template>

<style scoped>
.an-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-2);
}
.an-title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: var(--font-bold);
}
.an-desc {
  margin: 0 0 var(--space-3);
  color: var(--text-muted);
  font-size: var(--text-sm);
}
.an-toolbar {
  display: flex;
  align-items: center;
  margin-bottom: var(--space-3);
}
.an-warp {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}
.an-warp-rows {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.an-warp-row {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  font-size: var(--text-sm);
}
.an-warp-row > span:first-child {
  width: 120px;
  color: var(--text-muted);
  flex-shrink: 0;
}
.an-warp-actions {
  display: flex;
  gap: var(--space-2);
}
.an-warp-settings {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-4);
  padding-top: var(--space-3);
  border-top: 1px solid var(--border-subtle);
}
/* 窄屏（手机）：WARP 设置两列压得太窄，单列显示。 */
@media (max-width: 767px) {
  .an-warp-settings {
    grid-template-columns: 1fr;
  }
}
.an-setting {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.an-setting-label {
  font-size: var(--text-sm);
  color: var(--text-muted);
}
.an-setting-row {
  display: flex;
  gap: var(--space-2);
  align-items: center;
}
.an-setting-row :deep(.ch-input),
.an-setting-row :deep(.ch-select) {
  flex: 1;
}
.an-empty {
  text-align: center;
  color: var(--text-muted);
  padding: var(--space-6);
}
.mono {
  font-family: var(--font-mono);
}
</style>
