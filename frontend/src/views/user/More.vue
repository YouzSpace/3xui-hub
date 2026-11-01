<script setup>
/**
 * 我的（新 UI）：资料卡 / 偏好设置 / 服务与支持 / 退出登录
 * 使用文档已改为全屏二级页（user-tutorials），此页只留入口。
 * 反馈链接、公告均为现有真实功能。
 */
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { useThemeStore } from '@/stores/theme'
import { userApi } from '@/api/user'
import UserIcon from '@/components/user/UserIcon.vue'
import ListGroup from '@/components/user/ListGroup.vue'
import ListRow from '@/components/user/ListRow.vue'

const auth = useAuthStore()
const siteConfig = useSiteConfigStore()
const theme = useThemeStore()
const router = useRouter()

const user = computed(() => auth.user || {})
const nodes = ref([])

// 是否买过代理：决定「我的代理」入口是否出现
// （商城可以关闭售卖，但买过的人随时要能看自己的凭据和 IP）
const hasProxy = ref(false)
userApi
  .proxyEntitlements()
  .then((list) => {
    hasProxy.value = Array.isArray(list) && list.length > 0
  })
  .catch(() => {
    hasProxy.value = false
  })

const avatarLetter = computed(() => (user.value.email?.[0] || 'U').toUpperCase())
const planLabel = computed(() => user.value.plan_name || '无套餐')
const expiredLabel = computed(() => {
  if (!user.value.expired_at) return '永不过期'
  return new Date(user.value.expired_at).toLocaleDateString()
})
const onlineCount = computed(() => nodes.value.filter((n) => n.status === 'online').length)

// 公告：跳转全屏公告页
const openAnnouncement = () => {
  router.push({ name: 'user-announcement' })
}

const openFeedback = () => {
  const link = siteConfig.config.feedback_link
  if (link) window.open(link, '_blank')
}

const onLogout = () => {
  auth.logout()
  router.push({ name: 'user-login' })
}

onMounted(async () => {
  try { nodes.value = await userApi.nodes() } catch (e) { nodes.value = [] }
})
</script>

<template>
  <div class="more uu-slide-up">
    <!-- 资料卡 -->
    <section class="mp uu-mat-regular">
      <div class="mp-head">
        <div class="mp-avatar">{{ avatarLetter }}</div>
        <div class="mp-id">
          <p class="mp-email">{{ user.email || '—' }}</p>
        </div>
        <span v-if="user.plan_name" class="mp-badge">{{ user.plan_name }}</span>
      </div>

      <div class="mp-stats">
        <div class="mp-stat">
          <p class="mp-stat-label">套餐</p>
          <p class="mp-stat-value">{{ planLabel }}</p>
        </div>
        <div class="mp-stat">
          <p class="mp-stat-label">到期</p>
          <p class="mp-stat-value">{{ expiredLabel }}</p>
        </div>
        <div class="mp-stat">
          <p class="mp-stat-label">节点在线</p>
          <p class="mp-stat-value">{{ onlineCount }}</p>
        </div>
      </div>
    </section>

    <!-- 偏好设置 -->
    <ListGroup label="偏好设置">
      <ListRow
        :label="theme.isDark ? '切换浅色模式' : '切换深色模式'"
        :sub="theme.isDark ? '当前：深色' : '当前：浅色'"
        :icon-bg="theme.isDark ? 'var(--orange-t)' : 'var(--purple-t)'"
        :icon-color="theme.isDark ? 'var(--orange)' : 'var(--purple)'"
        last
        @press="theme.toggle()"
      >
        <template #icon>
          <UserIcon :name="theme.isDark ? 'sun' : 'moon'" :size="20" />
        </template>
        <template #right>
          <span class="mp-switch" :class="{ 'mp-switch--on': theme.isDark }">
            <span class="mp-switch-knob"></span>
          </span>
        </template>
      </ListRow>
    </ListGroup>

    <!-- 服务与支持 -->
    <ListGroup label="服务与支持">
      <ListRow
        label="我的订单"
        sub="查看历史订单与支付状态"
        @press="router.push({ name: 'user-orders' })"
      >
        <template #icon><UserIcon name="cart" :size="20" /></template>
      </ListRow>
      <!-- 买过代理才显示：商城关闭时侧栏没有入口，这里是唯一的入口 -->
      <ListRow
        v-if="hasProxy"
        label="我的代理"
        sub="账号密码 · IP 列表 · 到期时间"
        @press="router.push({ name: 'user-proxy-mine' })"
      >
        <template #icon><UserIcon name="globe" :size="20" /></template>
      </ListRow>
      <ListRow
        label="邀请好友"
        sub="我的邀请码 · 兑换码"
        @press="router.push({ name: 'user-invite' })"
      >
        <template #icon><UserIcon name="person" :size="20" /></template>
      </ListRow>
      <ListRow
        label="使用文档"
        sub="客户端配置 · 常见问题"
        @press="router.push({ name: 'user-tutorials' })"
      >
        <template #icon><UserIcon name="doc" :size="20" /></template>
      </ListRow>
      <ListRow
        v-if="siteConfig.config.feedback_link"
        label="反馈与联系"
        sub="问题反馈与联系我们"
        @press="openFeedback"
      >
        <template #icon><UserIcon name="mail" :size="20" /></template>
      </ListRow>
      <ListRow
        label="系统公告"
        sub="最新服务动态通知"
        last
        @press="openAnnouncement"
      >
        <template #icon><UserIcon name="bell" :size="20" /></template>
      </ListRow>
    </ListGroup>

    <!-- 退出登录 -->
    <ListGroup>
      <ListRow
        label="退出登录"
        danger
        last
        icon-bg="var(--red-t)"
        icon-color="var(--red)"
        @press="onLogout"
      >
        <template #icon><UserIcon name="logout" :size="20" /></template>
      </ListRow>
    </ListGroup>
  </div>
