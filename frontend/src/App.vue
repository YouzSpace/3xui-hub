<script setup>
/**
 * ControlHub root shell — router-driven。
 * Toast 全局挂载（任何页面可调 useToast）。
 */
import Toast from '@/components/ui/Toast.vue'
import { useToast } from '@/components/ui/Toast.vue'
import { provide, ref, onMounted } from 'vue'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { version } from '../package.json'

// 让任意后代可 inject('toast')
provide('toast', useToast())

const siteConfig = useSiteConfigStore()
const appReady = ref(false)
onMounted(async () => {
  siteConfig.fetch() // 不 await，后台加载
  // 短暂显示 loading 后立即渲染页面
  setTimeout(() => { appReady.value = true }, 300)
})
</script>

<template>
  <div v-if="!appReady" class="app-loading">
    <div class="app-loading-spinner"></div>
  </div>
  <RouterView v-else />
  <Toast />
  <div class="app-version">v{{ version }}</div>
</template>

<style>
html,
body,
#app {
  height: 100%;
  margin: 0;
}
#app {
  background: var(--bg-base);
  color: var(--text-primary);
  font-family: var(--font-sans);
}

.app-loading {
  position: fixed;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  background: #f5f7fa;
  z-index: 9999;
}
.app-loading-spinner {
  width: 32px;
  height: 32px;
  border: 3px solid rgba(0,0,0,0.08);
  border-top-color: #2563eb;
  border-radius: 50%;
  animation: app-spin 0.8s linear infinite;
}
@keyframes app-spin { to { transform: rotate(360deg); } }

html.dark .app-loading { background: #171a1d; }
html.dark .app-loading-spinner { border-color: rgba(255,255,255,0.1); border-top-color: #60a5fa; }

.app-version {
  position: fixed;
  bottom: 12px;
  right: 12px;
  font-size: 11px;
  color: rgba(0,0,0,0.3);
  z-index: 100;
  pointer-events: none;
  user-select: none;
}
html.dark .app-version { color: rgba(255,255,255,0.2); }
</style>
