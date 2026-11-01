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

const mountMonitor = async (nodes) => {
  adminApi.listNodes = vi.fn(() => Promise.resolve(nodes))
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
  it('汇总卡显示实时连接、平均 CPU / 内存、覆盖地区', async () => {
    const wrapper = await mountMonitor([
      panelNode(),
      xrayNode(),
      panelNode({ id: 3, name: 'US01', status: 'offline', region: 'US', metrics: null }),
    ])

    const values = wrapper.findAll('.sc-value').map((n) => n.text())
    expect(values[0]).toBe('5') // 只有 xray 节点上报连接数
    expect(values[1]).toBe('20.0%') // (10 + 30) / 2
    expect(values[2]).toBe('30.0%') // (20 + 40) / 2
    expect(values[3].replace(/\s+/g, ' ')).toBe('3 个') // HK / JP / US
  })

  it('平均 CPU / 内存只算在线节点，且没有读数时显示 — 而不是 0', async () => {
    const wrapper = await mountMonitor([
      panelNode({ metrics: null }), // 在线但没有任何读数
      xrayNode({ id: 2, status: 'offline', metrics: { cpu: 90, mem: 90, conns: 1, active_users: 0, up_speed: 0, down_speed: 0, at: new Date().toISOString() } }),
    ])

    const values = wrapper.findAll('.sc-value').map((n) => n.text())
    expect(values[1]).toBe('—')
    expect(values[2]).toBe('—')
  })

  it('实时连接只统计 xray 节点，没有上报时显示 —', async () => {
    const withXray = await mountMonitor([xrayNode()])
    expect(withXray.findAll('.sc-value')[0].text()).toBe('5')
    expect(withXray.text()).toContain('活跃用户 2')

    const onlyPanel = await mountMonitor([panelNode()])
    expect(onlyPanel.findAll('.sc-value')[0].text()).toBe('—')
    expect(onlyPanel.text()).toContain('活跃用户 —')
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
    expect(withXray.findAll('.nm-live strong').map((n) => n.text())).toEqual(['1.0 KB/s', '2.0 KB/s'])

    const onlyPanel = await mountMonitor([panelNode()])
    expect(onlyPanel.findAll('.nm-live strong').map((n) => n.text())).toEqual(['—', '—'])
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
    expect(wrapper.findAll('.sc-value')[3].text().replace(/\s+/g, ' ')).toBe('1 个')
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
    expect(wrapper.text()).toContain('1 个节点无法识别地区')
    expect(wrapper.text()).toContain('暂无可定位的地区')
  })
})
