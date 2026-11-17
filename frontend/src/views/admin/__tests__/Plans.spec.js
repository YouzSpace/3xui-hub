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

// 静态 IP 出口分类（「出口类型 = 静态 IP」时的下拉）
const EXIT_PROFILES = [
  { id: 9, name: '香港住宅', enabled: true, provider_name: '主力账号' },
  { id: 10, name: '已停用分类', enabled: false },
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
  adminApi.listExitProfiles = vi.fn(() => Promise.resolve(EXIT_PROFILES))
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

/** 出口类型单选（.pl-exit 里一排 radio）：按 value 点 */
const pickExitType = async (value) => {
  const radio = [...document.querySelectorAll('.pl-exit input[type="radio"]')].find(
    (r) => r.value === value,
  )
  expect(radio, `没找到出口类型「${value}」`).toBeTruthy()
  radio.click()
  await flushPromises()
}

/** 出口分类下拉：自定义 Select，面板 Teleport 到 body，点完触发器再点选项 */
const pickExitProfile = async (text) => {
  const trigger = document.querySelector('.pl-exit-profile .ch-select-trigger')
  expect(trigger, '出口分类下拉没渲染').toBeTruthy()
  trigger.click()
  await flushPromises()
  const opt = [...document.querySelectorAll('.ch-select-option')].find((o) =>
    o.textContent.includes(text),
  )
  expect(opt, `下拉里没找到「${text}」`).toBeTruthy()
  opt.click()
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

/**
 * 出口类型 × 出口分类。
 *
 * 守两件事：
 *  1. 出口分类下拉只在「静态 IP」时出现 —— 直连套餐的弹窗里不该有这个字段（变异测试就是
 *     把这个 v-if 去掉，看下面第一条用例变红）；
 *  2. 保存口径：只有静态 IP 才把 exit_profile_id 传上去，直连 / 单独 socks 一律传 null，
 *     不给后端「直连套餐却绑着住宅出口」的机会。
 */
describe('Plans.vue — 出口类型 / 出口分类', () => {
  it('直连（默认）不显示出口分类下拉；选「静态 IP」才出现', async () => {
    const wrapper = await mountPage()

    await rowOf(wrapper, '月度100G').findAll('button')[0].trigger('click')
    await flushPromises()

    // 默认直连：没有分类下拉
    expect(document.querySelector('.pl-exit-profile')).toBeNull()
    // 三个选项都在，且默认选中直连
    const radios = [...document.querySelectorAll('.pl-exit input[type="radio"]')]
    expect(radios).toHaveLength(3)
    expect(radios.find((r) => r.checked)?.value).toBe('direct')

    await pickExitType('static_chain')

    expect(document.querySelector('.pl-exit-profile')).not.toBeNull()
    expect(document.body.textContent).toContain('出口分类')
    // 分类列表来自 /exit-profiles，停用的分类不给选
    expect(adminApi.listExitProfiles).toHaveBeenCalled()
  })

  it('选静态 IP + 分类 → payload 含 exit_type=static_chain 和 exit_profile_id', async () => {
    const wrapper = await mountPage()

    await rowOf(wrapper, '月度100G').findAll('button')[0].trigger('click')
    await flushPromises()

    await pickExitType('static_chain')
    await pickExitProfile('香港住宅')
    await clickFooterButton('保存')

    expect(adminApi.updatePlan).toHaveBeenCalledTimes(1)
    const [id, payload] = adminApi.updatePlan.mock.calls[0]
    expect(id).toBe(7)
    expect(payload.exit_type).toBe('static_chain')
    expect(payload.exit_profile_id).toBe(9)
  })

  it('从静态 IP 改回直连 → exit_profile_id 传 null（不留旧绑定）', async () => {
    const wrapper = await mountPage()

    await rowOf(wrapper, '月度100G').findAll('button')[0].trigger('click')
    await flushPromises()

    await pickExitType('static_chain')
    await pickExitProfile('香港住宅')
    // 再改回直连：分类下拉消失，payload 里必须清空绑定
    await pickExitType('direct')
    expect(document.querySelector('.pl-exit-profile')).toBeNull()

    await clickFooterButton('保存')

    const [, payload] = adminApi.updatePlan.mock.calls[0]
    expect(payload.exit_type).toBe('direct')
    expect(payload.exit_profile_id).toBeNull()
  })

  it('新建套餐默认直连 → payload 是 direct + null', async () => {
    const wrapper = await mountPage()

    const createBtn = wrapper.findAll('.pl-head button').find((b) => b.text().includes('新建套餐'))
    await createBtn.trigger('click')
    await flushPromises()

    await clickFooterButton('保存')

    const payload = adminApi.createPlan.mock.calls[0][0]
    expect(payload.exit_type).toBe('direct')
    expect(payload.exit_profile_id).toBeNull()
  })

  it('选了静态 IP 却没选分类 → 拦下不提交（避免建出没有出口的套餐）', async () => {
    const wrapper = await mountPage()

    await rowOf(wrapper, '月度100G').findAll('button')[0].trigger('click')
    await flushPromises()

    await pickExitType('static_chain')
    await clickFooterButton('保存')

    expect(toast.error).toHaveBeenCalledWith('出口类型选了静态 IP，请选择出口分类')
    expect(adminApi.updatePlan).not.toHaveBeenCalled()
  })
})
