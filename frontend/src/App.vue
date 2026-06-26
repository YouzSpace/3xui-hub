<script setup>
/**
 * ControlHub root shell — router-driven。
 * Toast 全局挂载（任何页面可调 useToast）。
 */
import Toast from '@/components/ui/Toast.vue'
import { useToast } from '@/components/ui/Toast.vue'
import { provide, onMounted } from 'vue'
import { useSiteConfigStore } from '@/stores/siteConfig'

// 让任意后代可 inject('toast')
provide('toast', useToast())

const siteConfig = useSiteConfigStore()
onMounted(() => siteConfig.fetch())
</script>

<template>
  <RouterView />
  <Toast />
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
</style>
