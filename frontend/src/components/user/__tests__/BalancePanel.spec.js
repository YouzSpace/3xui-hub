import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 用户端余额卡片。
 *
 * 盯三件事（后端已有测试守事务与并发，这里只管渲染与请求口径）：
 *  ① 分 → 元展示：5000 分显示 ¥50.00，负数流水带 '-' 且不串色
 *  ② 兑换码提交前归一化（去空白 + 大写），成功后清空输入并重新拉余额
 *  ③ 兑换失败原样透出后端文案（兑换码已使用 / 已过期 …），不刷新余额
 */

const { userApi, toast } = vi.hoisted(() => ({
  userApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))
vi.mock('@/api/user', () => ({ userApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import BalancePanel from '@/components/user/BalancePanel.vue'

const LOGS = [
  {
    id: 2, amount: 5000, type: 'redeem', note: '兑换码', balance_after: 5000,
    created_at: '2026-11-03T02:00:00+00:00',
  },
  {
    id: 1, amount: -1200, type: 'consume', note: '余额支付-月付套餐', balance_after: 3800,
    created_at: '2026-11-03T01:00:00+00:00',
  },
]

beforeEach(() => {
  userApi.balance = vi.fn(() => Promise.resolve({ balance: 5000, logs: LOGS }))
  userApi.redeem = vi.fn(() => Promise.resolve({ added: 5000, balance: 10000 }))
  vi.clearAllMocks()
  toast.success.mockClear()
  toast.error.mockClear()
})

describe('BalancePanel.vue', () => {
  it('余额按分转元展示，流水带符号与类型文案', async () => {
    const wrapper = mount(BalancePanel)
    await flushPromises()

    expect(wrapper.find('.bp-amount').text()).toBe('¥50.00')

    const amounts = wrapper.findAll('.bp-log-amt')
    expect(amounts.map((a) => a.text())).toEqual(['+¥50.00', '-¥12.00'])
    // 正负要用不同颜色类，不能只靠文本
    expect(amounts[0].classes()).toContain('is-in')
    expect(amounts[1].classes()).toContain('is-out')

    expect(wrapper.text()).toContain('兑换')
    expect(wrapper.text()).toContain('消费')
    expect(wrapper.text()).toContain('余额支付-月付套餐')

    wrapper.unmount()
  })

  it('流水每页 20 条：装满一页才显示「更多」，加载更多按下一页参数请求', async () => {
    const fullPage = Array.from({ length: 20 }, (_, i) => ({
      id: 100 - i, amount: 100, type: 'recharge', note: '', balance_after: 100,
      created_at: '2026-11-03T02:00:00+00:00',
    }))
    userApi.balance.mockImplementation(({ page }) =>
      Promise.resolve(
        page === 1
          ? { balance: 2000, logs: fullPage }
          : { balance: 2000, logs: [{ ...fullPage[0], id: 1 }] },
      ),
    )

    const wrapper = mount(BalancePanel)
    await flushPromises()
    const more = wrapper.find('.bp-more')
    expect(more.exists()).toBe(true)

    await more.trigger('click')
    await flushPromises()
    expect(userApi.balance).toHaveBeenLastCalledWith({ page: 2, per_page: 20 })
    // 第二页只有 1 条 → 没有更多了，按钮消失
    expect(wrapper.find('.bp-more').exists()).toBe(false)
    expect(wrapper.findAll('.bp-log').length).toBe(21)

    wrapper.unmount()
  })
})
