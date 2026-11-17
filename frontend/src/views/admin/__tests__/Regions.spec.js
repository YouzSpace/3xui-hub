import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 地区与调度页（Regions.vue）。
 *
 * 守的是三件容易只在前端炸掉的事：
 *  1. 总览数字确实渲染出来（/regions/overview 的 node_total / capacity_total / capacity_used）；
 *  2. 地区行字段口径（节点数、已绑 / 容量、capacity 为 null 显示「不限」）；
 *  3. 删有节点的地区时，后端拒绝的 msg 必须原样弹给管理员，不能被吞成「删除失败」。
 * 另外覆盖「展开地区 → 点迁移 → 弹窗打开」这条交互链路。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import Regions from '@/views/admin/Regions.vue'

const REGIONS = [
  { id: 1, name: '香港', sort: 0, node_count: 2, user_count: 900, capacity_total: 4000, capacity_used: 900 },
  // capacity_total = null → 页面显示「不限」，使用率显示「—」
  { id: 2, name: '日本', sort: 1, node_count: 1, user_count: 60, capacity_total: null, capacity_used: 60 },
]

const NODES = [
  { id: 11, name: 'hk-1', host: 'hk1.example.com', driver_type: '3xui', status: 'online', capacity: 2000, bound_users: 600, region_id: 1 },
  { id: 12, name: 'hk-2', host: 'hk2.example.com', driver_type: 'xray', status: 'offline', capacity: 2000, bound_users: 300, region_id: 1 },
  { id: 13, name: 'jp-1', host: 'jp1.example.com', driver_type: 'xray', status: 'online', capacity: null, bound_users: 60, region_id: 2 },
  { id: 14, name: 'free-1', host: 'f1.example.com', driver_type: '3xui', status: 'online', capacity: null, bound_users: 5, region_id: null },
]

const OVERVIEW = { node_total: 4, capacity_total: 4000, capacity_used: 960 }

// 建议容量：接口按 Go 字段名直出（Suggest / Confidence / Reason），前端两种命名都认
const RECS = {
  11: { NodeID: 11, Suggest: 342000, PerUserKB: 0, Basis: 'percent', Confidence: 'high', Samples: 42, Reason: '按占用百分比斜率推算，每用户约 0.0002%' },
  12: { NodeID: 12, Suggest: 8000, PerUserKB: 5, Basis: 'default', Confidence: 'low', Samples: 3, Reason: '样本不足，按默认 5KB/用户估算' },
}

beforeEach(() => {
  vi.clearAllMocks()
  adminApi.listRegions = vi.fn(() => Promise.resolve(REGIONS))
  adminApi.listNodes = vi.fn(() => Promise.resolve(NODES))
  adminApi.regionOverview = vi.fn(() => Promise.resolve(OVERVIEW))
  adminApi.listRegionNodes = vi.fn((id) => Promise.resolve(NODES.filter((n) => n.region_id === id)))
  adminApi.deleteRegion = vi.fn(() => Promise.resolve({}))
  adminApi.balanceRegion = vi.fn(() => Promise.resolve({ moved: 0 }))
  // 地区功能-22：默认无告警；推荐只给 hk-1（high）/ hk-2（low），其余拿不到
  adminApi.regionsAlerts = vi.fn(() => Promise.resolve({ alerts: [], counts: { warn: 0, critical: 0 } }))
  adminApi.capacityRecommend = vi.fn((id) => Promise.resolve(RECS[id] || null))
  // jsdom 没实现 confirm：默认返回 undefined（= 取消），这里显式确认
  vi.stubGlobal('confirm', vi.fn(() => true))
})

const mounted = []
afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
  vi.unstubAllGlobals()
})

