import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 余额兑换码页（管理端）。
 *
 * 后端 balance 域只接受**整数分**，面值输入是「元」——这里盯的就是这条换算边界，
 * 以及「明显非法就不发请求」这条产品口径（码只保留「删除」，无「作废」）。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))
vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import Giftcards from '@/views/admin/Giftcards.vue'

const activeRow = {
  id: 11, code: 'AAAA-BBBB-CCCC-DDDD', name: '双十一', value: 5000, status: 'active',
  used_by: null, used_at: null, expires_at: null, created_by: 1, note: null,
  created_at: '2026-11-03 10:00:00',
}
const usedRow = {
  id: 12, code: 'EEEE-FFFF-0000-1111', name: '', value: 1000, status: 'used',
  used_by: 7, used_at: '2026-11-03 11:00:00', expires_at: null, created_by: 1, note: '活动码',
  created_at: '2026-11-03 09:00:00',
}

const mountPage = async (rows = [activeRow, usedRow]) => {
  adminApi.listGiftcards.mockResolvedValue({ list: rows, total: rows.length, page: 1, per_page: 20 })
  const wrapper = mount(Giftcards, { attachTo: document.body })
  await flushPromises()
  return wrapper
}

const openCreate = async (wrapper) => {
  const btn = wrapper.findAll('button').find((b) => b.text().includes('生成兑换码'))
  await btn.trigger('click')
  await flushPromises()
}

/** 生成弹窗里的输入框按 label 文案定位（Input 组件根节点是 .ch-input-wrap）。 */
const fieldInput = (label) =>
  [...document.body.querySelectorAll('.ch-modal-card input')].find((i) =>
    i.closest('.gc-field')?.textContent.includes(label),
  )

const clickFooter = (text) => {
  const btn = [...document.body.querySelectorAll('.ch-modal-footer button')].find(
    (b) => b.textContent.trim() === text,
  )
  btn.click()
}

const setValue = async (input, value) => {
  input.value = value
  input.dispatchEvent(new Event('input'))
  await flushPromises()
}

beforeEach(() => {
  adminApi.listGiftcards = vi.fn(() => Promise.resolve({ list: [], total: 0 }))
  adminApi.createGiftcards = vi.fn(() => Promise.resolve({ count: 0, codes: [] }))
  vi.clearAllMocks()
  toast.success.mockClear()
  toast.error.mockClear()
  document.body.innerHTML = ''
})

describe('Giftcards.vue — 列表', () => {
  it('面值按分转元展示，状态与使用人如实渲染', async () => {
    const wrapper = await mountPage()

    const texts = wrapper.findAll('tbody tr').map((tr) => tr.text())
    expect(texts[0]).toContain('AAAA-BBBB-CCCC-DDDD')
    expect(texts[0]).toContain('¥50.00')
    expect(texts[0]).toContain('未使用')
    // 未使用的码没有使用人
    expect(texts[0]).toContain('—')
    // 已使用的码显示使用者 id + 使用时间
    expect(texts[1]).toContain('¥10.00')
    expect(texts[1]).toContain('已使用')
    expect(texts[1]).toContain('#7')

    wrapper.unmount()
  })

})

describe('Giftcards.vue — 生成', () => {
  it('面值「10 元」换算成 1000 分提交', async () => {
    const wrapper = await mountPage()
    adminApi.createGiftcards.mockResolvedValue({ count: 2, codes: ['AAAA-0000-0000-0001', 'AAAA-0000-0000-0002'] })
    await openCreate(wrapper)

    await setValue(fieldInput('数量'), '2')
    await setValue(fieldInput('面值'), '10')
    await setValue(fieldInput('名称'), '活动')

    clickFooter('生成')
    await flushPromises()

    expect(adminApi.createGiftcards).toHaveBeenCalledWith({ count: 2, value: 1000, name: '活动' })
    // 生成成功后把本批码摊出来，方便一次复制
    expect(document.body.textContent).toContain('AAAA-0000-0000-0001')
    expect(toast.error).not.toHaveBeenCalled()

    wrapper.unmount()
  })

  it('面值小数超过两位就地拦下，不发请求', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)

    await setValue(fieldInput('面值'), '10.555')
    clickFooter('生成')
    await flushPromises()

    expect(adminApi.createGiftcards).not.toHaveBeenCalled()
    expect(toast.error).toHaveBeenCalledWith(expect.stringContaining('面值'))

    wrapper.unmount()
  })

  it('数量超出 1-100 就地拦下，不发请求', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)

    await setValue(fieldInput('数量'), '101')
    await setValue(fieldInput('面值'), '10')
    clickFooter('生成')
    await flushPromises()

    expect(adminApi.createGiftcards).not.toHaveBeenCalled()
    expect(toast.error).toHaveBeenCalledWith(expect.stringContaining('1-100'))

    wrapper.unmount()
  })
})
