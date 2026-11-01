import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import AdminLogin from '@/views/admin/Login.vue'

vi.mock('@/components/ui/Toast.vue', () => ({
  default: { template: '<div />' },
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))

describe('Admin Login.vue', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
  })

  it('renders title and inputs', () => {
    const router = createRouter({ history: createMemoryHistory(), routes: [] })
    const wrapper = mount(AdminLogin, { global: { plugins: [router] } })
    expect(wrapper.text()).toContain('ControlHub Admin')
    expect(wrapper.findAll('input').length).toBeGreaterThanOrEqual(2)
  })
})
