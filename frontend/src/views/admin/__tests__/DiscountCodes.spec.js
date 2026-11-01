import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 优惠码页：文件夹分组 / 勾选 / 导出 CSV / 码前缀。
 *
 * 后端的分组口径已经由 Feature 测试（DiscountCodeFolderTest）守住，这里盯前端三件
 * 只有渲染层才知道的事：
 *  ① 文件夹行按 folders[].matched 标「搜索命中」，不标在 unmatched 的文件夹上
 *  ② 勾选跨文件夹与平铺混勾，导出弹窗里的文件名可自填（不是随机串）
 *  ③ CSV 列序与转义：码/折扣/有效期/已用上限/说明，含逗号引号的按 RFC4180 转义
 *  ④ 生成时前缀非法就地拦下，不往后端发请求
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn() },
}))
vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import DiscountCodes from '@/views/admin/DiscountCodes.vue'

const loose = {
  id: 101, code: 'PROLOOSE01', source: 'admin', user_id: null, user_email: null,
  discount: 0.9, max_uses: 3, max_uses_per_user: 1, used_count: 1,
  expires_at: null, note: '未归档的码', status: 'active', plan_id: null, plan_name: null,
  folder_id: null, created_at: '2026-10-20 10:00:00',
}

const filed = {
  id: 201, code: 'FOLDER0001', source: 'admin', user_id: null, user_email: null,
  discount: 0.88, max_uses: 5, max_uses_per_user: 2, used_count: 0,
  expires_at: '2027-01-31 23:59:59', note: '文件夹里的码, 带逗号"引号"', status: 'active',
  plan_id: null, plan_name: null, folder_id: 7, created_at: '2026-10-20 09:00:00',
}

const FOLDERS = [
  { id: 7, name: '双十一', code_count: 1, matched: true },
  { id: 8, name: '空文件夹', code_count: 0, matched: false },
]

/** 列表接口按 folder 参数分流：主列表只给未归档，folder=7 给文件夹内的码。 */
const listMock = (params = {}) => {
  if (params.folder === 7) {
    return Promise.resolve({ list: [filed], total: 1, folders: FOLDERS })
  }
  if (params.folder === 8) {
    return Promise.resolve({ list: [], total: 0, folders: FOLDERS })
  }
  return Promise.resolve({ list: [loose], total: 1, folders: FOLDERS })
}

/** 捕获 Blob 内容：jsdom 没有真的下载，靠包装 createObjectURL 拿到 CSV 文本。 */
const captured = []
// 点下载用的 <a> 被组件摘掉了，趁 click 时把 download 属性捞出来
const downloadNames = []
let originalCreateObjectURL
let originalRevokeObjectURL
let clickSpy

beforeEach(() => {
  adminApi.listDiscountCodes = vi.fn(listMock)
  adminApi.listPlans = vi.fn(() => Promise.resolve([]))
  adminApi.getInviteSettings = vi.fn(() => Promise.resolve({}))
  adminApi.createDiscountCodes = vi.fn(() => Promise.resolve({ count: 1, codes: ['X'] }))
  vi.clearAllMocks()
  toast.success.mockClear()
  toast.error.mockClear()

  captured.length = 0
  downloadNames.length = 0
  originalCreateObjectURL = URL.createObjectURL
  originalRevokeObjectURL = URL.revokeObjectURL
  URL.createObjectURL = vi.fn((blob) => {
    captured.push(blob)
    return 'blob:mock'
  })
  URL.revokeObjectURL = vi.fn()
  // jsdom 对 a.click() 会尝试真导航（打印 "Not implemented: navigation"）且不触发下载，
  // 这里只记录 download 属性，不真的点
  clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function () {
    downloadNames.push(this.getAttribute('download'))
  })
})

afterEach(() => {
  URL.createObjectURL = originalCreateObjectURL
  URL.revokeObjectURL = originalRevokeObjectURL
  clickSpy.mockRestore()
})

const mountPage = async () => {
  const wrapper = mount(DiscountCodes, { attachTo: document.body })
  await flushPromises()
  return wrapper
}

/** 读回被包成 Blob 的 CSV 文本（BOM 去掉再看内容）。 */
const readCsv = async (blob) => {
  const text = await new Promise((resolve) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.readAsText(blob)
  })
  return text.replace(/^﻿/, '')
}

