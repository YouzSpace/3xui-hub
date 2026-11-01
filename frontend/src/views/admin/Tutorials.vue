<script setup>
/**
 * Tutorials — 管理员教程管理
 */
import { ref, computed, onMounted, onActivated } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import Icon from '@/components/ui/Icon.vue'

const route = useRoute()
const router = useRouter()
const toast = useToast()

const tutorials = ref([])
const editing = ref(null) // null = 列表，{} = 新建/编辑
const form = ref({ title: '', content: '', sort: 0, enabled: true })
const saving = ref(false)
const saved = ref(false)

const load = async () => {
  try { tutorials.value = await adminApi.listTutorials() } catch (e) { tutorials.value = [] }
}

/**
 * 按分类分组展示（仅后台列表页，用户端 TutorialList 不变）。
 * - 组间：分类名拼音升序（localeCompare('zh-CN')，让「安卓系列」排在「iOS」前这类中文习惯生效）
 * - 组内：sort 升序，再按 id 兜底，保证同 sort 值时顺序稳定不跳动
 * 无分类的归到「默认」组，不丢数据。
 */
const grouped = computed(() => {
  const map = new Map()
  for (const t of tutorials.value) {
    const cat = (t.category || '').trim() || '默认'
    if (!map.has(cat)) map.set(cat, [])
    map.get(cat).push(t)
  }
  return Array.from(map.entries())
    .map(([category, items]) => ({
      category,
      items: items.slice().sort((a, b) => (a.sort ?? 0) - (b.sort ?? 0) || a.id - b.id),
    }))
    .sort((a, b) => a.category.localeCompare(b.category, 'zh-CN'))
})


const openNew = () => {
  editing.value = {}
  form.value = { title: '', category: '默认', content: '', sort: 0, enabled: true }
}

const openEdit = (t) => {
  editing.value = t
  form.value = { title: t.title, category: t.category || '默认', content: t.content, sort: t.sort, enabled: t.enabled }
}

const cancel = () => {
  editing.value = null
}

