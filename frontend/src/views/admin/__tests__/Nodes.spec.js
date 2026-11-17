import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 节点列表页 × 行内 agent 版本角标。
 *
 * 这里守的是「模板里调了、script 里没定义」这类只有渲染到那一行才会炸的 bug：
 * 列表模板用 `rowAgentOutdated(n)` / `rowAgentTitle(n)` 两处（有指标 / 无指标两个分支），
 * 函数一被删/一改名，挂载直接抛 "is not a function"，用例立刻红。
 *
 * 口径（见 Nodes.vue 注释）：have 与 want 都拿得到才比较，缺任何一个都算「未上报」，
 * 不能因为面板还没拿到 manifest 就把所有节点标成「落后」。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))
// 列表分页走 usePagedList（页码来自路由 query），这里不需要真路由
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}))

import Nodes from '@/views/admin/Nodes.vue'

const nodeBase = {
  port: 443, scheme: 'https', status: 'online', latency: 12,
  inbounds: { vless: [1], trojan: [], hysteria: [] },
}

// 四种上报状态各一行：落后 / 一致 / 节点没上报 / 面板没拿到 manifest
const OUTDATED = {
  ...nodeBase, id: 1, name: '老旧节点', driver_type: 'xray', host: 'n1.example.com',
  agent_version: '1.0.9', agent_latest: '1.1.0',
  agent: { cpu: 10, mem: 20, conns: 3 },
}
const SAME = {
  ...nodeBase, id: 2, name: '已是最新', driver_type: 'xray', host: 'n2.example.com',
  agent_version: '1.1.0', agent_latest: '1.1.0',
}
const NO_REPORT = {
  ...nodeBase, id: 3, name: '刚上线', driver_type: 'xray', host: 'n3.example.com',
  agent_version: null, agent_latest: '1.1.0',
}
const NO_MANIFEST = {
  ...nodeBase, id: 4, name: '面板没版本号', driver_type: 'xray', host: 'n4.example.com',
  agent_version: '1.0.9', agent_latest: null,
}

beforeEach(() => {
  adminApi.listNodes = vi.fn(() => Promise.resolve([OUTDATED, SAME, NO_REPORT, NO_MANIFEST]))
  vi.clearAllMocks()
})

const mounted = []
afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
})