describe('DiscountCodes.vue — 文件夹行', () => {
  it('标出搜索命中的文件夹，未命中的不标', async () => {
    const wrapper = await mountPage()

    const rows = wrapper.findAll('.dc-folder')
    expect(rows).toHaveLength(2)

    expect(rows[0].text()).toContain('双十一')
    expect(rows[0].find('.dc-folder-matched').exists()).toBe(true)
    expect(rows[0].text()).toContain('搜索命中')
    expect(rows[0].text()).toContain('1')

    expect(rows[1].text()).toContain('空文件夹')
    expect(rows[1].find('.dc-folder-matched').exists()).toBe(false)

    wrapper.unmount()
  })

  it('点文件夹展开后按 folder 参数拉码，再点收起', async () => {
    const wrapper = await mountPage()

    await wrapper.findAll('.dc-folder-toggle')[0].trigger('click')
    await flushPromises()

    expect(adminApi.listDiscountCodes).toHaveBeenCalledWith(
      expect.objectContaining({ folder: 7 }),
    )
    expect(wrapper.find('.dc-table--folder').exists()).toBe(true)
    expect(wrapper.find('.dc-table--folder').text()).toContain('FOLDER0001')

    await wrapper.findAll('.dc-folder-toggle')[0].trigger('click')
    await flushPromises()
    expect(wrapper.find('.dc-table--folder').exists()).toBe(false)

    wrapper.unmount()
  })
})

describe('DiscountCodes.vue — 勾选与导出', () => {
  it('文件夹全选 + 平铺勾选混在一起，导出的是两边并集', async () => {
    const wrapper = await mountPage()

    // 平铺表里勾第一条（PROLOOSE01）
    await wrapper.find('.dc-table tbody input[type="checkbox"]').setValue(true)
    await flushPromises()

    // 文件夹勾选框（issue #59 起是勾选框，不再是「全选」文字按钮）：
    // 先把里面的码拉回来再整批勾上
    await wrapper.findAll('.dc-folder-actions')[0].find('input[type="checkbox"]').setValue(true)
    await flushPromises()

    expect(wrapper.text()).toContain('已勾选 2 个')

    // 打开导出弹窗，默认文件名带日期且可改
    await wrapper.find('.dc-export-btn').trigger('click')
    await flushPromises()
    const nameInput = document.body.querySelector('.ch-modal-card input')
    expect(nameInput.value).toMatch(/^discount-codes-\d{4}-\d{2}-\d{2}\.csv$/)

    nameInput.value = '我的优惠码'
    nameInput.dispatchEvent(new Event('input'))
    await flushPromises()

    const exportBtn = [...document.body.querySelectorAll('.ch-modal-footer button')]
      .find((b) => b.textContent.includes('导出'))
    exportBtn.click()
    await flushPromises()

    expect(captured).toHaveLength(1)
    const csv = await readCsv(captured[0])
    const lines = csv.split('\n')

    // 列序：码/折扣/有效期/已用上限/说明
    expect(lines[0]).toBe('码,折扣,有效期,已用上限,说明')
    // 按码排序，两条都在
    expect(lines[1]).toBe('FOLDER0001,0.88,2027-01-31 23:59:59,5,"文件夹里的码, 带逗号""引号"""')
    expect(lines[2]).toBe('PROLOOSE01,0.90,永久,3,未归档的码')

    // 文件名按用户填的走（补 .csv），不是随机串
    expect(downloadNames).toEqual(['我的优惠码.csv'])

    wrapper.unmount()
  })

  it('没勾选时不显示批量操作条，勾上之后导出/删除入口一起出现', async () => {
    const wrapper = await mountPage()

    // issue #59：批量操作条只在勾选 > 0 时存在（不再是在「未归档」旁常驻一个禁用按钮）
    expect(wrapper.find('.dc-bulkbar').exists()).toBe(false)
    expect(wrapper.find('.dc-export-btn').exists()).toBe(false)

    await wrapper.find('.dc-table tbody input[type="checkbox"]').setValue(true)
    await flushPromises()

    expect(wrapper.find('.dc-bulkbar').exists()).toBe(true)
    expect(wrapper.find('.dc-export-btn').attributes('disabled')).toBeUndefined()

    wrapper.unmount()
  })
})

