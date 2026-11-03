import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 管理端返佣记录（只读流水）。
 *
 * 后端口径：金额是「分」、比例是 decimal(5,4)（0.1000 = 10%）。
 * 这里盯的就是两条展示换算，以及空态 / 失败重试不吞错。
 */

const { adminApi } = vi.hoisted(() => ({ adminApi: {} }))
vi.mock('@/api/admin', () => ({ adminApi }))

import AffiliateRebates from '@/views/admin/AffiliateRebates.vue'

const ROWS = [
  {
    id: 2, inviter_id: 1, inviter_email: 'boss@qq.com',
    invitee_id: 9, invitee_email: 'zh***@qq.com',
    order_id: 30, order_no: 'CH202611030002', amount: 800, rate: 0.1,
    created_at: '2026-11-03 12:00:00',
  },
  {
    id: 1, inviter_id: 1, inviter_email: 'boss@qq.com',
    invitee_id: 8, invitee_email: 'li***@gmail.com',
    order_id: 21, order_no: 'CH202611030001', amount: 99, rate: 0.125,
    created_at: '2026-11-02 10:00:00',
  },
]

const mountPage = async (res) => {
  adminApi.listAffiliateRebates.mockResolvedValue(res)
  const wrapper = mount(AffiliateRebates)
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  adminApi.listAffiliateRebates = vi.fn()
  vi.clearAllMocks()
})

describe('AffiliateRebates.vue', () => {
  it('金额分转元、比例转百分比，按每页 20 条请求', async () => {
    const wrapper = await mountPage({ list: ROWS, total: 2, page: 1, per_page: 20 })

    expect(adminApi.listAffiliateRebates).toHaveBeenCalledWith({ page: 1, per_page: 20 })

    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('boss@qq.com')
    expect(rows[0].text()).toContain('zh***@qq.com')
    expect(rows[0].text()).toContain('CH202611030002')
    expect(rows[0].text()).toContain('¥8.00')
    expect(rows[0].text()).toContain('10%')
    expect(rows[0].text()).toContain('2026-11-03 12:00:00')
    // 0.1250 → 12.5%，不能带多余的 0
    expect(rows[1].text()).toContain('¥0.99')
    expect(rows[1].text()).toContain('12.5%')

    wrapper.unmount()
  })

  it('没有流水时给空态，不画空表头行', async () => {
    const wrapper = await mountPage({ list: [], total: 0, page: 1, per_page: 20 })

    expect(wrapper.findAll('tbody tr')).toHaveLength(1)
    expect(wrapper.text()).toContain('还没有返佣记录')

    wrapper.unmount()
  })

  it('失败时显示真实原因，重试重新请求', async () => {
    adminApi.listAffiliateRebates.mockRejectedValue(new Error('服务器错误'))
    const wrapper = mount(AffiliateRebates)
    await flushPromises()

    expect(wrapper.text()).toContain('加载失败：服务器错误')

    adminApi.listAffiliateRebates.mockResolvedValue({ list: ROWS, total: 2, page: 1, per_page: 20 })
    const retry = wrapper.findAll('button').find((b) => b.text().includes('重试'))
    await retry.trigger('click')
    await flushPromises()

    expect(adminApi.listAffiliateRebates).toHaveBeenCalledTimes(2)
    expect(wrapper.findAll('tbody tr')).toHaveLength(2)

    wrapper.unmount()
  })
})
