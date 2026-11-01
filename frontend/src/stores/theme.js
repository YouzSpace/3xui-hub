import { defineStore } from 'pinia'
import { ref } from 'vue'

const STORAGE_KEY = 'ch-theme'

/**
 * 主题切换。默认亮色；选暗色时给 <html> 加 .dark 类覆盖 tokens。
 * 持久化到 localStorage，挂载前由 index.html 内联脚本预置类避免闪烁。
 */
export const useThemeStore = defineStore('theme', () => {
  const isDark = ref(localStorage.getItem(STORAGE_KEY) === 'dark')

  const apply = () => {
    const el = document.documentElement
    el.classList.toggle('light', !isDark.value)
    el.classList.toggle('dark', isDark.value)
  }

  const toggle = () => {
    isDark.value = !isDark.value
    localStorage.setItem(STORAGE_KEY, isDark.value ? 'dark' : 'light')
    apply()
  }

  const init = () => apply()

  return { isDark, toggle, init }
})
