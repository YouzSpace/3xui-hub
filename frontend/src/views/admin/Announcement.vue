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

/* ── 反馈与联系列表（feedback_links）：与公告共用底部「保存全部」一个按钮 ── */
const fbItems = ref([]) // [{title, type, url}]

/** 解析后端存的 feedback_links JSON（坏数据静默回空，不崩页面）。 */
const parseFeedbackLinks = (raw) => {
  if (!raw) return []
  try {
    const arr = JSON.parse(raw)
    return Array.isArray(arr)
      ? arr
          .filter((it) => it && typeof it === 'object' && typeof it.title === 'string')
          .map((it) => ({
            title: it.title || '',
            type: it.type === 'qr' ? 'qr' : 'link',
            url: typeof it.url === 'string' ? it.url : '',
          }))
      : []
  } catch {
    return []
  }
}

const addFbItem = () => {
  fbItems.value.push({ title: '', type: 'link', url: '' })
}

const removeFbItem = (i) => {
  fbItems.value.splice(i, 1)
}

/** 上移/下移（交换后整体保存）。 */
const moveFbItem = (i, dir) => {
  const j = i + dir
  if (j < 0 || j >= fbItems.value.length) return
  const list = fbItems.value
  ;[list[i], list[j]] = [list[j], list[i]]
}

const load = async () => {
  try {
    const data = await adminApi.getSiteSettings()
    if (data) {
      form.value.announcement = data.announcement || ''
      form.value.feedback_link = data.feedback_link || ''
      fbItems.value = parseFeedbackLinks(data.feedback_links)
    }
  } catch (e) {}
}

onMounted(load)
onActivated(load)

const onSave = async () => {
  loading.value = true
  try {
    // 一个保存管全部：公告 + 老反馈链接 + 反馈与联系列表，一次提交
    const payload = { ...form.value }
    const items = fbItems.value.filter((it) => it.title.trim() && it.url.trim())
    if (items.length !== fbItems.value.length) {
      toast.error('反馈与联系里有未填完的行（标题或链接为空），请补全或删除')
      return
    }
    payload.feedback_links = JSON.stringify(items)
    await adminApi.updateSiteSettings(payload)
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
    <h1 class="ann-title">公告与联系</h1>

    <Card class="ann-card">
      <template #header><h2>登录公告</h2></template>
      <p class="ann-hint">用户登录后弹窗显示，留空则不显示。用户可点击"不再提示"关闭。</p>
      <textarea v-model="form.announcement" class="ann-textarea" rows="6" placeholder="输入公告内容，支持多行文本"></textarea>
    </Card>

    <Card class="ann-card">
      <template #header><h2>反馈与联系</h2></template>
      <p class="ann-hint">
        用户端「更多 → 反馈与联系」的二级页内容。一行一个联系方式：
        类型「链接」= 点击整行直接跳转（TG / X / Ins 等）；类型「二维码」= 点击看大图（填图片 URL）。
        不填列表时退回下方「反馈链接」整行跳转的老行为。
      </p>

      <div v-for="(it, i) in fbItems" :key="i" class="ann-fb-row">
        <Input v-model="it.title" placeholder="标题（如：Telegram 群）" class="ann-fb-title" />
        <select v-model="it.type" class="ann-fb-type">
          <option value="link">链接</option>
          <option value="qr">二维码</option>
        </select>
        <Input
          v-model="it.url"
          :placeholder="it.type === 'qr' ? '二维码图片 URL' : 'https://t.me/xxx'"
          class="ann-fb-url"
        />
        <button class="ann-fb-move" title="上移" :disabled="i === 0" @click="moveFbItem(i, -1)">↑</button>
        <button class="ann-fb-move" title="下移" :disabled="i === fbItems.length - 1" @click="moveFbItem(i, 1)">↓</button>
        <button class="ann-fb-del" title="删除" @click="removeFbItem(i)">✕</button>
      </div>

      <div class="ann-fb-actions">
        <Button variant="secondary" size="sm" @click="addFbItem">+ 添加一行</Button>
      </div>

      <p class="ann-fb-legacy-hint">
        兼容：未添加列表时，下面这个老「反馈链接」仍然生效（点整行直接跳转）。
      </p>
      <Input v-model="form.feedback_link" label="反馈链接（老入口，可留空）" placeholder="https://t.me/your_group 或 QQ群链接" />
    </Card>

    <div class="ann-save-wrap">
      <Button :loading="loading" :success="saved" @click="onSave" size="lg">保存全部</Button>
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

/* 反馈与联系列表 */
.ann-fb-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.ann-fb-title { flex: 1.1; }
.ann-fb-type {
  padding: 8px 10px;
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  background: var(--bg-page);
  color: var(--text-primary);
  font-size: var(--text-sm);
  font-family: inherit;
}
.ann-fb-url { flex: 1.6; }
.ann-fb-move,
.ann-fb-del {
  width: 30px;
  height: 36px;
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  background: var(--bg-page);
  color: var(--text-secondary);
  cursor: pointer;
  font-size: 13px;
  transition: all 0.2s;
}
.ann-fb-move:hover:not(:disabled) { color: var(--accent); border-color: var(--accent); }
.ann-fb-move:disabled { opacity: 0.4; cursor: default; }
.ann-fb-del:hover { color: var(--danger); border-color: var(--danger); }
.ann-fb-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin: var(--space-2) 0 var(--space-3);
}
.ann-fb-legacy-hint {
  margin: 0 0 var(--space-2);
  font-size: var(--text-xs);
  color: var(--text-muted);
}
</style>
