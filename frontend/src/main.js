import { createApp } from 'vue'
import App from './App.vue'
import router from '@/router'
import pinia from '@/stores'
import { useThemeStore } from '@/stores/theme'

// Design tokens (must load before Tailwind directives in components).
import './assets/styles/tokens.css'
// Tailwind base / components / utilities.
import './assets/styles/main.css'

const app = createApp(App)
app.use(router)
app.use(pinia)

// 应用持久化的主题（index.html 内联脚本已预置类避免闪烁，这里同步 store 状态）
useThemeStore().init()

app.mount('#app')
