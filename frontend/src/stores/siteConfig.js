import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { userApi } from '@/api/user'

export const useSiteConfigStore = defineStore('siteConfig', () => {
  const config = ref({
    site_title: '',
    site_subtitle: '',
    site_description: '',
    site_keywords: '',
    site_logo: '',
    site_favicon: '',
  })
  const loaded = ref(false)

  const title = computed(() => config.value.site_title || 'ControlHub')
  const subtitle = computed(() => config.value.site_subtitle || '')
  const description = computed(() => config.value.site_description || '')
  const keywords = computed(() => config.value.site_keywords || '')
  const logo = computed(() => config.value.site_logo || '')
  const favicon = computed(() => config.value.site_favicon || '')

  async function fetch() {
    if (loaded.value) return
    try {
      const res = await userApi.siteConfig()
      if (res) {
        config.value = { ...config.value, ...res }
        // 更新页面标题
        if (res.site_title) {
          document.title = res.site_subtitle
            ? `${res.site_title} - ${res.site_subtitle}`
            : res.site_title
        }
        // 更新 favicon
        let link = document.querySelector("link[rel~='icon']")
        if (res.site_favicon) {
          if (!link) {
            link = document.createElement('link')
            link.rel = 'icon'
            document.head.appendChild(link)
          }
          link.href = res.site_favicon + (res.site_favicon.includes('?') ? '&' : '?') + '_t=' + Date.now()
        } else if (link) {
          link.remove()
        }
      }
    } catch (e) {
      // 静默失败
    } finally {
      loaded.value = true
    }
  }

  return { config, title, subtitle, description, keywords, logo, favicon, fetch, updateConfig }

  function updateConfig(newConfig) {
    config.value = { ...config.value, ...newConfig }
    // 更新页面标题
    if (newConfig.site_title !== undefined) {
      document.title = newConfig.site_subtitle
        ? `${newConfig.site_title} - ${newConfig.site_subtitle}`
        : newConfig.site_title
    }
    // 更新 favicon
    if (newConfig.site_favicon !== undefined) {
      let link = document.querySelector("link[rel~='icon']")
      if (newConfig.site_favicon) {
        if (!link) {
          link = document.createElement('link')
          link.rel = 'icon'
          document.head.appendChild(link)
        }
        link.href = newConfig.site_favicon + (newConfig.site_favicon.includes('?') ? '&' : '?') + '_t=' + Date.now()
      } else if (link) {
        link.remove()
      }
    }
  }
})