</template>

<style scoped>
.more {
  display: flex;
  flex-direction: column;
  gap: 20px;
  padding-bottom: 8px;
}

/* ===== 资料卡 ===== */
.mp {
  border-radius: 24px;
  overflow: hidden;
}
.mp-head {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 20px;
  background: linear-gradient(135deg, rgba(10, 132, 255, 0.18) 0%, transparent 60%);
}
.mp-avatar {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 64px;
  height: 64px;
  border-radius: 16px;
  background: var(--blue);
  color: #fff;
  font-size: 26px;
  font-weight: 700;
  letter-spacing: -1px;
  flex-shrink: 0;
}
.mp-id {
  flex: 1;
  min-width: 0;
}
.mp-email {
  margin: 0;
  font-size: 17px;
  font-weight: 700;
  letter-spacing: -0.3px;
  color: var(--lbl1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.mp-badge {
  padding: 4px 10px;
  border-radius: 999px;
  background: var(--blue);
  color: #fff;
  font-size: 12px;
  font-weight: 700;
  white-space: nowrap;
  flex-shrink: 0;
}
.mp-stats {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  border-top: 1px solid var(--sep);
}
.mp-stat {
  padding: 14px 16px;
}
.mp-stat + .mp-stat {
  border-left: 1px solid var(--sep);
}
.mp-stat-label {
  margin: 0;
  font-size: 11px;
  color: var(--lbl3);
  letter-spacing: 0.03em;
}
.mp-stat-value {
  margin: 3px 0 0;
  font-size: 14px;
  font-weight: 600;
  color: var(--lbl1);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* ===== 主题开关 ===== */
.mp-switch {
  display: flex;
  align-items: center;
  width: 48px;
  height: 28px;
  padding: 2px 3px;
  border-radius: 999px;
  background: var(--blue);
  justify-content: flex-start;
  pointer-events: none;
}
.mp-switch--on {
  justify-content: flex-end;
}
.mp-switch-knob {
  width: 23px;
  height: 23px;
  border-radius: 999px;
  background: #fff;
  box-shadow: 0 2px 4px rgba(0, 0, 0, 0.3);
}

@media (max-width: 480px) {
  .mp-head {
    flex-wrap: wrap;
  }
}
</style>
