import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { installCanvasEnv } from './canvas-env'

/**
 * 仪表盘监控区块的数据渲染契约。
 *
 * 最要紧的两条：
 *  ① **没有数据就是 '—'，不能显示成 0**（节点没上报 ≠ 占用 0%）
 *  ② 拉取失败时保留上一次数据 + 显示「数据中断」，不要把页面清空
 */

const { adminApi } = vi.hoisted(() => ({ adminApi: {} }))
vi.mock('@/api/admin', () => ({ adminApi }))

import NodeMonitor from '@/components/admin/NodeMonitor.vue'

const panelNode = (overrides = {}) => ({
  id: 1,
  name: 'HK01',
  status: 'online',
  driver_type: '3x-ui',
  region: 'HK',
  latency: 42,
  metrics: { cpu: 10, mem: 20, conns: null, active_users: null, up_speed: null, down_speed: null, at: new Date().toISOString() },
  ...overrides,
})

const xrayNode = (overrides = {}) => ({
  id: 2,
  name: 'JP01',
  status: 'online',
  driver_type: 'xray',
  region: 'JP',
  latency: 0,
  metrics: {
    cpu: 30,
    mem: 40,
    conns: 5,
    active_users: 2,
    up_speed: 1024,
    down_speed: 2048,
    at: new Date().toISOString(),
  },
  ...overrides,
})

const mountMonitor = async (nodes, summary = null) => {
  adminApi.listNodes = vi.fn(() => Promise.resolve(nodes))
  // load() 现在连同 24h 汇总一起取（Promise.all），缺了这个 mock 整个 load 会进失败分支
  adminApi.monitorSummary = vi.fn(() => Promise.resolve(summary))
  const wrapper = mount(NodeMonitor)
  await flushPromises()

  return wrapper
}

