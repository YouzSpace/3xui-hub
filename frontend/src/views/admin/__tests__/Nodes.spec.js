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
