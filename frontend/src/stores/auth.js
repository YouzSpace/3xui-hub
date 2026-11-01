import { defineStore } from 'pinia'
import { ref } from 'vue'
import { userApi } from '@/api/user'

/**
 * 用户端认证 + 当前用户状态。
 * access_token 存 localStorage，请求由 client 拦截器注入 Bearer。
 */
export const useAuthStore = defineStore('auth', () => {
  const token = ref(localStorage.getItem('access_token') || '')
  const user = ref(null)
  const ready = ref(false)

  const setToken = (t) => {
    token.value = t
    if (t) localStorage.setItem('access_token', t)
    else localStorage.removeItem('access_token')
  }

  const login = async (inputToken) => {
    const data = await userApi.login(inputToken)
    setToken(data.access_token)
    return data
  }

  const loginEmail = async (email, password) => {
    const data = await userApi.loginEmail(email, password)
    setToken(data.access_token)
    return data
  }

  const register = async (email, password, captcha) => {
    const data = await userApi.register(email, password, captcha || '')
    setToken(data.access_token)
    return data
  }

  const registerWithEmailVerify = async (email, password, code) => {
    const data = await userApi.register(email, password, '', code)
    setToken(data.access_token)
    return data
  }

  const fetchMe = async () => {
    if (!token.value) {
      user.value = null
      ready.value = true
      return null
    }
    try {
      user.value = await userApi.me()
    } catch (e) {
      user.value = null
      setToken('')
    } finally {
      ready.value = true
    }
    return user.value
  }

  const logout = () => {
    setToken('')
    user.value = null
  }

  return { token, user, ready, login, loginEmail, register, registerWithEmailVerify, fetchMe, logout, setToken }
})
