<script setup>
/**
 * Tutorials — 使用文档全屏页（二级页面）。
 * 数据来自 userApi.tutorials()（GET /api/tutorials，后端已按 sort 升序）。
 */
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { userApi } from '@/api/user'
import ListGroup from '@/components/user/ListGroup.vue'
import ListRow from '@/components/user/ListRow.vue'
import UserIcon from '@/components/user/UserIcon.vue'

const router = useRouter()
const docs = ref([])

const fmtDate = (s) => {
  const d = new Date(s)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric' })
}

// 后端按 sort 升序返回；此处按 category 分组，分组标题即各文档自身的分类
const groups = ref([])
const groupDocs = (list) => {
  const map = new Map()
  for (const t of list) {
    const cat = (t.category || '').trim() || '默认'
    if (!map.has(cat)) map.set(cat, [])
    map.get(cat).push(t)
  }
  return [...map.entries()].map(([label, items]) => ({ label, items }))
}

const openDoc = (id) => router.push(`/dashboard/tutorial/${id}`)

onMounted(async () => {
  try { docs.value = (await userApi.tutorials()) || [] } catch (e) { docs.value = [] }
  groups.value = groupDocs(docs.value)
})
</script>

<template>
  <div class="td uu-slide-up">
    <template v-if="groups.length">
      <ListGroup
        v-for="g in groups"
        :id="'tut-cat-' + encodeURIComponent(g.label)"
        :key="g.label"
        :label="g.label"
      >
        <ListRow
          v-for="(t, i) in g.items"
          :key="t.id"
          :label="t.title"
          :sub="fmtDate(t.created_at)"
          :last="i === g.items.length - 1"
          @press="openDoc(t.id)"
        >
          <template #icon><UserIcon name="doc" :size="20" /></template>
        </ListRow>
      </ListGroup>
    </template>
    <ListGroup v-else label="使用文档">
      <p class="td-empty">暂无文档，敬请期待。</p>
    </ListGroup>
  </div>
</template>

<style scoped>
.td { display: flex; flex-direction: column; gap: 20px; }
.td-empty { margin: 0; padding: 16px; font-size: 14px; color: var(--lbl3); }
</style>