const mountPage = async () => {
  const wrapper = mount(Nodes, { attachTo: document.body })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

/** 按节点名找到那一行（表格里一行一个节点） */
const rowOf = (wrapper, name) => wrapper.findAll('tbody tr').find((tr) => tr.text().includes(name))

describe('Nodes.vue — 行内 agent 版本判断函数', () => {
  it('rowAgentOutdated / rowAgentTitle 在组件上确实定义且可调用', async () => {
    const wrapper = await mountPage()

    // 模板里两处调用点都靠这两个函数；缺一个整页渲染就崩
    expect(typeof wrapper.vm.rowAgentOutdated).toBe('function')
    expect(typeof wrapper.vm.rowAgentTitle).toBe('function')
  })

  it('rowAgentOutdated：只有「两边都有且不一致」才算落后', async () => {
    const wrapper = await mountPage()
    const fn = wrapper.vm.rowAgentOutdated

    expect(fn(OUTDATED)).toBe(true)
    expect(fn(SAME)).toBe(false)
    expect(fn(NO_REPORT)).toBe(false)
    expect(fn(NO_MANIFEST)).toBe(false)
    // 空行 / 缺字段不能抛异常（列表首帧 nodes 还是空数组）
    expect(fn(null)).toBe(false)
    expect(fn(undefined)).toBe(false)
    expect(fn({})).toBe(false)
  })

  it('rowAgentTitle：缺 manifest 时给「未加载」而不是拿 null 比大小', async () => {
    const wrapper = await mountPage()
    const fn = wrapper.vm.rowAgentTitle

    expect(fn(OUTDATED)).toBe('节点上报 1.0.9 / 面板最新 1.1.0')
    // 节点没上报版本：文案里显式写「未上报」，不是空白
    expect(fn(NO_REPORT)).toBe('节点上报 未上报 / 面板最新 1.1.0')
    expect(fn(NO_MANIFEST)).toBe('面板还没拿到最新版本号（manifest 未加载）')
  })
})

describe('Nodes.vue — 行内 agent 角标渲染', () => {
  it('落后节点标黄，其余不标；提示文案各自对上', async () => {
    const wrapper = await mountPage()

    const outdated = rowOf(wrapper, '老旧节点').find('.an-agent-ver')
    expect(outdated.classes()).toContain('is-outdated')
    expect(outdated.attributes('title')).toBe('节点上报 1.0.9 / 面板最新 1.1.0')
    expect(outdated.text()).toContain('1.0.9')

    const same = rowOf(wrapper, '已是最新').find('.an-agent-ver')
    expect(same.classes()).not.toContain('is-outdated')
    expect(same.attributes('title')).toBe('节点上报 1.1.0 / 面板最新 1.1.0')

    // 没上报指标的分支（v-else-if）也要渲染出版本角标，不能只在有 metrics 时才出现
    const noReport = rowOf(wrapper, '刚上线').find('.an-agent-ver')
    expect(noReport.exists()).toBe(true)
    expect(noReport.classes()).not.toContain('is-outdated')
    expect(noReport.text()).toContain('未上报')

    const noManifest = rowOf(wrapper, '面板没版本号').find('.an-agent-ver')
    expect(noManifest.classes()).not.toContain('is-outdated')
    expect(noManifest.attributes('title')).toBe('面板还没拿到最新版本号（manifest 未加载）')
  })

  it('非 xray 节点（3x-ui 面板）不渲染 agent 角标', async () => {
    adminApi.listNodes = vi.fn(() => Promise.resolve([
      { ...nodeBase, id: 9, name: '面板节点', driver_type: '3x-ui', host: 'p1.example.com', username: 'admin' },
    ]))
    const wrapper = await mountPage()

    expect(rowOf(wrapper, '面板节点').find('.an-agent-ver').exists()).toBe(false)
  })
})

/**
 * 地区功能-13：节点列表新增「地区 / 绑定用户·容量 / 配置来源」三列。
 *
 * 这三列的数据来自后端新增字段（region_id / bound_users / capacity），
 * 后端还没合入时字段整体缺失 —— 那里必须显示「—」而不是把 undefined/null 印在页面上。
 * 所以这组用例同时钉两件事：字段在时的渲染口径，字段不在时的兜底。
 */
const REGION_NODES = [
  // 容量为 null = 不限（这就是变异测试要打的地方）
  { ...nodeBase, id: 21, name: '港A', driver_type: '3x-ui', host: 'hk1.example.com', region_id: 7, bound_users: 3, capacity: null },
  // 容量有值：原样显示数字
  { ...nodeBase, id: 22, name: '港B', driver_type: '3x-ui', host: 'hk2.example.com', region_id: 7, bound_users: 2, capacity: 50 },
  // 明确未归属（region_id = null）：全量下发
  { ...nodeBase, id: 23, name: '无地区', driver_type: 'xray', host: 'n9.example.com', region_id: null, bound_users: 0, capacity: null },
  // 老接口：三个字段全缺，一律「—」，不能崩也不能编
  { ...nodeBase, id: 24, name: '老字段', driver_type: 'xray', host: 'old.example.com' },
  // 连 driver_type 都没有的异常行：配置来源同样不能印 undefined
  { ...nodeBase, id: 25, name: '无来源', host: 'x.example.com' },
]

describe('Nodes.vue — 地区 / 绑定容量 / 配置来源', () => {
  beforeEach(() => {
    adminApi.listRegions = vi.fn(() => Promise.resolve([{ id: 7, name: '香港' }, { id: 8, name: '日本' }]))
    adminApi.listNodes = vi.fn(() => Promise.resolve(REGION_NODES))
  })

  it('表头多出「地区 / 绑定用户 / 容量 / 配置来源」三列', async () => {
    const wrapper = await mountPage()
    const heads = wrapper.findAll('thead th').map((th) => th.text())
    expect(heads).toContain('地区')
    expect(heads).toContain('绑定用户 / 容量')
    expect(heads).toContain('配置来源')
  })

  it('地区列：有归属显示地区名，null 显示「未归属（全量）」，字段缺失显示「—」', async () => {
    const wrapper = await mountPage()
    expect(rowOf(wrapper, '港A').find('.an-region').text()).toBe('香港')
    expect(rowOf(wrapper, '无地区').find('.an-region').text()).toBe('未归属（全量）')
    expect(rowOf(wrapper, '老字段').find('.an-region').text()).toBe('—')
  })

  it('绑定用户/容量：capacity 为空显示「不限」，有值原样显示', async () => {
    const wrapper = await mountPage()
    // 3 个绑定 + 不限容量
    expect(rowOf(wrapper, '港A').find('.an-binding').text()).toBe('3 / 不限')
    // 容量有上限就是数字，不能因为走了同一条格式化就变成「不限」
    expect(rowOf(wrapper, '港B').find('.an-binding').text()).toBe('2 / 50')
  })

  it('绑定用户/容量：bound_users 字段缺失显示「—」，不显示 undefined', async () => {
    const wrapper = await mountPage()
    const cell = rowOf(wrapper, '老字段').find('.an-binding').text()
    expect(cell).toBe('—')
    expect(cell).not.toContain('undefined')
  })

  it('配置来源：3x-ui 与 xray 各自文案，字段缺失「—」', async () => {
    const wrapper = await mountPage()
    expect(rowOf(wrapper, '港A').find('.an-source').text()).toBe('3x-ui（手动配置）')
    expect(rowOf(wrapper, '无地区').find('.an-source').text()).toBe('模板 / 自定义')
    expect(rowOf(wrapper, '无来源').find('.an-source').text()).toBe('—')
  })

  it('地区字典拉不到时（后端未合入）地区名退回 #id，页面照常渲染', async () => {
    adminApi.listRegions = vi.fn(() => Promise.reject({ message: '404' }))
    const wrapper = await mountPage()
    expect(rowOf(wrapper, '港A').find('.an-region').text()).toBe('#7')
    expect(rowOf(wrapper, '无地区').find('.an-region').text()).toBe('未归属（全量）')
  })

  it('地区字典是 data:{regions:[...]} 包裹形状时也要认（后端列表口径）', async () => {
    adminApi.listRegions = vi.fn(() => Promise.resolve({ regions: [{ id: 7, name: '香港' }] }))
    const wrapper = await mountPage()
    expect(rowOf(wrapper, '港A').find('.an-region').text()).toBe('香港')
  })
})

/**
 * 地区功能-22：编辑弹窗里容量框下方的「系统建议」一行。
 *
 * 数据来自 GET /nodes/:id/capacity-recommend（后端按最近 7 天内存样本反推）。
 * 两件事必须守住：
 *  1. 有推荐时显示「💡 系统建议 X 万（置信度 Y）」，「采用」只是把数字填进容量框（不直接提交）；
 *  2. 接口不存在（404 / 方法都没有）时整行不渲染，也不弹错误 —— 保存节点的链路不能受影响。
 */
describe('Nodes.vue — 容量建议', () => {
  const openEditOf = async (wrapper, name) => {
    // 行内三个按钮：测试 / 编辑 / 删除
    await rowOf(wrapper, name).findAll('button')[1].trigger('click')
    await flushPromises()
  }

  beforeEach(() => {
    adminApi.listRegions = vi.fn(() => Promise.resolve([{ id: 7, name: '香港' }]))
    adminApi.listNodes = vi.fn(() => Promise.resolve(REGION_NODES))
    adminApi.getNode = vi.fn((id) => Promise.resolve(REGION_NODES.find((n) => n.id === id)))
    adminApi.setNodeCapacity = vi.fn(() => Promise.resolve({}))
    adminApi.capacityRecommend = vi.fn(() =>
      Promise.resolve({ NodeID: 21, Suggest: 342000, PerUserKB: 0, Basis: 'percent', Confidence: 'high', Samples: 42, Reason: '按占用百分比斜率推算' }),
    )
  })

  it('弹窗里按 万 显示建议容量 + 置信度，「采用」把值填进容量框', async () => {
    const wrapper = await mountPage()
    await openEditOf(wrapper, '港A')

    const line = document.body.querySelector('[data-test="capacity-rec"]')
    expect(line).toBeTruthy()
    expect(line.textContent).toContain('系统建议 34.2 万')
    expect(line.textContent).toContain('置信度 高')

    const adoptBtn = [...line.querySelectorAll('button')].find((b) => b.textContent.trim() === '采用')
    expect(adoptBtn).toBeTruthy()
    adoptBtn.click()
    await flushPromises()

    // 采用 = 填进容量框（走原来的保存流程才落库，可反悔）
    expect(wrapper.vm.form.capacity).toBe('342000')
    expect(adminApi.setNodeCapacity).not.toHaveBeenCalled()
  })

  it('推荐接口不存在时不渲染这一行，也不弹错误，弹窗照常打开', async () => {
    adminApi.capacityRecommend = undefined
    const wrapper = await mountPage()
    await openEditOf(wrapper, '港A')

    expect(document.body.querySelector('[data-test="capacity-rec"]')).toBeNull()
    expect(toast.error).not.toHaveBeenCalled()
    expect(wrapper.vm.modalOpen).toBe(true)
    expect(wrapper.vm.form.capacity).toBe('')
  })

  it('接口报错（节点没有样本 / 404）同样静默：不渲染、不弹错误', async () => {
    adminApi.capacityRecommend = vi.fn(() => Promise.reject({ message: '404 page not found' }))
    const wrapper = await mountPage()
    await openEditOf(wrapper, '港A')

    expect(document.body.querySelector('[data-test="capacity-rec"]')).toBeNull()
    expect(toast.error).not.toHaveBeenCalled()
  })
})