const save = async () => {
  if (!form.value.title.trim()) { toast.error('请输入标题'); return }
  if (!form.value.content.trim()) { toast.error('请输入内容'); return }
  saving.value = true
  try {
    if (editing.value.id) {
      await adminApi.updateTutorial(editing.value.id, form.value)
      toast.success('已更新')
    } else {
      await adminApi.createTutorial(form.value)
      toast.success('已创建')
    }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再回列表
    saved.value = true
    setTimeout(() => {
      saved.value = false
      editing.value = null
    }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const remove = async (t) => {
  if (!confirm(`确定删除「${t.title}」？`)) return
  try {
    await adminApi.deleteTutorial(t.id)
    // 撤销：按原标题/内容重建（教程是简单数据，可恢复）
    toast.success(`已删除「${t.title}」`, 4000, async () => {
      try {
        await adminApi.createTutorial({
          title: t.title,
          category: t.category || '默认',
          content: t.content,
          sort: t.sort ?? 0,
          enabled: t.enabled !== false,
        })
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

const toggleEnabled = async (t) => {
  try {
    await adminApi.updateTutorial(t.id, { enabled: !t.enabled })
    t.enabled = !t.enabled
  } catch (e) {
    toast.error('操作失败')
  }
}

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="tut">
    <!-- 列表 -->
    <template v-if="!editing">
      <div class="tut-header">
        <h2 class="tut-title">教程管理</h2>
        <button class="tut-add-btn" @click="openNew"><Icon name="edit" :size="14" /> 新建教程</button>
      </div>

      <!-- 按分类分组：组间按分类名拼音升序，组内按 sort 升序（见 computed grouped） -->
      <div v-if="grouped.length" class="tut-groups">
        <section v-for="g in grouped" :key="g.category" class="tut-group">
          <header class="tut-group-head">
            <span class="tut-group-name">{{ g.category }}</span>
            <span class="tut-group-count">{{ g.items.length }} 篇</span>
          </header>
          <div class="tut-list">
            <div v-for="t in g.items" :key="t.id" class="tut-item">
              <div class="tut-item-info">
                <span class="tut-item-title">{{ t.title }}</span>
                <span class="tut-item-sort">排序: {{ t.sort }}</span>
              </div>
              <div class="tut-item-actions">
                <button class="tut-toggle" :class="{ 'tut-toggle--on': t.enabled }" @click="toggleEnabled(t)">
                  {{ t.enabled ? '已启用' : '已禁用' }}
                </button>
                <button class="tut-icon-btn" @click="openEdit(t)"><Icon name="edit" :size="16" /></button>
                <button class="tut-icon-btn tut-icon-btn--danger" @click="remove(t)"><Icon name="trash" :size="16" /></button>
              </div>
            </div>
          </div>
        </section>
      </div>
      <p v-else class="tut-empty">暂无教程</p>
    </template>

    <!-- 编辑 -->
    <template v-else>
      <div class="tut-header">
        <h2 class="tut-title">{{ editing.id ? '编辑教程' : '新建教程' }}</h2>
        <button class="tut-back-btn" @click="cancel"><Icon name="chevron-left" :size="14" /> 返回</button>
      </div>

      <div class="tut-form">
        <div class="tut-field">
          <label class="tut-label">标题</label>
          <input v-model="form.title" class="tut-input" placeholder="如：Clash 使用教程" />
        </div>
        <div class="tut-field">
          <label class="tut-label">分类</label>
          <input v-model="form.category" class="tut-input" placeholder="如：Clash、Shadowrocket、V2Ray" />
        </div>
        <div class="tut-field">
          <label class="tut-label">排序（数字越小越靠前）</label>
          <input v-model.number="form.sort" type="number" class="tut-input tut-input--sm" />
        </div>
        <div class="tut-field">
          <label class="tut-label">内容（Markdown）</label>
          <textarea v-model="form.content" class="tut-textarea" rows="20" placeholder="# 教程标题&#10;&#10;正文内容..." />
        </div>
        <div class="tut-field tut-field--row">
          <label class="tut-label">启用</label>
          <label class="tut-switch">
            <input type="checkbox" v-model="form.enabled" />
            <span class="tut-switch-slider"></span>
          </label>
        </div>
        <div class="tut-actions">
          <button class="tut-save-btn" :class="{ 'is-success': saved }" :disabled="saving || saved" @click="save">
            <span v-if="saved" class="tut-save-check" aria-hidden="true">✓</span>
            {{ saving ? '保存中...' : saved ? '已保存' : '保存' }}
          </button>
          <button class="tut-cancel-btn" @click="cancel">取消</button>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.tut { display: flex; flex-direction: column; gap: 20px; }
.tut-header { display: flex; align-items: center; justify-content: space-between; }
.tut-title { margin: 0; font-size: 18px; font-weight: 800; color: #1d1d1f; }

.tut-add-btn, .tut-back-btn {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 8px 16px; border-radius: 999px; font-size: 13px; font-weight: 600;
  border: 1px solid rgba(0,0,0,0.08); background: #fff; color: #1d1d1f;
  cursor: pointer; transition: all 0.2s; font-family: inherit;
}
.tut-add-btn:hover, .tut-back-btn:hover { background: #f5f5f5; }

/* 分组：组间留白分隔，组标题弱化不抢条目 */
.tut-groups { display: flex; flex-direction: column; gap: 24px; }
.tut-group-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding-bottom: 8px;
  margin-bottom: 10px;
  border-bottom: 1px solid rgba(0, 0, 0, 0.06);
}
.tut-group-name { font-size: 13px; font-weight: 600; color: #1d1d1f; letter-spacing: 0.01em; }
.tut-group-count { font-size: 12px; color: #999; font-variant-numeric: tabular-nums; }
html.dark .tut-group-head { border-bottom-color: rgba(255, 255, 255, 0.08); }
html.dark .tut-group-name { color: #e5e5e5; }

.tut-list { display: flex; flex-direction: column; gap: 8px; }
.tut-item {
  display: flex; align-items: center; justify-content: space-between;
  padding: 16px 20px; background: #fff; border: 1px solid rgba(0,0,0,0.04);
  border-radius: 12px; box-shadow: 0 2px 12px -2px rgba(0,0,0,0.06);
}
.tut-item-info { display: flex; align-items: center; gap: 12px; }
.tut-item-title { font-size: 14px; font-weight: 600; color: #1d1d1f; }
.tut-item-sort { font-size: 12px; color: #999; }
.tut-item-actions { display: flex; align-items: center; gap: 8px; }

.tut-toggle {
  padding: 4px 12px; border-radius: 999px; font-size: 12px; font-weight: 600;
  border: 1px solid rgba(0,0,0,0.08); background: #f5f5f5; color: #999;
  cursor: pointer; transition: all 0.2s; font-family: inherit;
}
.tut-toggle--on { background: #f0fdf4; color: #16a34a; border-color: #bbf7d0; }

.tut-icon-btn {
  display: flex; align-items: center; justify-content: center;
  width: 32px; height: 32px; border-radius: 8px; border: none;
  background: transparent; color: #666; cursor: pointer; transition: all 0.2s;
}
.tut-icon-btn:hover { background: #f0f0f0; }
.tut-icon-btn--danger:hover { background: #fef2f2; color: #dc2626; }

.tut-empty { text-align: center; color: #999; font-size: 13px; padding: 40px; }

/* 表单 */
.tut-form { display: flex; flex-direction: column; gap: 16px; }
.tut-field { display: flex; flex-direction: column; gap: 6px; }
.tut-field--row { flex-direction: row; align-items: center; gap: 12px; }
.tut-label { font-size: 13px; font-weight: 600; color: #666; }
.tut-input {
  padding: 10px 14px; border: 1px solid rgba(0,0,0,0.08); border-radius: 10px;
  font-size: 14px; font-family: inherit; outline: none; transition: border-color 0.2s;
}
.tut-input:focus { border-color: #2563eb; }
.tut-input--sm { width: 120px; }
.tut-textarea {
  padding: 14px; border: 1px solid rgba(0,0,0,0.08); border-radius: 10px;
  font-size: 14px; font-family: 'SF Mono', 'Fira Code', monospace; outline: none;
  resize: vertical; line-height: 1.6; transition: border-color 0.2s;
}
.tut-textarea:focus { border-color: #2563eb; }

/* 开关 */
.tut-switch { position: relative; display: inline-block; width: 44px; height: 24px; }
.tut-switch input { opacity: 0; width: 0; height: 0; }
.tut-switch-slider {
  position: absolute; cursor: pointer; inset: 0;
  background: var(--switch-off, #ccc); border-radius: 999px; transition: 0.3s;
}
.tut-switch-slider::before {
  content: ''; position: absolute; height: 18px; width: 18px;
  left: 3px; bottom: 3px; background: var(--switch-knob, #fff); border-radius: 50%; transition: 0.3s;
}
.tut-switch input:checked + .tut-switch-slider { background: var(--switch-on, #2563eb); }
.tut-switch input:checked + .tut-switch-slider::before { transform: translateX(20px); }

.tut-actions { display: flex; gap: 12px; margin-top: 8px; }
.tut-save-btn {
  padding: 10px 24px; border-radius: 999px; font-size: 14px; font-weight: 600;
  border: none; background: var(--accent); color: #fff; cursor: pointer;
  transition: all 0.2s; font-family: inherit;
}
.tut-save-btn:hover { background: var(--accent-hover); }
.tut-save-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.tut-save-btn.is-success { background: var(--success); opacity: 1; cursor: default; }
.tut-save-check { display: inline-block; margin-right: 4px; animation: tut-check-in 0.35s cubic-bezier(0.34, 1.56, 0.64, 1); }
@keyframes tut-check-in {
  from { transform: scale(0); opacity: 0; }
  to { transform: scale(1); opacity: 1; }
}
.tut-cancel-btn {
  padding: 10px 24px; border-radius: 999px; font-size: 14px; font-weight: 600;
  border: 1px solid rgba(0,0,0,0.08); background: #fff; color: #1d1d1f;
  cursor: pointer; transition: all 0.2s; font-family: inherit;
}
.tut-cancel-btn:hover { background: #f5f5f5; }

/* 暗色模式 */
html.dark .tut-title { color: #fff; }
html.dark .tut-add-btn, html.dark .tut-back-btn { background: rgba(255,255,255,0.05); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
html.dark .tut-item { background: rgba(20,20,20,0.8); border-color: rgba(255,255,255,0.06); }
html.dark .tut-item-title { color: #e5e5e5; }
html.dark .tut-toggle { background: rgba(255,255,255,0.05); color: #888; border-color: rgba(255,255,255,0.08); }
html.dark .tut-toggle--on { color: #4ade80; background: rgba(22,163,74,0.15); }
html.dark .tut-icon-btn { color: #999; }
html.dark .tut-icon-btn:hover { background: rgba(255,255,255,0.08); }
html.dark .tut-label { color: #999; }
html.dark .tut-input, html.dark .tut-textarea { background: rgba(255,255,255,0.04); border-color: rgba(255,255,255,0.08); color: #e5e5e5; }
html.dark .tut-cancel-btn { background: rgba(255,255,255,0.05); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
</style>
