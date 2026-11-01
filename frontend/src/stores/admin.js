import { defineStore } from 'pinia'
import { ref } from 'vue'
import { adminApi } from '@/api/admin'

/**
 * Admin 状态：登录态由后端 session 维持，这里只记前端标志位。
 */
export const useAdminStore = defineStore('admin', () => {
  const loggedIn = ref(false)
  const admin = ref(null)

  const login = async (username, password, otp) => {
    const data = await adminApi.login(username, password, otp)
    admin.value = data.admin
    loggedIn.value = true
    return data
  }

  const logout = async () => {
    try {
      await adminApi.logout()
    } catch (e) {
      // 忽略
    }
    loggedIn.value = false
    admin.value = null
  }

  const probe = async () => {
    try {
      await adminApi.listUsers()
      loggedIn.value = true
      return true
    } catch (e) {
      loggedIn.value = false
      return false
    }
  }

  return { loggedIn, admin, login, logout, probe }
})
