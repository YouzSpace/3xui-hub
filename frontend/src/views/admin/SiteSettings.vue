<script setup>
/**
 * 站点信息配置：修改网站标题、描述、关键词、Logo、Favicon。
 */
import { ref, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const siteConfig = useSiteConfigStore()
const loading = ref(false)
const saving = ref(false)

const form = ref({
  site_title: '',
  site_subtitle: '',
  site_description: '',
  site_keywords: '',
  site_logo: '',
  site_favicon: '',
})

const load = async () => {
  loading.value = true
  try {
    const data = await adminApi.getSiteSettings()
    form.value = { ...form.value, ...data }
  } catch (e) {
    toast.error('加载失败')
  } finally {
    loading.value = false
  }
}

const save = async () => {
  saving.value = true
  try {
    await adminApi.updateSiteSettings(form.value)
    siteConfig.updateConfig(form.value)
    toast.success('保存成功')
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="ss">
    <h1 class="ss-title">网站信息</h1>

    <Card>
      <div class="ss-form">
        <label class="ss-field">
          <span>网站标题</span>
          <Input v-model="form.site_title" placeholder="ControlHub" />
        </label>
        <label class="ss-field">
          <span>网站副标题</span>
          <Input v-model="form.site_subtitle" placeholder="一句话介绍" />
        </label>
        <label class="ss-field">
          <span>网站描述</span>
          <textarea v-model="form.site_description" class="ss-textarea" rows="3" placeholder="一句话描述你的网站"></textarea>
        </label>
        <label class="ss-field">
          <span>SEO 关键词</span>
          <Input v-model="form.site_keywords" placeholder="关键词1, 关键词2, 关键词3" />
        </label>
        <label class="ss-field">
          <span>网站 Logo URL</span>
          <Input v-model="form.site_logo" placeholder="https://example.com/logo.png" />
        </label>
        <label class="ss-field">
          <span>Favicon URL</span>
          <Input v-model="form.site_favicon" placeholder="https://example.com/favicon.ico" />
        </label>
        <div class="ss-actions">
          <Button variant="primary" size="sm" :disabled="saving" @click="save">
            <Icon name="check" :size="16" /> {{ saving ? '保存中...' : '保存' }}
          </Button>
        </div>
      </div>
    </Card>
  </div>
</template>

<style scoped>
.ss-title {
  font-size: var(--text-xl);
  font-weight: var(--font-semibold);
  margin-bottom: var(--space-4);
}
.ss-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  max-width: 600px;
}
.ss-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}
.ss-field span {
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  color: var(--text-secondary);
}
.ss-textarea {
  width: 100%;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  font-size: var(--text-sm);
  background: var(--bg-surface);
  color: var(--text-primary);
  resize: vertical;
  font-family: inherit;
}
.ss-textarea:focus {
  outline: none;
  border-color: var(--accent);
}
.ss-actions {
  padding-top: var(--space-2);
}
</style>
