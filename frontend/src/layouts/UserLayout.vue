<script setup>
/**
 * UserLayout — 用户端极简外壳，单列 ≤720px，移动端友好。
 */
import Icon from '@/components/ui/Icon.vue'
import Button from '@/components/ui/Button.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useAuthStore } from '@/stores/auth'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { useRouter } from 'vue-router'

const auth = useAuthStore()
const siteConfig = useSiteConfigStore()
const router = useRouter()

const onLogout = () => {
  auth.logout()
  router.push({ name: 'user-login' })
}
</script>

<template>
  <div class="ul-wrap">
    <header class="ul-header">
      <div class="ul-brand">
        <span class="ul-logo">
          <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="logo" class="ul-logo-img" />
          <Icon v-else name="shield" :size="20" />
        </span>
        <span class="ul-name">{{ siteConfig.title }}</span>
      </div>
      <div class="ul-header-actions">
        <ThemeToggle />
        <Button variant="ghost" size="sm" @click="onLogout">
          <Icon name="logout" :size="16" /> 退出
        </Button>
      </div>
    </header>
    <main class="ul-main">
      <RouterView />
    </main>
  </div>
</template>

<style scoped>
.ul-wrap {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
}
.ul-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--space-4) var(--space-5);
  border-bottom: 1px solid var(--border-subtle);
}
.ul-brand {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.ul-logo {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  border-radius: var(--radius-md);
  background: var(--accent-muted);
  color: var(--accent);
}
.ul-logo-img {
  width: 32px;
  height: 32px;
  object-fit: contain;
  border-radius: var(--radius-md);
}
.ul-name {
  font-weight: var(--font-semibold);
  font-size: var(--text-md);
}
.ul-header-actions {
  display: flex;
  align-items: center;
  gap: var(--space-1);
}
.ul-main {
  flex: 1;
  display: flex;
  justify-content: center;
  padding: var(--space-6) var(--space-4);
}
.ul-main > :deep(*) {
  width: 100%;
  max-width: 720px;
}
</style>
