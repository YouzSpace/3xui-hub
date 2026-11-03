import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 用户端余额充值弹层（在线支付 / 兑换码两个 tab）。
 *
 * 盯三件事（兑换逻辑从 BalancePanel 移到这里）：
 *  ① 默认「在线支付」tab，兑换码输入框不露出；点「兑换码」tab 才显示
 *  ② 兑换码提交前归一化（去空白 + 大写），成功到账后 emit redeemed 并关闭弹层
 *  ③ 兑换失败原样透出后端文案，不 emit redeemed
 */

const { userApi, toast } = vi.hoisted(() => ({
  userApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))
vi.mock('@/api/user', () => ({ userApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import BalanceRechargeSheet from '@/components/user/BalanceRechargeSheet.vue'

// BottomSheet 用 Teleport to body，测试时 stub 掉让内容留在 wrapper 里
const mountSheet = () =>
  mount(BalanceRechargeSheet, {
    props: { modelValue: true },
    global: { stubs: { teleport: true } },
  })

const switchToRedeem = async (wrapper) => {
  const tabs = wrapper.findAll('.brs-tab')
  const redeemTab = tabs.find((t) => t.text() === '兑换码')
  await redeemTab.trigger('click')
  await flushPromises()
}

beforeEach(() => {
  userApi.paymentMethods = vi.fn(() => Promise.resolve([]))
  userApi.redeem = vi.fn(() => Promise.resolve({ added: 5000, balance: 10000 }))
  vi.clearAllMocks()
  toast.success.mockClear()
  toast.error.mockClear()
})

describe('BalanceRechargeSheet.vue', () => {
  it('默认在线支付，兑换码输入框不露出；点「兑换码」tab 后才显示', async () => {
    const wrapper = mountSheet()
    await flushPromises()

    // 默认在线支付：没有兑换码输入框
    expect(wrapper.find('.brs-redeem-input').exists()).toBe(false)

    await switchToRedeem(wrapper)
    expect(wrapper.find('.brs-redeem-input').exists()).toBe(true)

    wrapper.unmount()
  })

  it('兑换码提交前去空白转大写，成功到账后 emit redeemed', async () => {
    const wrapper = mountSheet()
    await flushPromises()
    await switchToRedeem(wrapper)

    await wrapper.find('.brs-redeem-input').setValue('  abcd-efgh-0000  ')
    await wrapper.find('.brs-submit').trigger('click')
    await flushPromises()

    expect(userApi.redeem).toHaveBeenCalledWith('ABCD-EFGH-0000')
    expect(toast.success).toHaveBeenCalledWith(expect.stringContaining('¥50.00'))
    expect(wrapper.emitted('redeemed')).toBeTruthy()

    wrapper.unmount()
  })

  it('兑换失败原样透出后端文案，不 emit redeemed', async () => {
    userApi.redeem.mockRejectedValue({ message: '兑换码已使用' })

    const wrapper = mountSheet()
    await flushPromises()
    await switchToRedeem(wrapper)

    await wrapper.find('.brs-redeem-input').setValue('USED-CODE')
    await wrapper.find('.brs-submit').trigger('click')
    await flushPromises()

    expect(toast.error).toHaveBeenCalledWith('兑换码已使用')
    expect(wrapper.emitted('redeemed')).toBeFalsy()

    wrapper.unmount()
  })
})
