import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 代理商品页 × socks 独占。
 *
 * 守的是这次新加的三处口径：
 *  1. socks 货源列表的「分配方式 / 已售给」两列：独占且卖掉了才出用户 id，其余一律「—」；
 *  2. 规格库存列：独占显示「可用 X 条」（stock_available，老接口没有就自己算），共享仍显示「可售 / 总数」；
 *  3. 规格切到独占后「货源」下拉首项变成「不指定（自动从独占池挑）」，保存时 source_id 传 null
 *     （绑定具体某条就退化成共享了，等于把独占池废掉）。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import ProxyStore from '@/views/admin/ProxyStore.vue'

const PRODUCTS = [
  {
    id: 1,
    name: '美国住宅代理',
    subtitle: '',
    is_active: true,
    sort_order: 0,
    skus: [
      // 共享 socks：库存仍按「可售 / 总数」显示
      {
        id: 11, product_id: 1, name: '共享 10G', source_type: 'socks', allocation: 'shared',
        source_id: 3, region: 'US', traffic_gb: 10, duration_days: 30, max_thread_count: 0,
        price: 5, cost_price: 1, stock_total: 10, stock_locked: 2, stock_sold: 1,
        stock_available: 7, is_active: true, sort_order: 0,
      },
      // 独占 socks：货源不指定（空），库存按池子里的可用条数
      {
        id: 12, product_id: 1, name: '独占 30G', source_type: 'socks', allocation: 'exclusive',
        source_id: null, region: 'US', traffic_gb: 30, duration_days: 30, max_thread_count: 0,
        price: 20, cost_price: 5, stock_total: 5, stock_locked: 0, stock_sold: 2,
        stock_available: 3, is_active: true, sort_order: 0,
      },
      // 老接口：没有 allocation、没有 stock_available → 按共享 + 自己算
      {
        id: 13, product_id: 1, name: '老规格', source_type: 'socks',
        source_id: 4, region: null, traffic_gb: 0, duration_days: 30, max_thread_count: 0,
        price: 3, cost_price: 0, stock_total: 8, stock_locked: 1, stock_sold: 2,
        is_active: true, sort_order: 0,
      },
    ],
  },
]

const SOCKS = [
  // 独占且已卖出 → 「已售给」出用户 id
  {
    id: 3, name: 'hk-1', host: '1.2.3.4', port: 1080, username: 'u1', has_password: true,
    country: 'HK', allocation: 'exclusive', sold_to: 42, sold_at: '2026-11-07T10:00:00Z', is_active: true,
  },
  // 共享 → 「—」
  {
    id: 4, name: 'us-1', host: '5.6.7.8', port: 1080, username: null, has_password: false,
    country: 'US', allocation: 'shared', is_active: true,
  },
  // 后端还没返回 allocation 的老数据 → 按共享兜底，不能瞎猜「独占」
  { id: 5, name: 'old-1', host: '9.9.9.9', port: 1080, has_password: false, is_active: true },
]

beforeEach(() => {
  vi.clearAllMocks()
  adminApi.proxyStoreSettings = vi.fn(() => Promise.resolve({ enabled: true }))
  adminApi.listProxyProducts = vi.fn(() => Promise.resolve({ items: PRODUCTS }))
  adminApi.listProxySocks = vi.fn(() => Promise.resolve({ items: SOCKS }))
  adminApi.listWebshareProviders = vi.fn(() => Promise.resolve({ items: [] }))
  adminApi.updateProxySku = vi.fn(() => Promise.resolve({}))
  adminApi.updateProxySock = vi.fn(() => Promise.resolve({}))
})

const mounted = []
afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
  document.body.innerHTML = ''
})

