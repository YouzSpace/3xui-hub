<script setup>
/**
 * Admin · App 版本分发（精简版）。
 *
 * 只保留最基础的 4 项：版本号 / 更新说明 / 下载地址 / 强制更新开关。
 * App 启动时 GET /api/app-version 读同一批键做升级决策。
 */
import { ref, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const form = ref({
  app_latest_version_name: '',
  app_update_notes: '',
  app_download_url: '',
  app_force_update: '0',
})

const loading = ref(false)
const saving = ref(false)
const saved = ref(false)

const load = async () => {
  loading.value = true
  try {
    const data = await adminApi.getAppVersionSettings()
    form.value = { ...form.value, ...data }
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const save = async () => {
  const f = form.value
  saving.value = true
  try {
    await adminApi.updateAppVersionSettings({
      app_latest_version_name: f.app_latest_version_name,
      app_update_notes: f.app_update_notes,
      app_download_url: f.app_download_url,
      app_force_update: f.app_force_update === '1' || f.app_force_update === true,
    })
    toast.success('已保存')
    saved.value = true
    setTimeout(() => { saved.value = false }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const savingForce = ref(false)

// 强制更新开关：改了就自动保存，不用点「保存设置」。
const toggleForce = async (checked) => {
  form.value.app_force_update = checked ? '1' : '0'
  savingForce.value = true
  try {
    await adminApi.updateAppVersionSettings({ app_force_update: checked })
    toast.success(checked ? '已开启强制更新' : '已关闭强制更新')
  } catch (e) {
    // 保存失败回滚开关状态。
    form.value.app_force_update = checked ? '0' : '1'
    toast.error(e.message || '保存失败')
  } finally {
    savingForce.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="av">
    <div class="av-head">
      <div>
        <h1 class="av-title">App 版本</h1>
        <p class="av-sub">配置 App 升级规则，留空则不提示更新。</p>
      </div>
      <Button variant="primary" size="sm" :loading="saving" :success="saved" @click="save">保存设置</Button>
    </div>

    <Card class="av-card">
      <h2 class="av-section">升级配置</h2>
      <p class="av-tip av-tip-block">版本号留空 = 站点未配置分发，App 端静默不弹更新。</p>
      <div class="av-grid">
        <label class="av-field">
          <span class="av-label">版本号</span>
          <Input v-model="form.app_latest_version_name" placeholder="1.2.0" />
        </label>
        <label class="av-field">
          <span class="av-label">下载地址</span>
          <Input v-model="form.app_download_url" placeholder="https://example.com/app.apk" />
        </label>
        <label class="av-field av-field-full">
          <span class="av-label">更新说明</span>
          <textarea v-model="form.app_update_notes" class="av-textarea" rows="4" placeholder="修复连接超时&#10;优化测速"></textarea>
        </label>
        <div class="av-field">
          <span class="av-label">强制更新</span>
          <label class="av-switch">
            <input
              type="checkbox"
              class="av-switch-input"
              :checked="form.app_force_update === '1' || form.app_force_update === true"
              :disabled="savingForce"
              @change="toggleForce($event.target.checked)"
            />
            <span class="av-switch-slider"></span>
            <span class="av-switch-text">{{ savingForce ? '保存中…' : '开启后所有旧版本都强制升级' }}</span>
          </label>
        </div>
      </div>
    </Card>
  </div>
</template>

<style scoped>
.av { max-width: 900px; }
.av-head {
  display: flex; align-items: flex-start; justify-content: space-between;
  gap: var(--space-3); margin-bottom: var(--space-4);
}
.av-title { margin: 0 0 4px; font-size: var(--text-xl); font-weight: var(--font-bold); }
.av-sub { margin: 0; color: var(--text-muted); font-size: var(--text-sm); }
.av-card { margin-bottom: var(--space-4); }
.av-section { margin: 0 0 var(--space-3); font-size: var(--text-base); font-weight: var(--font-semibold); }
.av-grid { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-3) var(--space-4); }
.av-field { display: flex; flex-direction: column; gap: 6px; }
.av-field-full { grid-column: 1 / -1; }
.av-label { font-size: var(--text-sm); }
.av-tip { color: var(--text-muted); font-size: var(--text-xs); line-height: 1.5; }
.av-tip-block { margin: 0 0 var(--space-3); }
.av-switch { display: inline-flex; align-items: center; gap: 10px; cursor: pointer; }
.av-switch-input { position: absolute; opacity: 0; width: 0; height: 0; }
.av-switch-slider {
  position: relative; display: inline-block; width: 48px; height: 27px; flex-shrink: 0;
  background: var(--switch-off, #d1d5db); border-radius: 14px; transition: 0.3s;
  box-shadow: inset 0 1px 3px rgba(0,0,0,0.1);
}
.av-switch-slider::before {
  content: ''; position: absolute; height: 21px; width: 21px; left: 3px; bottom: 3px;
  background: var(--switch-knob, #fff); border-radius: 50%; transition: 0.3s;
  box-shadow: 0 2px 4px rgba(0,0,0,0.2);
}
.av-switch-input:checked + .av-switch-slider { background: var(--switch-on, #2563eb); box-shadow: none; }
.av-switch-input:checked + .av-switch-slider::before { transform: translateX(21px); }
.av-switch-text { font-size: var(--text-xs); color: var(--text-muted); }
.av-textarea {
  width: 100%; padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border-subtle); border-radius: var(--radius-md);
  font-size: var(--text-sm); background: var(--bg-surface); color: var(--text-primary);
  resize: vertical; font-family: inherit;
}
.av-textarea:focus { outline: none; border-color: var(--accent); }
</style>
