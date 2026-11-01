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
    register_email_verify: false,
    announcement: '',
    feedback_link: '',
  })
  const loaded = ref(false)

  const title = computed(() => config.value.site_title || 'ControlHub')
  const subtitle = computed(() => config.value.site_subtitle || '')
  const description = computed(() => config.value.site_description || '')
  const keywords = computed(() => config.value.site_keywords || '')
  const logo = computed(() => config.value.site_logo || '')
  const favicon = computed(() => config.value.site_favicon || '')
  // 代理商城总开关：默认关闭，后台「商城商品」页打开后才在用户端出现入口
  const proxyStoreEnabled = computed(
    () => config.value.proxy_store_enabled === true ||
      config.value.proxy_store_enabled === '1' ||
      config.value.proxy_store_enabled === 1
  )
  const registerEmailVerify = computed(() => config.value.register_email_verify === true || config.value.register_email_verify === '1' || config.value.register_email_verify === 1)

  async function fetch() {
    if (loaded.value) return
    try {
      const res = await userApi.siteConfig()
      if (res) {
        config.value = { ...config.value, ...res }
        if (res.site_title) {
          document.title = res.site_subtitle
            ? `${res.site_title} - ${res.site_subtitle}`
            : res.site_title
        }
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
      // 只有真拿到数据才置 loaded：失败时留false，允许下次调用重试。
      // 原写法在这里无条件置 true，一旦首屏请求失败，store 会永久停在
      // 初始空值上（自定义首页读不到内容 → 整页空白），且再也不会重试。
      loaded.value = config.value.site_title !== ''
    }
  }

  async function updateConfig(newConfig) {
    config.value = { ...config.value, ...newConfig }
    if (newConfig.site_title !== undefined) {
      document.title = newConfig.site_subtitle
        ? `${newConfig.site_title} - ${newConfig.site_subtitle}`
        : newConfig.site_title
    }
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

  // loaded 要导出：公开首页（不走 UserLayout）需要在 onMounted 里判断
  // 「配置是否已就位」，否则会读到 store 的初始空值。
  return { config, title, subtitle, description, keywords, logo, favicon, registerEmailVerify, proxyStoreEnabled, loaded, fetch, updateConfig }
})
