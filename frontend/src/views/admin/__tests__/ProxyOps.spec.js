import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 代理运营页 × 「共享 socks」Tab 的「分配方式」列。
 *
 * 只钉一件事：独占的权益要标出「独占 · 用户 #id」，共享标「共享」，
 * 后端还没返回 allocation 的旧数据留「—」（不猜成独占，否则客服会按独占去排查）。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import ProxyOps from '@/views/admin/ProxyOps.vue'

const SOCKS_GROUPS = [
  {
    socks_id: 3,
    host: '1.2.3.4',
    port: 1080,
    country: 'HK',
    socks_active: true,
    user_count: 2,
    total_count: 2,
    next_expiry: '2026-12-01T00:00:00Z',
    users: [
      {
        entitlement_id: 900, user_id: 42, email: 'a@example.com', sku_name: '独占 30G',
        status: 'active', expires_at: '2026-12-01T00:00:00Z', allocation: 'exclusive',
      },
      {
        entitlement_id: 901, user_id: 43, email: 'b@example.com', sku_name: '共享 10G',
        status: 'active', expires_at: '2026-12-01T00:00:00Z', allocation: 'shared',
      },
      {
        entitlement_id: 902, user_id: 44, email: 'c@example.com', sku_name: '老规格',
        status: 'active', expires_at: '2026-12-01T00:00:00Z',
      },
    ],
  },
]

beforeEach(() => {
  vi.clearAllMocks()
  adminApi.proxyOpsOverview = vi.fn(() => Promise.resolve({ stats: {}, socks_options: [] }))
  adminApi.proxyOpsAccounts = vi.fn(() => Promise.resolve([]))
  adminApi.proxyOpsSocksGroups = vi.fn(() => Promise.resolve(SOCKS_GROUPS))
})

const mounted = []
afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
  document.body.innerHTML = ''
})

const mountPage = async () => {
  const wrapper = mount(ProxyOps, { attachTo: document.body })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

describe('ProxyOps.vue — 共享 socks 的分配方式列', () => {
  it('独占标出用户、共享标共享、缺字段留「—」', async () => {
    const wrapper = await mountPage()

    // 切到「共享 socks」Tab
    const socksTab = wrapper.findAll('.po-tab').find((b) => b.text().includes('共享 socks'))
    await socksTab.trigger('click')
    await flushPromises()

    expect(adminApi.proxyOpsSocksGroups).toHaveBeenCalledTimes(1)
    expect(wrapper.findAll('thead th').map((th) => th.text())).toContain('分配方式')

    const rows = wrapper.findAll('tbody tr')
    const row100 = rows.find((tr) => tr.text().includes('a@example.com'))
    expect(row100.text()).toContain('独占 · 用户 #42')

    const shared = rows.find((tr) => tr.text().includes('b@example.com'))
    expect(shared.text()).toContain('共享')
    expect(shared.text()).not.toContain('独占')

    const legacy = rows.find((tr) => tr.text().includes('c@example.com'))
    expect(legacy.text()).toContain('—')
    expect(legacy.text()).not.toContain('独占')
  })
})
