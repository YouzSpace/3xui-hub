import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 用户端邀请页 · 返佣区（GET /api/affiliate）。
 *
 * 盯三件事：
 *  ① 累计返佣（分）与比例（0.1 → 10%）如实展示，被邀请人列表用打码邮箱 + 贡献 + 加入时间；
 *  ② 返佣接口挂了只影响返佣那块 —— 邀请码必须照常显示（两个接口各自加载）；
 *  ③ 失败块带真实原因 + 重试，重试真的重发请求。
 */

const { userApi, toast } = vi.hoisted(() => ({
  userApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))
vi.mock('@/api/user', () => ({ userApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import Invite from '@/views/user/Invite.vue'

const INVITE = {
  invite: {
    code: 'ABC123',
    discount: 0.9,
    enabled: true,
    invited_count: 2,
    next_refresh_at: '2026-12-03T00:00:00+08:00',
  },
  redeem_codes: [],
}

const AFFILIATE = {
  rate: 0.1,
  total_rebate: 800,
  invited_count: 2,
  invitees: [
    { user_id: 9, email: 'zh***@qq.com', rebate: 800, invited_at: '2026-11-01T10:00:00+08:00' },
    { user_id: 10, email: 'li***@gmail.com', rebate: 0, invited_at: '2026-11-02T10:00:00+08:00' },
  ],
}

const mountPage = async () => {
  const wrapper = mount(Invite)
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  userApi.myInvite = vi.fn(() => Promise.resolve(INVITE))
  userApi.myAffiliate = vi.fn(() => Promise.resolve(AFFILIATE))
  vi.clearAllMocks()
  toast.success.mockClear()
  toast.error.mockClear()
})

describe('Invite.vue — 返佣区', () => {
  it('累计返佣按分转元、比例转百分比，被邀请人列出打码邮箱与贡献', async () => {
    const wrapper = await mountPage()

    expect(wrapper.text()).toContain('累计返佣')
    expect(wrapper.find('.iv-rebate-amount').text()).toBe('¥8.00')
    expect(wrapper.text()).toContain('10%')

    expect(wrapper.findAll('.iv-invitee-email').map((e) => e.text())).toEqual([
      'zh***@qq.com',
      'li***@gmail.com',
    ])
    // 没产生过返佣的邀请人显示 ¥0.00（不是隐藏）
    expect(wrapper.findAll('.iv-invitee-rebate').map((e) => e.text())).toEqual(['¥8.00', '¥0.00'])
    expect(wrapper.findAll('.iv-invitee-time')[0].text()).toContain('加入')

    // 邀请码不受返佣接口影响
    expect(wrapper.text()).toContain('ABC123')

    wrapper.unmount()
  })

  it('没有邀请过好友时给出空态', async () => {
    userApi.myAffiliate.mockResolvedValue({ rate: 0.1, total_rebate: 0, invited_count: 0, invitees: [] })
    const wrapper = await mountPage()

    expect(wrapper.find('.iv-rebate-amount').text()).toBe('¥0.00')
    expect(wrapper.text()).toContain('还没有邀请好友')

    wrapper.unmount()
  })

  it('返佣接口失败时只坏返佣那块：邀请码仍显示，可重试', async () => {
    userApi.myAffiliate.mockRejectedValue(new Error('服务器错误'))
    const wrapper = await mountPage()

    expect(wrapper.text()).toContain('返佣数据加载失败：服务器错误')
    expect(wrapper.text()).toContain('ABC123')

    userApi.myAffiliate.mockResolvedValue(AFFILIATE)
    const retry = wrapper.findAll('button').find((b) => b.text().includes('重新加载') && b.classes().includes('iv-retry'))
    await retry.trigger('click')
    await flushPromises()

    expect(userApi.myAffiliate).toHaveBeenCalledTimes(2)
    expect(wrapper.find('.iv-rebate-amount').text()).toBe('¥8.00')

    wrapper.unmount()
  })
})