const mountPage = async () => {
  const wrapper = mount(Regions, { attachTo: document.body })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

const regionRow = (wrapper, name) => wrapper.findAll('tbody tr.rg-row').find((tr) => tr.text().includes(name))

describe('Regions.vue — 总览与地区列表', () => {
  it('顶部总览渲染节点数 / 总容量 / 已绑 / 使用率', async () => {
    const wrapper = await mountPage()

    const values = wrapper.findAll('.rg-stat-value').map((n) => n.text())
    expect(values).toEqual(['4', '4000', '960', '24%'])
  })

  it('总览接口不可用时按地区汇总兜底，不抛错也不弹错误', async () => {
    adminApi.regionOverview = vi.fn(() => Promise.reject({ message: '404 page not found' }))
    const wrapper = await mountPage()

    // 地区列表合计：capacity_total = 4000，capacity_used = 960，节点数用已加载节点数
    const values = wrapper.findAll('.rg-stat-value').map((n) => n.text())
    expect(values).toEqual(['4', '4000', '960', '24%'])
    expect(toast.error).not.toHaveBeenCalled()
  })

  it('地区行：节点数、已绑 / 容量、capacity 为空显示「不限」', async () => {
    const wrapper = await mountPage()

    const hk = regionRow(wrapper, '香港')
    expect(hk.text()).toContain('香港')
    expect(hk.text()).toContain('2')
    expect(hk.text()).toContain('900 / 4000')
    expect(hk.text()).toContain('23%') // 900 / 4000 四舍五入

    const jp = regionRow(wrapper, '日本')
    expect(jp.text()).toContain('60 / 不限')
    // 容量不限时没有使用率进度条，显示占位符
    expect(jp.find('.ch-progress').exists()).toBe(false)
  })

  it('未归属节点单独一块显示，并带「设置归属 / 容量」入口', async () => {
    const wrapper = await mountPage()

    expect(wrapper.text()).toContain('未归属节点（全量下发）')
    const freeRow = wrapper.findAll('tbody tr').find((tr) => tr.text().includes('free-1'))
    expect(freeRow).toBeTruthy()
    expect(freeRow.text()).toContain('设置归属 / 容量')
  })
})

describe('Regions.vue — 迁移入口', () => {
  it('展开地区后点「迁移」打开迁移弹窗，并列出其他节点作为目标', async () => {
    const wrapper = await mountPage()

    // 点地区行展开该地区的节点表
    await regionRow(wrapper, '香港').trigger('click')
    await flushPromises()
    expect(adminApi.listRegionNodes).toHaveBeenCalledWith(1)

    const nodeRow = wrapper.findAll('tbody tr').find((tr) => tr.text().includes('hk-1'))
    const migrateBtn = nodeRow.findAll('button').find((b) => b.text() === '迁移')
    expect(migrateBtn).toBeTruthy()
    await migrateBtn.trigger('click')
    await flushPromises()

    expect(wrapper.vm.migOpen).toBe(true)
    // Modal 走 Teleport 到 body，标题不在 wrapper 里
    expect(document.body.textContent).toContain('迁移 · hk-1')
    // 目标节点排除自己，其余 3 台都能选
    expect(wrapper.vm.targetOptions.map((o) => o.value)).toEqual([12, 13, 14])
  })
})

describe('Regions.vue — 删除地区', () => {
  it('后端拒绝删除时把后端 msg 原样弹给管理员', async () => {
    adminApi.deleteRegion = vi.fn(() => Promise.reject({ message: '该地区还有 2 台节点，请先移出' }))
    const wrapper = await mountPage()

    const deleteBtn = regionRow(wrapper, '香港').find('button[title="删除地区"]')
    expect(deleteBtn.exists()).toBe(true)
    await deleteBtn.trigger('click')
    await flushPromises()

    expect(adminApi.deleteRegion).toHaveBeenCalledWith(1)
    expect(toast.error).toHaveBeenCalledWith('该地区还有 2 台节点，请先移出')
    // 失败后不刷新列表（地区仍在页面上）
    expect(regionRow(wrapper, '香港')).toBeTruthy()
  })

  it('删除成功后才重新拉列表', async () => {
    const wrapper = await mountPage()
    expect(adminApi.listRegions).toHaveBeenCalledTimes(1)

    const deleteBtn = regionRow(wrapper, '香港').find('button[title="删除地区"]')
    await deleteBtn.trigger('click')
    await flushPromises()

    expect(adminApi.deleteRegion).toHaveBeenCalledWith(1)
    expect(toast.success).toHaveBeenCalledWith('已删除')
    expect(adminApi.listRegions).toHaveBeenCalledTimes(2)
  })
})

/**
 * 地区功能-22：容量告警条 + 容量推荐列。
 *
 * 告警条的口径来自 GET /regions/alerts 的 counts：critical>0 出红条、warn>0 出黄条。
 * 这两条判断互相独立 —— critical=0 时哪怕 warn 很大也绝不能出红条（变异测试就打这里）。
 * 推荐列来自逐节点接口 GET /nodes/:id/capacity-recommend，接口没就绪时整列显示「—」。
 */
describe('Regions.vue — 容量告警条', () => {
  it('counts.critical > 0 渲染红色告警条，条上是 critical 的数量', async () => {
    adminApi.regionsAlerts = vi.fn(() => Promise.resolve({ alerts: [], counts: { warn: 0, critical: 2 } }))
    const wrapper = await mountPage()

    const red = wrapper.find('[data-test="alert-critical"]')
    expect(red.exists()).toBe(true)
    expect(red.classes()).toContain('rg-alert--critical')
    expect(red.text()).toContain('2 个节点/地区容量超过 95%')
    // critical 为唯一等级时不重复出黄条
    expect(wrapper.find('[data-test="alert-warn"]').exists()).toBe(false)
  })

  it('只有 warn 时只出黄条，绝不出红条（critical>0 与 warn>0 不能混用）', async () => {
    adminApi.regionsAlerts = vi.fn(() => Promise.resolve({ alerts: [], counts: { warn: 3, critical: 0 } }))
    const wrapper = await mountPage()

    expect(wrapper.find('[data-test="alert-critical"]').exists()).toBe(false)
    const yellow = wrapper.find('[data-test="alert-warn"]')
    expect(yellow.exists()).toBe(true)
    expect(yellow.classes()).toContain('rg-alert--warn')
    expect(yellow.text()).toContain('3 个节点/地区容量超过 85%')
  })

  it('告警接口没就绪（404）时两条都不渲染，也不弹错误', async () => {
    adminApi.regionsAlerts = vi.fn(() => Promise.reject({ message: '404 page not found' }))
    const wrapper = await mountPage()

    expect(wrapper.find('[data-test="alert-critical"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="alert-warn"]').exists()).toBe(false)
    expect(toast.error).not.toHaveBeenCalled()
    // 页面主体照常
    expect(regionRow(wrapper, '香港')).toBeTruthy()
  })
})

describe('Regions.vue — 建议容量 / 采用', () => {
  // 展开后的节点行：外层 tbody 里还有一条 colspan 容器行（它包含整张内表，text 也含节点名），
  // 所以按「第一格文字 === 节点名」在内表里精确找，避免匹配到容器行。
  const nodeRowOf = (wrapper, name) =>
    wrapper.findAll('table.rg-table--inner tbody tr').find((tr) => tr.findAll('td')[0]?.text() === name)

  it('节点表多出「建议容量」「采用」两列', async () => {
    const wrapper = await mountPage()
    const heads = wrapper.findAll('thead th').map((th) => th.text())
    expect(heads).toContain('建议容量')
    expect(heads).toContain('采用')
  })

  it('展开地区后按节点渲染建议容量 + 置信度图标，hover 带 Reason', async () => {
    const wrapper = await mountPage()
    await regionRow(wrapper, '香港').trigger('click')
    await flushPromises()

    expect(adminApi.capacityRecommend).toHaveBeenCalledWith(11)
    const rec = nodeRowOf(wrapper, 'hk-1').find('td.rg-rec')
    expect(rec.text()).toContain('342000')
    expect(rec.text()).toContain('✅') // high
    expect(rec.attributes('title')).toContain('占用百分比')

    const rec2 = nodeRowOf(wrapper, 'hk-2').find('td.rg-rec')
    expect(rec2.text()).toContain('8000')
    expect(rec2.text()).toContain('💡') // low
  })

  it('接口拿不到推荐的节点显示「—」，不出现「采用」按钮', async () => {
    adminApi.capacityRecommend = vi.fn(() => Promise.reject({ message: '404' }))
    const wrapper = await mountPage()
    await regionRow(wrapper, '香港').trigger('click')
    await flushPromises()

    const hk1 = nodeRowOf(wrapper, 'hk-1')
    expect(hk1.find('td.rg-rec').text()).toBe('—')
    expect(hk1.findAll('button').some((b) => b.text() === '采用')).toBe(false)
  })

  it('点「采用」把建议值写进 PUT /nodes/:id/capacity，并刷新列表', async () => {
    adminApi.setNodeCapacity = vi.fn(() => Promise.resolve({}))
    const wrapper = await mountPage()
    await regionRow(wrapper, '香港').trigger('click')
    await flushPromises()
    expect(adminApi.listRegions).toHaveBeenCalledTimes(1)

    const hk1 = nodeRowOf(wrapper, 'hk-1')
    const adoptBtn = hk1.findAll('button').find((b) => b.text() === '采用')
    expect(adoptBtn).toBeTruthy()
    await adoptBtn.trigger('click')
    await flushPromises()

    expect(adminApi.setNodeCapacity).toHaveBeenCalledWith(11, 342000)
    expect(toast.success).toHaveBeenCalled()
    // 采用后重新拉一遍页面数据
    expect(adminApi.listRegions).toHaveBeenCalledTimes(2)
  })
})
