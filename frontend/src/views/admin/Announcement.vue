<script setup>
import { ref, onMounted, onActivated } from 'vue'
import { useToast } from '@/components/ui/Toast.vue'
import { adminApi } from '@/api/admin'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'

const toast = useToast()
const loading = ref(false)
const saved = ref(false)

const form = ref({
  announcement: '',
  feedback_link: '',
})

const load = async () => {
  try {
    const data = await adminApi.getSiteSettings()
    if (data) {
      form.value.announcement = data.announcement || ''
      form.value.feedback_link = data.feedback_link || ''
    }
  } catch (e) {}
}

onMounted(load)
onActivated(load)

const onSave = async () => {
  loading.value = true
  try {
    await adminApi.updateSiteSettings(form.value)
    toast.success('保存成功')
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段）
    saved.value = true
    setTimeout(() => { saved.value = false }, 900)
  } catch (e) {
    toast.error(e?.message || '保存失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="ann-page">
    <h1 class="ann-title">公告管理</h1>

    <Card class="ann-card">
      <template #header><h2>登录公告</h2></template>
      <p class="ann-hint">用户登录后弹窗显示，留空则不显示。用户可点击"不再提示"关闭。</p>
      <textarea v-model="form.announcement" class="ann-textarea" rows="6" placeholder="输入公告内容，支持多行文本"></textarea>
    </Card>

    <Card class="ann-card">
      <template #header><h2>反馈链接</h2></template>
      <p class="ann-hint">用户仪表盘会显示反馈按钮，点击跳转到此链接。可填QQ群、TG群、邮箱等。</p>
      <Input v-model="form.feedback_link" label="反馈链接" placeholder="https://t.me/your_group 或 QQ群链接" />
    </Card>

    <div class="ann-save-wrap">
      <Button :loading="loading" :success="saved" @click="onSave" size="lg">保存</Button>
    </div>
  </div>
</template>

<style scoped>
.ann-page { max-width: 640px; }
.ann-title { font-size: var(--text-xl); font-weight: var(--font-semibold); margin-bottom: var(--space-5); }
.ann-card { margin-bottom: var(--space-5); }
.ann-card h2 { font-size: var(--text-base); font-weight: var(--font-semibold); }
.ann-hint { font-size: var(--text-sm); color: var(--text-secondary); margin-bottom: var(--space-3); }
.ann-textarea { width: 100%; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-page); color: var(--text-primary); font-size: var(--text-sm); resize: vertical; line-height: 1.6; font-family: inherit; box-sizing: border-box; }
.ann-textarea:focus { outline: none; border-color: var(--accent); box-shadow: 0 0 0 2px var(--accent-muted); }
.ann-save-wrap { margin-top: var(--space-4); display: flex; justify-content: flex-end; }
@media (max-width: 640px) { .ann-save-wrap { justify-content: stretch; } .ann-save-wrap > button { width: 100%; } }
</style>