const mountPage = async () => {
  const wrapper = mount(ProxyStore, { attachTo: document.body })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

/** 表格里按文本找一行（规格表、socks 表都在组件内） */
const rowOf = (wrapper, text) => wrapper.findAll('tbody tr').find((tr) => tr.text().includes(text))

/** 打开弹窗里第 n 个自定义下拉，返回当前面板上的选项文案 */
const openSelectOptions = async (n) => {
  document.querySelectorAll('.ch-modal-body .ch-select-trigger')[n].click()
  await flushPromises()
  return [...document.querySelectorAll('.ch-select-option')].map((o) => o.textContent.trim())
}

/** 点下拉面板里文案匹配的那一项 */
const pickOption = async (label) => {
  const opt = [...document.querySelectorAll('.ch-select-option')].find((o) => o.textContent.includes(label))
  expect(opt, `下拉里没有「${label}」`).toBeTruthy()
  opt.click()
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

describe('ProxyStore.vue — socks 货源列表', () => {
  it('「分配方式」「已售给」两列：独占出用户 id，共享/老数据一律「—」', async () => {
    const wrapper = await mountPage()

    const exclusive = rowOf(wrapper, '1.2.3.4')
    expect(exclusive.text()).toContain('独占')
    expect(exclusive.text()).toContain('用户 #42')

    const shared = rowOf(wrapper, '5.6.7.8')
    expect(shared.text()).toContain('共享')
    expect(shared.text()).toContain('—')

    // 没有 allocation 字段的老线路按共享算，「已售给」不能凭空出用户
    const legacy = rowOf(wrapper, '9.9.9.9')
    expect(legacy.text()).toContain('共享')
    expect(legacy.text()).toContain('—')
    expect(legacy.text()).not.toContain('用户 #')
  })
})

describe('ProxyStore.vue — 规格库存列', () => {
  it('独占显示「可用 X 条」，共享显示「可售 / 总数」，缺 stock_available 时自己算', async () => {
    const wrapper = await mountPage()

    const exclusive = rowOf(wrapper, '独占 30G')
    expect(exclusive.text()).toContain('可用 3 条')

    const shared = rowOf(wrapper, '共享 10G')
    expect(shared.text()).toContain('7 / 10')
    expect(shared.text()).toContain('占用 2')

    // 老接口没有 stock_available：8 - 1 - 2 = 5
    expect(rowOf(wrapper, '老规格').text()).toContain('5 / 8')
  })
})

describe('ProxyStore.vue — 规格弹窗的分配方式', () => {
  it('切到独占后，货源下拉首项变成「不指定（自动从独占池挑）」，保存传 source_id:null', async () => {
    const wrapper = await mountPage()

    // 打开「共享 10G」这条 socks 规格
    await rowOf(wrapper, '共享 10G').findAll('button')[0].trigger('click')
    await flushPromises()
    expect(wrapper.vm.skuForm.allocation).toBe('shared')

    // 货源下拉（第 2 个）此时首项是「请选择一条 socks」
    let options = await openSelectOptions(1)
    expect(options[0]).toContain('请选择一条 socks')
    document.querySelectorAll('.ch-modal-body .ch-select-trigger')[1].click() // 收起
    await flushPromises()

    // 分配方式（第 3 个下拉）切独占
    await openSelectOptions(2)
    await pickOption('独占（一人一条）')
    expect(wrapper.vm.skuForm.allocation).toBe('exclusive')

    // 再开货源下拉：首项已经换成「不指定」
    options = await openSelectOptions(1)
    expect(options[0]).toContain('不指定（自动从独占池挑）')
    await pickOption('不指定（自动从独占池挑）')

    await clickFooterButton('保存')

    expect(adminApi.updateProxySku).toHaveBeenCalledTimes(1)
    const [id, payload] = adminApi.updateProxySku.mock.calls[0]
    expect(id).toBe(11)
    expect(payload.allocation).toBe('exclusive')
    expect(payload.source_id).toBeNull()
  })

  it('共享模式下不选具体 socks 仍拦下来（不许存成没有货源的规格）', async () => {
    const wrapper = await mountPage()

    await rowOf(wrapper, '共享 10G').findAll('button')[0].trigger('click')
    await flushPromises()

    wrapper.vm.skuForm.source_id = ''
    await flushPromises()
    await clickFooterButton('保存')

    expect(adminApi.updateProxySku).not.toHaveBeenCalled()
    expect(toast.error).toHaveBeenCalledWith('socks 货源必须选择一条已录入的 socks')
  })
})

describe('ProxyStore.vue — socks 编辑弹窗的分配方式', () => {
  it('切到独占后保存，payload 带 allocation:exclusive', async () => {
    const wrapper = await mountPage()

    const row = rowOf(wrapper, '5.6.7.8')
    const editBtn = row.findAll('button').find((b) => b.text().includes('编辑'))
    await editBtn.trigger('click')
    await flushPromises()

    expect(wrapper.vm.sockForm.allocation).toBe('shared')

    const radio = document.querySelector('.ch-modal-body input[type="radio"][value="exclusive"]')
    expect(radio).toBeTruthy()
    radio.click()
    await flushPromises()
    expect(wrapper.vm.sockForm.allocation).toBe('exclusive')

    await clickFooterButton('保存')

    expect(adminApi.updateProxySock).toHaveBeenCalledTimes(1)
    const [id, payload] = adminApi.updateProxySock.mock.calls[0]
    expect(id).toBe(4)
    expect(payload.allocation).toBe('exclusive')
  })
})
