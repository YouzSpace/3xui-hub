<script setup>
/**
 * AnnouncementSheet — 公告弹层（上下居中）。
 * 「不再提示」把当前公告全文写入 localStorage，公告换新即再次自动弹出。
 */
import { computed } from 'vue'
import { useSiteConfigStore } from '@/stores/siteConfig'
import BottomSheet from '@/components/user/BottomSheet.vue'

defineProps({
  modelValue: { type: Boolean, default: false },
})
const emit = defineEmits(['update:modelValue'])

const siteConfig = useSiteConfigStore()
const content = computed(() => (siteConfig.config.announcement || '').trim())

const dismissForever = () => {
  try { localStorage.setItem('announcement_dismissed', content.value) } catch (e) { /* 忽略隐私模式等异常 */ }
  emit('update:modelValue', false)
}
</script>

<template>
  <BottomSheet
    :model-value="modelValue"
    title="系统公告"
    max-width="480px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <p class="as-content">{{ content || '暂无公告' }}</p>
    <template #footer>
      <button type="button" class="as-ok uu-press" @click="dismissForever">不再提示</button>
      <button type="button" class="as-close uu-press" @click="emit('update:modelValue', false)">关闭</button>
    </template>
  </BottomSheet>
</template>

<style scoped>
.as-content {
  margin: 0;
  font-size: 15px;
  line-height: 1.7;
  color: var(--lbl2);
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 46vh;
  overflow-y: auto;
}
.as-ok {
  width: 100%;
  padding: 14px;
  border: none;
  border-radius: 16px;
  background: var(--blue);
  color: #fff;
  font-size: 17px;
  font-weight: 600;
  font-family: inherit;
}
.as-close {
  width: 100%;
  padding: 14px;
  border: none;
  border-radius: 16px;
  background: var(--fill3);
  color: var(--lbl1);
  font-size: 17px;
  font-weight: 600;
  font-family: inherit;
}
</style>
