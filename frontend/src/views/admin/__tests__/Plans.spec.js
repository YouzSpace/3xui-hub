import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 套餐页 × 「可用地区」。
 *
 * 守两件事：
 *  1. 列表列口径：不勾地区（region_ids = null）显示「全部」；勾了就显示地区名，超过 3 个补「+N」。
 *  2. 保存口径：勾了传 id 数组，一个都不勾传 null —— 传 [] 会被后端当成「一个地区都不许用」，
 *     和「不限地区」完全相反（变异测试就是把它改成 [] 看这条用例变红）。
 *
 * 另外钉住「后端还没上 region_ids / 地区接口 404」的两种情况：页面不能崩，也不能谎报「全部」。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))
// 列表分页走 usePagedList（页码来自路由 query），这里不需要真路由
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}))

import Plans from '@/views/admin/Plans.vue'

const GB = 1024 * 1024 * 1024

const REGIONS = [
  { id: 1, name: '香港' },
  { id: 2, name: '日本' },
  { id: 3, name: '美国' },
  { id: 4, name: '新加坡' },
  { id: 5, name: '韩国' },
]

const plan = (over) => ({
  price: 19.9,
  reset_price: 5,
  type: 'period',
  months: 1,
  monthly_traffic: 100 * GB,
  period_traffic: 100 * GB,
  total_traffic: null,
  is_active: true,
  include_local: true,
  third_party_sub_ids: [],
  ...over,
})

const PLANS = [
  // 后端没返回 region_ids 的老套餐 → 「全部」
  plan({ id: 7, name: '月度100G', region_ids: null }),
  plan({ id: 8, name: '双地区套餐', region_ids: [1, 2] }),
  plan({ id: 9, name: '五地区套餐', region_ids: [1, 2, 3, 4, 5] }),
]

const paged = (items) =>
  Object.assign([...items], {
    pagination: { current_page: 1, last_page: 1, total: items.length, per_page: 50 },
  })

beforeEach(() => {
  vi.clearAllMocks()
  adminApi.listPlans = vi.fn(() => Promise.resolve(paged(PLANS)))
  adminApi.listThirdParty = vi.fn(() => Promise.resolve({ items: [] }))
  // 后端口径：{ regions: [...] }
  adminApi.listRegions = vi.fn(() => Promise.resolve({ regions: REGIONS }))
  adminApi.createPlan = vi.fn(() => Promise.resolve({ id: 100 }))
  adminApi.updatePlan = vi.fn(() => Promise.resolve({}))
  vi.stubGlobal('confirm', vi.fn(() => true))
})

const mounted = []
afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
  document.body.innerHTML = ''
  vi.unstubAllGlobals()
})

const mountPage = async () => {
  const wrapper = mount(Plans, {
    attachTo: document.body,
    global: { stubs: { RouterLink: { template: '<a><slot /></a>' } } },
  })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

/** 表格里按套餐名找那一行 */
const rowOf = (wrapper, name) => wrapper.findAll('tbody tr').find((tr) => tr.text().includes(name))
/** 「可用地区」是第 4 列（名称 / 价格 / 类型 / 可用地区） */
const regionCell = (wrapper, name) => rowOf(wrapper, name).findAll('td')[3]

/** Modal 走 Teleport 到 body，弹窗里的元素只能从 document 上找 */
const modalCheckboxes = () => [...document.querySelectorAll('.pl-region-list input[type="checkbox"]')]

/**
 * 勾/取消一个地区。每次点击后必须等一次 patch：v-model 的 checkbox 在
 * 点击时读的是上一次渲染缓存的数组，同一 tick 连点两次会互相覆盖（真实用户
 * 连点之间隔着宏任务，不会有这个问题）。
 */
const toggleBox = async (idx) => {
  modalCheckboxes()[idx].click()
  await flushPromises()
}

const clickFooterButton = async (label) => {
  const btn = [...document.querySelectorAll('.ch-modal-footer button')].find((b) =>
    b.textContent.trim().includes(label),
  )
  expect(btn, `弹窗底部没找到「${label}」按钮`).toBeTruthy()
  btn.click()
  await flushPromises()
}

describe('Plans.vue — 可用地区列', () => {
  it('不勾地区显示「全部」，勾了就显示地区名', async () => {
    const wrapper = await mountPage()

    expect(regionCell(wrapper, '月度100G').text()).toBe('全部')
    expect(regionCell(wrapper, '双地区套餐').text()).toBe('香港、日本')
  })

  it('超过 3 个地区只列前 3 个再补「+N」', async () => {
    const wrapper = await mountPage()

    expect(regionCell(wrapper, '五地区套餐').text()).toBe('香港、日本、美国 +2')
  })

  it('地区接口挂了不影响套餐列表：不弹错，有 region_ids 的只报个数', async () => {
    adminApi.listRegions = vi.fn(() => Promise.reject({ message: '404 page not found' }))
    const wrapper = await mountPage()

    expect(rowOf(wrapper, '双地区套餐')).toBeTruthy()
    expect(regionCell(wrapper, '双地区套餐').text()).toBe('已选 2 个地区')
    // 没有 region_ids → 仍然「全部」，不受地区接口影响
    expect(regionCell(wrapper, '月度100G').text()).toBe('全部')
    expect(toast.error).not.toHaveBeenCalled()
  })
})

describe('Plans.vue — 保存时的 region_ids', () => {
  it('编辑套餐勾 2 个地区 → 提交 region_ids:[1,2]', async () => {
    const wrapper = await mountPage()

    // 点第一行的编辑按钮（pl-actions 里第一个）
    await rowOf(wrapper, '月度100G').findAll('button')[0].trigger('click')
    await flushPromises()

    // 弹窗里列出全部地区，按勾选顺序就是 region_ids 的顺序
    expect(modalCheckboxes()).toHaveLength(REGIONS.length)

    await toggleBox(0) // 香港
    await toggleBox(1) // 日本

    await clickFooterButton('保存')

    expect(adminApi.updatePlan).toHaveBeenCalledTimes(1)
    const [id, payload] = adminApi.updatePlan.mock.calls[0]
    expect(id).toBe(7)
    expect(payload.region_ids).toEqual([1, 2])
  })

  it('新建套餐一个地区都不勾 → 提交 region_ids:null（不是 []）', async () => {
    const wrapper = await mountPage()

    const createBtn = wrapper.findAll('.pl-head button').find((b) => b.text().includes('新建套餐'))
    await createBtn.trigger('click')
    await flushPromises()

    expect(modalCheckboxes()).toHaveLength(REGIONS.length)
    expect(document.body.textContent).toContain('不勾选 = 所有地区都能用（默认）')

    await clickFooterButton('保存')

    expect(adminApi.createPlan).toHaveBeenCalledTimes(1)
    const payload = adminApi.createPlan.mock.calls[0][0]
    expect(payload.region_ids).toBeNull()
  })

  it('把已勾的地区全取消 → 也回落到 null', async () => {
    const wrapper = await mountPage()

    await rowOf(wrapper, '双地区套餐').findAll('button')[0].trigger('click')
    await flushPromises()

    // 原来勾着香港 + 日本，再点一次取消
    await toggleBox(0)
    await toggleBox(1)

    await clickFooterButton('保存')

    const [, payload] = adminApi.updatePlan.mock.calls[0]
    expect(payload.region_ids).toBeNull()
  })
})