beforeEach(() => {
  installCanvasEnv()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('NodeMonitor.vue', () => {
  it('汇总卡 4 张：24h 流量 / CPU / 内存 / 实时速率；覆盖地区在「节点地区」卡右上角', async () => {
    const wrapper = await mountMonitor([
      panelNode(),
      xrayNode(),
      panelNode({ id: 3, name: 'US01', status: 'offline', region: 'US', metrics: null }),
    ])

    const values = wrapper.findAll('.sc-value').map((n) => n.text())
    // 活跃人数/连接数已合并到上面的「用户总数」卡，这里不再重复
    expect(values.length).toBe(4)
    // [0] 近24h流量（summary 未 mock 数据 → —）
    expect(values[0]).toBe('—')
    expect(values[1]).toBe('20.0%') // (10 + 30) / 2
    expect(values[2]).toBe('30.0%') // (20 + 40) / 2
    // [3] 实时速率：大字是上下行**总和**（up 1024 + down 2048 B/s）
    expect(values[3].replace(/\s+/g, ' ')).toBe('3.0 KB/s')
    expect(wrapper.text()).not.toContain('活跃人数')
    // 覆盖地区挪到「节点地区」标题右上角
    const cov = wrapper.find('.nm-coverage')
    expect(cov.exists()).toBe(true)
    expect(cov.text()).toContain('覆盖 3 个地区')
  })

  it('平均 CPU / 内存只算在线节点，且没有读数时显示 — 而不是 0', async () => {
    const wrapper = await mountMonitor([
      panelNode({ metrics: null }), // 在线但没有任何读数
      xrayNode({ id: 2, status: 'offline', metrics: { cpu: 90, mem: 90, conns: 1, active_users: 0, up_speed: 0, down_speed: 0, at: new Date().toISOString() } }),
    ])

    const values = wrapper.findAll('.sc-value').map((n) => n.text())
    expect(values[0]).toBe('—')
    expect(values[1]).toBe('—')
  })

  it('活跃人数/连接数不再出现在监控汇总里（已并进业务概览的用户总数卡）', async () => {
    const wrapper = await mountMonitor([xrayNode()])

    expect(wrapper.text()).not.toContain('活跃人数')
    expect(wrapper.text()).not.toContain('仅 xray 节点上报')
  })

  it('节点卡片：地区名、状态、CPU/内存，缺失值显示 —', async () => {
    const wrapper = await mountMonitor([
      panelNode({ metrics: { cpu: 88, mem: null, conns: null, active_users: null, up_speed: null, down_speed: null, at: new Date().toISOString() } }),
    ])

    const card = wrapper.find('.nm-node')
    expect(card.text()).toContain('HK01')
    expect(card.text()).toContain('中国香港')
    expect(card.text()).toContain('在线')
    expect(card.text()).toContain('88%')
    expect(card.text()).toContain('—') // 内存缺失
  })

  it('资源分级：≥90 标 critical，70–89 标 warn', async () => {
    const wrapper = await mountMonitor([
      panelNode({ metrics: { cpu: 95, mem: 75, conns: null, active_users: null, up_speed: null, down_speed: null, at: new Date().toISOString() } }),
    ])

    const meters = wrapper.findAll('.nm-meter')
    expect(meters[0].classes()).toContain('is-critical')
    expect(meters[1].classes()).toContain('is-warn')
  })

  it('实时上下行只统计 xray 节点；全是面板节点时显示 —', async () => {
    const withXray = await mountMonitor([xrayNode()])
    // 实时速率在汇总卡（.sc-value 第 4 张）的说明行里
    expect(withXray.findAll('.nm-note-line').at(-1).text()).toContain('↑ 上传 1.0 KB/s · ↓ 下载 2.0 KB/s')

    const onlyPanel = await mountMonitor([panelNode()])
    expect(onlyPanel.findAll('.nm-note-line').at(-1).text()).toContain('↑ 上传 — · ↓ 下载 —')
  })

  it('拉取失败：显示数据中断横幅，并保留上一次数据不清空', async () => {
    const wrapper = await mountMonitor([panelNode()])
    expect(wrapper.find('.nm-banner').exists()).toBe(false)
    expect(wrapper.text()).toContain('HK01')

    adminApi.listNodes = vi.fn(() => Promise.reject(new Error('boom')))
    await wrapper.vm.load()
    await flushPromises()

    expect(wrapper.find('.nm-banner').exists()).toBe(true)
    expect(wrapper.text()).toContain('数据中断')
    // 关键：上一次的数据还在，没有被清成 0 / 空
    expect(wrapper.text()).toContain('HK01')
    // 覆盖地区已挪到「节点地区」卡右上角（不再是第 4 张汇总卡）
    expect(wrapper.find('.nm-coverage').text()).toContain('覆盖 1 个地区')
  })

  it('每 5 秒轮询一次，标签页隐藏时跳过', async () => {
    vi.useFakeTimers()
    adminApi.listNodes = vi.fn(() => Promise.resolve([panelNode()]))
    const wrapper = mount(NodeMonitor)
    await vi.advanceTimersByTimeAsync(0)
    expect(adminApi.listNodes).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(5000)
    expect(adminApi.listNodes).toHaveBeenCalledTimes(2)

    // 隐藏时不打接口
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => true })
    await vi.advanceTimersByTimeAsync(5000)
    expect(adminApi.listNodes).toHaveBeenCalledTimes(2)

    Object.defineProperty(document, 'hidden', { configurable: true, get: () => false })
    await vi.advanceTimersByTimeAsync(5000)
    expect(adminApi.listNodes).toHaveBeenCalledTimes(3)

    wrapper.unmount()
  })

  it('地区列表来自地球的聚合，点一下把地球转过去', async () => {
    const wrapper = await mountMonitor([panelNode(), xrayNode(), panelNode({ id: 3, name: 'HK02', status: 'offline', region: 'HK' })])

    const items = wrapper.findAll('.nm-region-list button')
    expect(items.map((n) => n.text().replace(/\s+/g, ''))).toEqual(['中国香港1/2', '日本1/1'])

    await items[0].trigger('click')
    // 点列表不该报错，且地球仍在画（焦点地区）
    expect(wrapper.findAll('.nm-region-list button').length).toBe(2)
  })

  it('节点没有可识别地区时如实提示，不硬塞一个地区', async () => {
    const wrapper = await mountMonitor([panelNode({ region: null })])

    expect(wrapper.text()).toContain('未识别地区')
    expect(wrapper.find('.nm-coverage').text()).toContain('1 个无法识别')
    expect(wrapper.text()).toContain('暂无可定位的地区')
  })
})
