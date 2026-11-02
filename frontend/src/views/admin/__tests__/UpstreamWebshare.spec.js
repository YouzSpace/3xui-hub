import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import UpstreamWebshare from '@/views/admin/UpstreamWebshare.vue'
import { adminApi } from '@/api/admin'

/**
 * 上游 · Webshare 页面接线。
 *
 * 覆盖：
 *  ① 账号 + 套餐 + 子用户三段数据能渲染（字段名与后端对齐，不再全 null）
 *  ② 开号成功后**必须当场弹出初始密码**（后端只在这一次响应里给，关掉就没了）
 *  ③ 停用后弹新密码
 *  ④ 列表渲染里不出现密码
 */

vi.mock('@/components/ui/Toast.vue', () => ({
  default: { template: '<div />' },
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))

vi.mock('@/api/admin', () => ({
  adminApi: {
    listWebshareProviders: vi.fn(),
    listWebsharePlans: vi.fn(),
    listWebshareAccounts: vi.fn(),
    createWebshareAccount: vi.fn(),
    disableWebshareAccount: vi.fn(),
    deleteWebshareAccount: vi.fn(),
    pullWebshareUsage: vi.fn(),
    pullWebshareUsageBatch: vi.fn(),
    createWebshareProvider: vi.fn(),
    updateWebshareProvider: vi.fn(),
    deleteWebshareProvider: vi.fn(),
    testWebshareProvider: vi.fn(),
  },
}))

const provider = {
  id: 7,
  name: '主账号',
  driver: 'webshare',
  enabled: true,
  last_status: 'success',
  last_error: null,
  has_token: true,
  account_count: 1,
  last_checked_at: '2026-10-23T08:20:00+00:00',
}

const plan = {
  id: 14633864,
  status: 'active',
  bandwidth_limit: 250,
  proxy_count: 2,
  proxy_type: 'shared',
  countries: { GB: 1, ZZ: 1 },
  subusers_available: 2,
  monthly_price: 1,
}

const account = {
  id: 3,
  upstream_id: '115781',
  label: '沐木',
  username: 'mm-abc123',
  user_id: null,
  proxy_limit: 200,
  max_thread_count: 500,
  proxy_countries: null,
  used_bytes: 14155206532,
  status: 'active',
  last_synced_at: '2026-10-23T08:20:00+00:00',
}

const mountPage = async () => {
  const wrapper = mount(UpstreamWebshare)
  await flushPromises()
  return wrapper
}

describe('UpstreamWebshare.vue — 前端接线', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
    adminApi.listWebshareProviders.mockResolvedValue({ items: [provider] })
    adminApi.listWebsharePlans.mockResolvedValue({ items: [plan] })
    adminApi.listWebshareAccounts.mockResolvedValue({ items: [account] })
  })

  it('渲染账号 / 套餐 / 子用户三段数据', async () => {
    const wrapper = await mountPage()

    const text = wrapper.text()
    // 账号
    expect(text).toContain('主账号')
    expect(text).toContain('连接正常')
    expect(text).toContain('已配置')
    // 套餐（真实字段：bandwidth_limit / proxy_countries / subusers_available）
    expect(text).toContain('250 GB')
    expect(text).toContain('GB 1')
    expect(text).toContain('可开 2')
    // 子用户
    expect(text).toContain('沐木')
    expect(text).toContain('200 GB')
    expect(text).toContain('13.18 GB')
    // 列表里不能出现任何密码
    expect(text).not.toContain('initial_password')
  })

  it('开号成功后当场弹出初始密码（只给这一次）', async () => {
    adminApi.createWebshareAccount.mockResolvedValue({
      ...account,
      initial_password: 'only-shown-once-pw',
    })

    const wrapper = await mountPage()
    wrapper.vm.accountForm.label = '新用户代理'
    wrapper.vm.accountForm.proxy_limit = 10
    await wrapper.vm.saveAccount()
    // saveAccount 先把按钮切成对勾（成功反馈 900ms）再弹凭据弹窗，
    // 所以等这段延时走完再看 DOM —— 之前漏了这一步，断言永远落在定时器前面。
    await new Promise((r) => setTimeout(r, 950))
    await flushPromises()

    // 请求体只发后端认识的字段
    expect(adminApi.createWebshareAccount).toHaveBeenCalledWith(7, {
      label: '新用户代理',
      proxy_limit: 10,
      max_thread_count: 50,
    })

    // 凭据弹窗打开且带出初始密码（Modal 走 Teleport，内容在 body 上）
    expect(wrapper.vm.credentialOpen).toBe(true)
    expect(wrapper.vm.credential.password).toBe('only-shown-once-pw')
    expect(document.body.textContent).toContain('only-shown-once-pw')
  })

  it('停用后弹出新密码', async () => {
    adminApi.disableWebshareAccount.mockResolvedValue({
      ...account,
      status: 'disabled',
      proxy_limit: 0.01,
      new_password: 'rotated-pw',
    })

    const wrapper = await mountPage()
    // 停用有 confirm 二次确认，测试里直接放行
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    await wrapper.vm.disableAccount(account)
    await flushPromises()

    expect(wrapper.vm.credentialOpen).toBe(true)
    expect(wrapper.vm.credential.password).toBe('rotated-pw')
    expect(document.body.textContent).toContain("rotated-pw")
  })
})
