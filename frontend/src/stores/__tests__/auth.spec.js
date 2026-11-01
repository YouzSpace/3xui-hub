import { describe, it, expect, beforeEach, vi } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'

// 顶层 mock：userApi.login 返回固定 access_token
const loginMock = vi.fn().mockResolvedValue({ access_token: 'tok-xyz' })
vi.mock('@/api/user', () => ({
  userApi: {
    login: loginMock,
    me: vi.fn(),
    nodes: vi.fn(),
    switchProtocol: vi.fn(),
  },
}))

const { useAuthStore } = await import('@/stores/auth')

describe('useAuthStore', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    loginMock.mockClear()
  })

  it('setToken persists to localStorage', () => {
    const auth = useAuthStore()
    auth.setToken('abc')
    expect(auth.token).toBe('abc')
    expect(localStorage.getItem('access_token')).toBe('abc')
  })

  it('logout clears token and user', () => {
    const auth = useAuthStore()
    auth.setToken('abc')
    auth.user = { id: 1 }
    auth.logout()
    expect(auth.token).toBe('')
    expect(auth.user).toBeNull()
    expect(localStorage.getItem('access_token')).toBeNull()
  })

  it('fetchMe returns null without token and marks ready', async () => {
    const auth = useAuthStore()
    const res = await auth.fetchMe()
    expect(res).toBeNull()
    expect(auth.user).toBeNull()
    expect(auth.ready).toBe(true)
  })

  it('login sets token from response', async () => {
    const auth = useAuthStore()
    const data = await auth.login('sub_input')
    expect(data.access_token).toBe('tok-xyz')
    expect(auth.token).toBe('tok-xyz')
    expect(loginMock).toHaveBeenCalledWith('sub_input')
  })
})
