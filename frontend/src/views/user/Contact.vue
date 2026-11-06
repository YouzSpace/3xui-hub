<script setup>
/**
 * Contact — 用户端「反馈与联系」二级页。
 *
 * 数据：GET /api/site-config 的 feedback_links（后台「网站信息」页维护），
 * JSON 数组 [{title, type, url}]：
 *   - type=link：一行，点击整行 window.open 跳转
 *   - type=qr  ：一行带二维码图片（URL 直接 <img>），点击弹大图（长按保存）
 *
 * feedback_links 未配置 / 为空 → 显示空态（More 页此时也不会把入口换成二级页）。
 */
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import ListGroup from '@/components/user/ListGroup.vue'
import ListRow from '@/components/user/ListRow.vue'
import UserIcon from '@/components/user/UserIcon.vue'

const router = useRouter()
const items = ref([])
const loaded = ref(false)

/** 二维码大图预览（点击行打开，长按保存）。 */
const previewUrl = ref('')
const openPreview = (url) => { previewUrl.value = url }
const closePreview = () => { previewUrl.value = '' }

const openLink = (url) => {
  if (url) window.open(url, '_blank')
}

const parsed = computed(() => {
  try {
    const raw = JSON.parse(items.value || '[]')
    return Array.isArray(raw)
      ? raw.filter((it) => it && it.title && it.url)
      : []
  } catch {
    return []
  }
})

onMounted(async () => {
  try {
    const res = await fetch('/api/site-config')
    const body = await res.json()
    items.value = body?.data?.feedback_links || ''
  } catch {
    items.value = ''
  } finally {
    loaded.value = true
  }
})
</script>

<template>
  <div class="ct uu-slide-up">
    <ListGroup v-if="parsed.length" label="联系我们">
      <template v-for="(it, i) in parsed" :key="i">
        <!-- 链接型：点整行跳转 -->
        <ListRow
          v-if="it.type !== 'qr'"
          :label="it.title"
          :sub="it.url"
          :last="i === parsed.length - 1"
          @press="openLink(it.url)"
        >
          <template #icon><UserIcon name="globe" :size="20" /></template>
        </ListRow>
        <!-- 二维码型：点击看大图 -->
        <ListRow
          v-else
          :label="it.title"
          sub="点击查看二维码"
          :last="i === parsed.length - 1"
          @press="openPreview(it.url)"
        >
          <template #icon><UserIcon name="qr" :size="20" /></template>
        </ListRow>
      </template>
    </ListGroup>
    <ListGroup v-else label="联系我们">
      <p class="ct-empty">{{ loaded ? '暂无联系方式' : '加载中...' }}</p>
    </ListGroup>

    <!-- 二维码大图弹层（点击遮罩关闭） -->
    <Teleport to="body">
      <div v-if="previewUrl" class="ct-mask" @click="closePreview">
        <div class="ct-qr-box" @click.stop>
          <img :src="previewUrl" class="ct-qr-img" alt="二维码" />
          <p class="ct-qr-tip">长按或截图保存</p>
        </div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.ct { padding-top: 8px; }
.ct-empty { margin: 0; padding: 32px 0; text-align: center; color: var(--lbl3, #999); font-size: 14px; }

.ct-mask {
  position: fixed;
  inset: 0;
  z-index: 999;
  background: rgba(0, 0, 0, 0.6);
  display: flex;
  align-items: center;
  justify-content: center;
  animation: ctFade 0.15s ease-out;
}
@keyframes ctFade { from { opacity: 0; } to { opacity: 1; } }
.ct-qr-box {
  background: #fff;
  border-radius: 16px;
  padding: 20px;
  max-width: min(320px, 80vw);
  text-align: center;
}
.ct-qr-img {
  display: block;
  width: 100%;
  border-radius: 8px;
}
.ct-qr-tip {
  margin: 12px 0 0;
  font-size: 13px;
  color: #999;
}
</style>