describe('DiscountCodes.vue — 删除文件夹（issue #59，前后端联动）', () => {
  const openDeleteModal = async (wrapper) => {
    // 文件夹行右侧的垃圾桶图标（不是表格行里的删除按钮）
    await wrapper.find('.dc-folder-actions .dc-icon-btn--danger').trigger('click')
    await flushPromises()
  }

  it('默认只删文件夹：with_codes=false，提示码退回未归档', async () => {
    const wrapper = await mountPage()
    adminApi.deleteDiscountCodeFolder = vi.fn(() => Promise.resolve(null))

    await openDeleteModal(wrapper)

    const btn = [...document.body.querySelectorAll('.ch-modal-footer button')]
      .find((b) => b.textContent.trim() === '删除文件夹')
    expect(btn, '弹窗里应有「删除文件夹」按钮').toBeTruthy()
    btn.click()
    await flushPromises()

    // 与后端契约：第二参 false → 不带 ?with_codes（后端走默认分支，码退回未归档）
    expect(adminApi.deleteDiscountCodeFolder).toHaveBeenCalledWith(7, false)
    expect(toast.success).toHaveBeenCalledWith('已删除文件夹，其中优惠码已转为未归档')

    wrapper.unmount()
  })

  it('选「连码一起删」：with_codes=true，提示里用的是后端返回的实际删除数', async () => {
    const wrapper = await mountPage()
    adminApi.deleteDiscountCodeFolder = vi.fn(() => Promise.resolve({ deleted_codes: 1, failed: [] }))

    await openDeleteModal(wrapper)

    const radios = [...document.body.querySelectorAll('.ch-modal-card input[type="radio"]')]
    expect(radios, '码的去向是两个单选').toHaveLength(2)

    // 第二个 = 连码一起删
    radios[1].checked = true
    radios[1].dispatchEvent(new Event('change'))
    await flushPromises()

    const btn = [...document.body.querySelectorAll('.ch-modal-footer button')]
      .find((b) => b.textContent.includes('删除文件夹和码'))
    expect(btn, '选连码删时按钮文案要变').toBeTruthy()
    btn.click()
    await flushPromises()

    expect(adminApi.deleteDiscountCodeFolder).toHaveBeenCalledWith(7, true)
    // 文案取自 data.deleted_codes（不是本地猜的数）
    expect(toast.success).toHaveBeenCalledWith('已删除文件夹及其中 1 个优惠码')

    wrapper.unmount()
  })
})

describe('DiscountCodes.vue — 生成前缀', () => {
  const openCreate = async (wrapper) => {
    await wrapper.findAll('.dc-tools button').filter((b) => b.text().includes('生成优惠码'))[0].trigger('click')
    await flushPromises()
  }

  it('非法前缀就地拦下，不发请求', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)

    const inputs = [...document.body.querySelectorAll('.ch-modal-card input')]
    const prefix = inputs.find((i) => i.closest('.dc-field')?.textContent.includes('码前缀'))
    expect(prefix, '生成弹窗里应有前缀输入框').toBeTruthy()

    prefix.value = 'VIP-2026'
    prefix.dispatchEvent(new Event('input'))
    await flushPromises()

    const submit = [...document.body.querySelectorAll('.ch-modal-footer button')]
      .find((b) => b.textContent.trim() === '生成')
    submit.click()
    await flushPromises()

    expect(adminApi.createDiscountCodes).not.toHaveBeenCalled()
    expect(toast.error).toHaveBeenCalledWith(expect.stringContaining('前缀'))

    wrapper.unmount()
  })

  it('合法前缀转大写后提交，并带上新建文件夹名', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)

    const inputs = [...document.body.querySelectorAll('.ch-modal-card input')]
    const prefix = inputs.find((i) => i.closest('.dc-field')?.textContent.includes('码前缀'))
    prefix.value = 'vip2026'
    prefix.dispatchEvent(new Event('input'))

    const folderNameInput = inputs.find((i) => i.placeholder === '如：双十一活动')
    folderNameInput.value = '春节活动'
    folderNameInput.dispatchEvent(new Event('input'))
    await flushPromises()

    const submit = [...document.body.querySelectorAll('.ch-modal-footer button')]
      .find((b) => b.textContent.trim() === '生成')
    submit.click()
    await flushPromises()

    expect(adminApi.createDiscountCodes).toHaveBeenCalledTimes(1)
    const payload = adminApi.createDiscountCodes.mock.calls[0][0]
    expect(payload.prefix).toBe('VIP2026')
    expect(payload.folder_name).toBe('春节活动')
    expect(payload.folder_id).toBeNull()

    wrapper.unmount()
  })

  it('前缀留空回落到默认 PRO，不报错', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)

    const inputs = [...document.body.querySelectorAll('.ch-modal-card input')]
    const prefix = inputs.find((i) => i.closest('.dc-field')?.textContent.includes('码前缀'))
    prefix.value = ''
    prefix.dispatchEvent(new Event('input'))
    await flushPromises()

    const submit = [...document.body.querySelectorAll('.ch-modal-footer button')]
      .find((b) => b.textContent.trim() === '生成')
    submit.click()
    await flushPromises()

    expect(adminApi.createDiscountCodes).toHaveBeenCalledTimes(1)
    expect(adminApi.createDiscountCodes.mock.calls[0][0].prefix).toBe('PRO')
    expect(toast.error).not.toHaveBeenCalled()

    wrapper.unmount()
  })
})
