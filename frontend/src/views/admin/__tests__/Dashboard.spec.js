import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { installCanvasEnv } from '@/components/admin/__tests__/canvas-env'

/**
 * 仪表盘：业务概览 + 流量（排行 / 汇总曲线）。
 *
 * 两条口径要盯住：
 *  ① 服务器卡显示的是「在线 / 总数」+ 状态文案（全部在线 / 部分离线 / 全部离线）
 *  ② 任何一块数据挂了都不能把整行清空 —— 三块是各自独立的
 *
 * 流量那部分（原独立监控页并进来的）：
 *  ③ **没有数据就是「—」/「无数据」，不能显示成 0**
 *  ④ 曲线默认画**全机合计**（后端 series 末尾那条 aggregate 元素），不是第一个节点
 *  ⑤ 窗口切换必须重新取数；CSV 缺失值是空字段（不是 0，也不是「—」）
 *
 * 流量区块的两栏构成（对齐参考图）：
 *  ⑥ 左 2/3 = 曲线卡（标题随窗口变化）+「流量消耗 Top 5」；右 1/3 = CPU / 内存排行卡
 *  ⑦ 图例 4 项**不带数值**，当前读数只在 hover tooltip 里
 *  ⑧ Top5 带序号、只列 5 名、每行有「峰值 X at 今天/昨天 HH:MM」
 */

const { adminApi } = vi.hoisted(() => ({ adminApi: {} }))
vi.mock('@/api/admin', () => ({ adminApi }))

import Dashboard from '@/views/admin/Dashboard.vue'

const nodes = [
  { id: 1, name: 'HK01', status: 'online', inbounds: { vless: [1, 2] } },
  { id: 2, name: 'JP01', status: 'online', inbounds: { vless: [3] } },
  { id: 3, name: 'US01', status: 'offline', inbounds: {} },
]

/**
 * 峰值时刻要「今天 / 昨天」两种相对日，所以按运行当天动态算 ——
 * 写死日期的话这个用例明天就红了。
 */
const localIso = (dayOffset, hhmm) => {
  const [h, m] = hhmm.split(':').map(Number)
  const d = new Date()
  d.setDate(d.getDate() + dayOffset)
  d.setHours(h, m, 0, 0)
  return d.toISOString()
}

/** 窗口 → 后端返回的样本，便于断言「切换窗口真的换了数据」。 */
const SUMMARY = {
  '24h': {
    totals: { up: 3 * 1024 ** 3, down: 1024 ** 3, total: 4 * 1024 ** 3 },
    window: { key: '24h', start: '2026-10-13T00:00:00+08:00', end: '2026-10-14T00:00:00+08:00' },
    nodes: [
      {
        id: 1, name: 'HK01', up: 1024 ** 3, down: 1024 ** 3, total: 2 * 1024 ** 3,
        cpu_avg: 12.5, cpu_max: 40, mem_avg: 30, mem_max: 55,
        peak_rate: 1048576, peak_rate_at: localIso(0, '11:00'),
        cpu_max_at: localIso(0, '11:00'), mem_max_at: localIso(0, '11:00'),
      },
      {
        id: 2, name: 'JP01', up: 2 * 1024 ** 3, down: 0, total: 2 * 1024 ** 3,
        cpu_avg: 5, cpu_max: 9, mem_avg: null, mem_max: null,
        peak_rate: 2097152, peak_rate_at: localIso(-1, '09:30'),
        cpu_max_at: localIso(-1, '09:30'), mem_max_at: null,
      },
      // 窗口内没有任何采样：全部字段为 null（不是 0）
      {
        id: 3, name: 'US01', up: null, down: null, total: null, cpu_avg: null, cpu_max: null, mem_avg: null, mem_max: null,
        peak_rate: null, peak_rate_at: null, cpu_max_at: null, mem_max_at: null,
      },
    ],
  },
  '7d': {
    totals: { up: 5 * 1024 ** 3, down: 5 * 1024 ** 3, total: 10 * 1024 ** 3 },
    window: { key: '7d', start: '2026-10-07T00:00:00+08:00', end: '2026-10-14T00:00:00+08:00' },
    nodes: [
      {
        id: 1, name: 'HK01', up: 5 * 1024 ** 3, down: 5 * 1024 ** 3, total: 10 * 1024 ** 3,
        cpu_avg: 20, cpu_max: 60, mem_avg: 40, mem_max: 70,
        peak_rate: 4096, peak_rate_at: localIso(-3, '08:15'),
        cpu_max_at: localIso(-3, '08:15'), mem_max_at: localIso(-3, '08:15'),
      },
    ],
  },
}

/**
 * series：节点元素在前，**全机汇总元素在末尾**（node_id=null + aggregate=true）。
 * 汇总的点 = 各节点同桶相加，这里直接给好结果，测的是前端有没有用它。
 */
const SERIES = {
  // 24h：5 分钟一个点，值是该区间增量字节 → 速率以 300s 为分母
  '24h': [
    { node_id: 1, name: 'HK01', points: [{ t: 1000, up: 300, down: 600 }, { t: 1300, up: 600, down: 0 }] },
    { node_id: 2, name: 'JP01', points: [{ t: 1000, up: 150, down: 150 }] },
    { node_id: 3, name: 'US01', points: [] },
    {
      node_id: null,
      name: '合计',
      aggregate: true,
      points: [{ t: 1000, up: 450, down: 750 }, { t: 1300, up: 600, down: 0 }],
    },
  ],
  '7d': [
    { node_id: 1, name: 'HK01', points: [{ t: 1000, up: 3600, down: 7200 }] },
    { node_id: null, name: '合计', aggregate: true, points: [{ t: 1000, up: 3600, down: 7200 }] },
  ],
}

/**
 * 用户×机器流量（issue #43）：
 *  - 用户榜是套餐口径 weighted（乘倍率），up/down 是原始字节；
 *  - data_start 是新表最早有数据的那天（界面必须标出来，否则「还没攒够」会被读成「没人用」）；
 *  - 没有数据的用户/机器不出现在 rows 里。
 */
const USER_RANKING = {
  '24h': {
    window: '24h', start: '2026-10-17 12:00:00', end: '2026-10-18 12:00:00', data_start: '2026-10-17',
    rows: [
      {
        user_id: 1, email: 'alice@example.com', weighted: 3 * 1024 ** 3, up: 1024 ** 3, down: 2 * 1024 ** 3,
        plan_type: 'period', plan_name: '月付', plan_months: 1,
      },
      {
        user_id: 2, email: 'bob@example.com', weighted: 1024 ** 3, up: 512 * 1024 ** 2, down: 512 * 1024 ** 2,
        plan_type: 'total', plan_name: '年付', plan_months: null,
      },
    ],
  },
  '7d': {
    window: '7d', start: '2026-10-12', end: '2026-10-18', data_start: '2026-10-17',
    rows: [
      {
        user_id: 2, email: 'bob@example.com', weighted: 5 * 1024 ** 3, up: 1024 ** 3, down: 4 * 1024 ** 3,
        plan_type: 'total', plan_name: '年付', plan_months: null,
      },
    ],
  },
  '30d': {
    window: '30d', start: '2026-09-19', end: '2026-10-18', data_start: '2026-10-17',
    rows: [
      {
        user_id: 2, email: 'bob@example.com', weighted: 8 * 1024 ** 3, up: 2 * 1024 ** 3, down: 6 * 1024 ** 3,
        plan_type: 'total', plan_name: '年付', plan_months: null,
      },
    ],
  },
}

/**
 * 机器卡：网卡口径（nic_*）+ 用户代理原始（user_raw）+ 倍率，三列各自独立。
 * JP01 故意做成「网卡远大于用户代理」——这是真实分布（同一字节走两遍 + 系统流量）。
 */
const NODE_RANKING = {
  '24h': {
    window: '24h', start: '2026-10-17 12:00:00', end: '2026-10-18 12:00:00', data_start: '2026-10-17',
    rows: [
      {
        node_id: 1, name: 'HK01', weighted: 2 * 1024 ** 3, up: 1024 ** 3, down: 1024 ** 3,
        user_raw: 2 * 1024 ** 3, multiplier: 1.5,
        nic_up: 12 * 1024 ** 3, nic_down: 8 * 1024 ** 3, nic_total: 20 * 1024 ** 3,
      },
      {
        node_id: 2, name: 'JP01', weighted: 1024 ** 3, up: 512 * 1024 ** 2, down: 512 * 1024 ** 2,
        user_raw: 1024 ** 3, multiplier: 2,
        nic_up: 5 * 1024 ** 3, nic_down: 5 * 1024 ** 3, nic_total: 10 * 1024 ** 3,
      },
    ],
  },
  '7d': { window: '7d', start: '2026-10-12', end: '2026-10-18', data_start: '2026-10-17', rows: [] },
  '30d': { window: '30d', start: '2026-09-19', end: '2026-10-18', data_start: '2026-10-17', rows: [] },
}

/** 单用户按机器明细：核心视角（这个用户在哪台机器用了多少）+ 倍率 + 占该用户百分比。 */
const USER_NODES = {
  1: {
    window: '24h', start: '2026-10-17 12:00:00', end: '2026-10-18 12:00:00', data_start: '2026-10-17',
    rows: [
      { node_id: 1, name: 'HK01', weighted: 2 * 1024 ** 3, up: 512 * 1024 ** 2, down: 1536 * 1024 ** 2, multiplier: 1.5, share: 66.7 },
      { node_id: 2, name: 'JP01', weighted: 1024 ** 3, up: 512 * 1024 ** 2, down: 512 * 1024 ** 2, multiplier: 2, share: 33.3 },
    ],
  },
  2: {
    window: '24h', start: '2026-10-17 12:00:00', end: '2026-10-18 12:00:00', data_start: '2026-10-17',
    rows: [{ node_id: 1, name: 'HK01', weighted: 1024 ** 3, up: 512 * 1024 ** 2, down: 512 * 1024 ** 2, multiplier: 1.5, share: 100 }],
  },
}

const mountDashboard = async (overrides = {}) => {
  adminApi.listUsers = overrides.listUsers ?? vi.fn(() => {
    const page = [{ id: 1, email: 'a@example.com', enabled: true }]
    page.pagination = { total: 42 }
    return Promise.resolve(page)
  })
  adminApi.listNodes = overrides.listNodes ?? vi.fn(() => Promise.resolve(nodes))
  adminApi.getRevenueStats = overrides.getRevenueStats ?? vi.fn(() =>
    Promise.resolve({ today: { amount: 128.5, orders: 3 }, total: { amount: 9640.25, orders: 88 } }),
  )
  adminApi.monitorSummary = overrides.monitorSummary ?? vi.fn((w) => Promise.resolve(SUMMARY[w]))
  adminApi.monitorSeries = overrides.monitorSeries ?? vi.fn((w) => Promise.resolve(SERIES[w]))
  adminApi.userTrafficRanking = overrides.userTrafficRanking ?? vi.fn((w) => Promise.resolve(USER_RANKING[w]))
  adminApi.nodeTrafficRanking = overrides.nodeTrafficRanking ?? vi.fn((w) => Promise.resolve(NODE_RANKING[w]))
  adminApi.userTrafficNodes = overrides.userTrafficNodes ?? vi.fn((id) => Promise.resolve(USER_NODES[id]))

  const wrapper = mount(Dashboard, { global: { stubs: { RouterLink: true } } })
  await flushPromises()

  return wrapper
}

const cardValues = (wrapper) => wrapper.findAll('.sc-value').map((n) => n.text())

beforeEach(() => {
  installCanvasEnv()
})

describe('Dashboard.vue 业务概览', () => {
  it('显示用户总数（含活跃/连接）、服务器在线/总数、节点数、24h 流量、今日成交额', async () => {
    const wrapper = await mountDashboard()
    const values = cardValues(wrapper)

    expect(values[0]).toBe('42')
    expect(values[1].replace(/\s+/g, '')).toBe('2/3')
    expect(values[2]).toBe('3') // 入站配置总数 = 2 + 1 + 0
    expect(values[3]).toBe('¥128.50') // 今日成交额仍在第 4 格（位置没动）
    // 近 24 小时流量（上行 3 GB + 下行 1 GB = 4 GB），排在成交额之后
    expect(values[4]).toBe('4.0 GB')
    expect(wrapper.text()).toContain('部分离线')
    expect(wrapper.text()).toContain('累计 ¥9640.25')
    expect(wrapper.text()).toContain('3 单')
    // 用户卡：活跃人数与连接数并进说明行（不再有「注册用户数」这行冗余文案）
    expect(wrapper.text()).toContain('活跃')
    expect(wrapper.text()).not.toContain('注册用户数')
    // 流量卡：上下行分两行 + TOP1 机器徽章
    expect(wrapper.text()).toContain('↑ 上传')
    expect(wrapper.text()).toContain('↓ 下载')
    expect(wrapper.text()).toContain('TOP1')
    // 说明行独占一行（用户卡；流量卡已移到 NodeMonitor，用它的 nm-note-line）
    expect(wrapper.findAll('.dsh-note-line').length).toBe(1)
  })

  it('全部在线 / 全部离线的状态文案', async () => {
    const online = await mountDashboard({ listNodes: vi.fn(() => Promise.resolve(nodes.filter((n) => n.status === 'online'))) })
    expect(online.text()).toContain('全部在线')

    const offline = await mountDashboard({
      listNodes: vi.fn(() => Promise.resolve(nodes.map((n) => ({ ...n, status: 'offline' })))),
    })
    expect(offline.text()).toContain('全部离线')
  })

  it('成交额拿不到时显示 —，不显示 ¥0.00', async () => {
    const wrapper = await mountDashboard({ getRevenueStats: vi.fn(() => Promise.reject(new Error('boom'))) })

    // 拿不到就是拿不到：显示 ¥0.00 等于谎报「今天没成交」
    // 索引 3 = 今日成交额（0 用户 / 1 服务器 / 2 节点 / 3 成交额 / 4 近24h流量）
    expect(cardValues(wrapper)[3]).toBe('—')
    expect(wrapper.text()).toContain('—')
  })

  it('用户接口挂了不影响服务器/成交额那几张卡', async () => {
    const wrapper = await mountDashboard({ listUsers: vi.fn(() => Promise.reject(new Error('boom'))) })

    expect(cardValues(wrapper)[0]).toBe('0')
    expect(cardValues(wrapper)[1].replace(/\s+/g, '')).toBe('2/3')
    expect(cardValues(wrapper)[3]).toBe('¥128.50')
  })
})

describe('Dashboard.vue 流量消耗 Top 5', () => {
  it('挂载时按默认窗口 24h 拉取 summary + series', async () => {
    await mountDashboard()

    expect(adminApi.monitorSummary).toHaveBeenCalledWith('24h')
    expect(adminApi.monitorSeries).toHaveBeenCalledWith('24h')
  })

  it('TOP 排行按双向合计降序，null 排最后并标「无数据」', async () => {
    const wrapper = await mountDashboard()
    const names = wrapper.findAll('.dsh-rank-name').map((n) => n.text())

    // HK01 与 JP01 都是 2 GB，US01（null）必须最后
    expect(names[0]).toBe('HK01')
    expect(names[1]).toBe('JP01')
    expect(names[2]).toBe('US01')

    const last = wrapper.findAll('.dsh-rank-item')[2]
    expect(last.find('.dsh-rank-badge').text()).toBe('无数据')
    // 没有数据不画进度条（不是 0%）
    expect(last.find('.dsh-bar b').attributes('style')).toContain('width: 0%')
  })

  it('排行里不显示 0 冒充流量：无数据节点的三列都是 —', async () => {
    const wrapper = await mountDashboard()
    const us = wrapper.findAll('.dsh-rank-item')[2]

    expect(us.text()).toContain('↑ —')
    expect(us.text()).toContain('↓ —')
    expect(us.text()).not.toContain('0 B')
  })

  it('切换窗口触发重新取数（两个接口都换 window）', async () => {
    const wrapper = await mountDashboard()
    // NodeMonitor 的汇总卡也会取一次 24h 汇总，这里不再断言次数，只看参数
    expect(adminApi.monitorSummary).toHaveBeenLastCalledWith('24h')

    wrapper.vm.windowKey = '7d'
    await flushPromises()

    expect(adminApi.monitorSummary).toHaveBeenLastCalledWith('7d')
    expect(adminApi.monitorSeries).toHaveBeenLastCalledWith('7d')
    // 7d 只剩一台节点（Top5 里就是 HK01；CPU/内存排行卡也各有一行 HK01）
    expect(wrapper.findAll('.dsh-col--wide .dsh-rank-name').map((n) => n.text())).toEqual(['HK01'])
  })

  it('窗口非法回退：后端给什么 key 就用什么，页面只认自己请求的 window', async () => {
    const wrapper = await mountDashboard()
    wrapper.vm.windowKey = 'month'
    await flushPromises()

    expect(adminApi.monitorSummary).toHaveBeenLastCalledWith('month')
  })

  it('全空：窗口内没有任何采样时给出「暂无采样数据」提示，不显示 0 的合计', async () => {
    const empty = {
      totals: { up: 0, down: 0, total: 0 },
      window: { key: '24h', start: '', end: '' },
      nodes: [
        { id: 1, name: 'HK01', up: null, down: null, total: null, cpu_avg: null, cpu_max: null, mem_avg: null, mem_max: null },
      ],
    }
    const wrapper = await mountDashboard({ monitorSummary: vi.fn(() => Promise.resolve(empty)) })

    expect(wrapper.text()).toContain('暂无采样数据：采集随每 5 分钟健康检查进行，稍后即可看到')
    // 没有数据就不画曲线卡（合计卡也已经取消，同一数据只出现在曲线右上）
    expect(wrapper.find('.dsh-plot').exists()).toBe(false)
  })

  it('加载失败：显示失败横幅 + 重试，且不清空上一次的数据', async () => {
    const wrapper = await mountDashboard()
    expect(wrapper.find('.dsh-banner').exists()).toBe(false)
    expect(wrapper.text()).toContain('HK01')

    adminApi.monitorSummary = vi.fn(() => Promise.reject({ message: 'boom' }))
    adminApi.monitorSeries = vi.fn(() => Promise.reject({ message: 'boom' }))
    await wrapper.vm.loadTraffic()
    await flushPromises()

    expect(wrapper.find('.dsh-banner').exists()).toBe(true)
    expect(wrapper.text()).toContain('加载失败')
    // 关键：上一次成功的数据还在，没有被清成 0 / 空
    expect(wrapper.text()).toContain('HK01')
  })
})

describe('Dashboard.vue 汇总曲线', () => {
  it('默认画「合计」视图，不是第一个节点', async () => {
    const wrapper = await mountDashboard()

    expect(wrapper.vm.selectedView).toBe('aggregate')
    // 合计末点：up 600 / 300s = 2 B/s；down 0 / 300 = 0 B/s
    expect(wrapper.vm.chartSamples[1].upRate).toBeCloseTo(2, 6)
    expect(wrapper.vm.chartSamples[1].downRate).toBe(0)
  })

  it('画 4 条线：两条速率（实线）+ 两条累计（虚线，走右轴）', async () => {
    const wrapper = await mountDashboard()
    const keys = wrapper.vm.chart.paths.map((p) => p.key)

    expect(keys).toEqual(['upRate', 'downRate', 'upCum', 'downCum'])

    const lines = wrapper.findAll('.dsh-line')
    expect(lines.length).toBe(4)
    expect(lines[2].classes()).toContain('is-cumulative')
    expect(lines[3].classes()).toContain('is-cumulative')
    // 累计走右轴量程，与速率量程分开
    expect(wrapper.vm.chart.ceiling2).toBeGreaterThan(wrapper.vm.chart.ceiling)
  })

  it('累计量在窗口内单调递增（是增量累加，不是采样表的绝对值）', async () => {
    const wrapper = await mountDashboard()
    const samples = wrapper.vm.chartSamples

    // 合计：450 → 450+600 = 1050
    expect(samples[0].upCum).toBe(450)
    expect(samples[1].upCum).toBe(1050)
    expect(samples[1].downCum).toBeGreaterThanOrEqual(samples[0].downCum)
  })

  it('右上角显示本窗口全部节点合计的上传/下载', async () => {
    const wrapper = await mountDashboard()

    expect(wrapper.find('.dsh-total').text()).toBe('↑ 总上传 3.0 GB · ↓ 总下载 1.0 GB')
  })

  it('图例 4 项且不带数值，带色点与实/虚线型区分', async () => {
    const wrapper = await mountDashboard()
    const items = wrapper.findAll('.dsh-legend .dsh-legend-item')

    expect(items.length).toBe(4)
    expect(items[0].text()).toBe('上传速率')
    expect(items[1].text()).toBe('下载速率')
    expect(items[2].text()).toBe('累计上传')
    expect(items[3].text()).toBe('累计下载')
    // 后两项是虚线（图例色点用描边方块区分）
    expect(items[2].find('.dsh-dot--dashed').exists()).toBe(true)
    expect(items[3].find('.dsh-dot--dashed').exists()).toBe(true)
    // 图例四项本身没有数值 —— 当前读数只在 hover tooltip 里
    expect(items.map((n) => n.text()).join('')).not.toMatch(/\d/)
    // 最右是本窗口合计
    expect(wrapper.find('.dsh-legend .dsh-total').text()).toBe('↑ 总上传 3.0 GB · ↓ 总下载 1.0 GB')
  })

  it('切到单节点：曲线改画那台机器，且不再有节点数据时回落合计', async () => {
    const wrapper = await mountDashboard()

    wrapper.vm.selectedView = 1
    await flushPromises()
    expect(wrapper.vm.chartSamples[1].upRate).toBeCloseTo(2, 6)

    // 7d 里只有 HK01 → 选 2 不存在的节点时回落合计
    wrapper.vm.windowKey = '7d'
    await flushPromises()
    wrapper.vm.selectedView = 2
    await wrapper.vm.loadTraffic()
    await flushPromises()

    expect(wrapper.vm.selectedView).toBe('aggregate')
  })

  it('节点下拉选项第一项是「合计」，节点名在后', async () => {
    const wrapper = await mountDashboard()
    const options = wrapper.vm.viewOptions

    expect(options[0]).toEqual({ label: '合计（全部节点）', value: 'aggregate' })
    expect(options.slice(1).map((o) => o.label)).toEqual(['HK01', 'JP01', 'US01'])
  })

  it('hover 取最近采样点显示四值 tooltip，离开后 tooltip 消失', async () => {
    const wrapper = await mountDashboard()

    // 没 hover 时没有 tooltip（读数不常驻图例）
    expect(wrapper.find('.dsh-tip').exists()).toBe(false)
    expect(wrapper.vm.hoverIndex).toBeNull()

    wrapper.vm.hoverIndex = 0
    await flushPromises()

    // 首点四值：upRate 450/300 = 1.5 B/s（B 级按整数 → 2 B/s）、upCum 450 B
    const tip = wrapper.find('.dsh-tip')
    expect(tip.exists()).toBe(true)
    expect(tip.text()).toContain('上传速率')
    expect(tip.text()).toContain('下载速率')
    expect(tip.text()).toContain('累计上传')
    expect(tip.text()).toContain('累计下载')
    expect(tip.text()).toContain('450 B')
    expect(wrapper.find('.dsh-hover').exists()).toBe(true)

    // 离开 → tooltip 与竖线一起消失
    wrapper.vm.hoverIndex = null
    await flushPromises()
    expect(wrapper.find('.dsh-tip').exists()).toBe(false)
    expect(wrapper.find('.dsh-hover').exists()).toBe(false)
  })

  it('无采样点：显示占位提示，不画线', async () => {
    const wrapper = await mountDashboard({
      monitorSeries: vi.fn((w) =>
        Promise.resolve([
          ...SERIES[w].filter((item) => item.aggregate !== true),
          { node_id: null, name: '合计', aggregate: true, points: [] },
        ]),
      ),
    })

    expect(wrapper.text()).toContain('在此时间窗内暂无采样数据')
    expect(wrapper.vm.chart.count).toBe(0)
  })
})

describe('Dashboard.vue 两栏排行（Top5 / CPU / 内存）', () => {
  it('两栏结构：左栏 = 曲线卡 + Top5，右栏 = CPU / 内存排行', async () => {
    const wrapper = await mountDashboard()

    const wide = wrapper.find('.dsh-col--wide')
    const narrow = wrapper.find('.dsh-col--narrow')
    expect(wide.exists()).toBe(true)
    expect(narrow.exists()).toBe(true)

    // 左栏上曲线、下 Top5
    expect(wide.find('.dsh-section-title').text()).toBe('最近24小时流量')
    const wideTitles = wide.findAll('.dsh-section-title').map((n) => n.text())
    expect(wideTitles).toEqual(['最近24小时流量', '流量消耗 Top 5'])

    // 右栏 CPU 在上、内存在下
    expect(narrow.findAll('.dsh-section-title').map((n) => n.text())).toEqual([
      '平均 CPU 占用排行',
      '平均内存占用百分比排行',
    ])
  })

  it('曲线卡标题随窗口变化', async () => {
    const wrapper = await mountDashboard()

    expect(wrapper.vm.trafficTitle).toBe('最近24小时流量')
    wrapper.vm.windowKey = '7d'
    await flushPromises()
    expect(wrapper.vm.trafficTitle).toBe('最近7天流量')
    wrapper.vm.windowKey = '30d'
    await flushPromises()
    expect(wrapper.vm.trafficTitle).toBe('最近30天流量')
    wrapper.vm.windowKey = 'month'
    await flushPromises()
    expect(wrapper.vm.trafficTitle).toBe('本月流量')
  })

  it('Top5 带序号，且最多只列 5 名', async () => {
    const wrapper = await mountDashboard()
    const items = wrapper.findAll('.dsh-col--wide .dsh-rank-item')

    expect(items.length).toBe(3)
    expect(items.map((n) => n.find('.dsh-rank-seq').text())).toEqual(['1.', '2.', '3.'])
    // 序号与节点名同序：HK01 / JP01（都是 2 GB）→ US01（无数据）
    expect(wrapper.findAll('.dsh-col--wide .dsh-rank-name').map((n) => n.text())).toEqual(['HK01', 'JP01', 'US01'])

    // 6 台节点 → 只取前 5
    const many = Array.from({ length: 6 }, (_, i) => ({
      id: i + 1, name: `N${i + 1}`, up: (6 - i) * 1024 ** 2, down: 0, total: (6 - i) * 1024 ** 2,
      cpu_avg: 10, cpu_max: 20, mem_avg: 10, mem_max: 20,
      peak_rate: 1, peak_rate_at: '2026-10-14T11:00:00+08:00', cpu_max_at: '2026-10-14T11:00:00+08:00', mem_max_at: '2026-10-14T11:00:00+08:00',
    }))
    const capped = await mountDashboard({
      monitorSummary: vi.fn(() => Promise.resolve({ totals: { up: 0, down: 0, total: 0 }, window: { key: '24h', start: '', end: '' }, nodes: many })),
    })

    expect(capped.findAll('.dsh-col--wide .dsh-rank-item').length).toBe(5)
    expect(capped.findAll('.dsh-col--wide .dsh-rank-name').map((n) => n.text())).toEqual(['N1', 'N2', 'N3', 'N4', 'N5'])
  })

  it('Top5 每行显示峰值速率与时刻（今天 / 昨天）', async () => {
    const wrapper = await mountDashboard()
    const rows = wrapper.findAll('.dsh-col--wide .dsh-rank-item')

    // HK01：peak_rate 1048576 B/s = 1.0 MB/s，时刻是今天 11:00
    const peaks = rows.map((n) => (n.find('.dsh-rank-peak').exists() ? n.find('.dsh-rank-peak').text() : null))
    expect(peaks[0]).toContain('峰值 1.0 MB/s at 今天 11:00')
    expect(peaks[1]).toContain('峰值 2.0 MB/s at 昨天 09:30')
    // US01 没有峰值信息 → 不显示这一行（不是「峰值 — at —」）
    expect(peaks[2]).toBeNull()
  })

  it('CPU 排行：按平均降序、右侧平均 %、峰值时刻、success 色进度条', async () => {
    const wrapper = await mountDashboard()
    const cpu = wrapper.findAll('.dsh-col--narrow .ch-card').at(0)

    expect(cpu.find('.dsh-section-title').text()).toBe('平均 CPU 占用排行')
    expect(cpu.findAll('.dsh-rank-name').map((n) => n.text())).toEqual(['HK01', 'JP01', 'US01'])
    expect(cpu.findAll('.dsh-rank-value').map((n) => n.text())).toEqual(['12.5%', '5.0%', '—'])
    expect(cpu.findAll('.dsh-rank-peak')[0].text()).toContain('峰值 40% at 今天 11:00')
    expect(cpu.find('.dsh-bar--cpu').exists()).toBe(true)
    // 无数据节点不画条（不是 0%）
    expect(cpu.findAll('.dsh-bar')[2].classes()).toContain('is-unknown')
  })

  it('内存排行：按平均降序，mem 为 null 的节点排在最后', async () => {
    const wrapper = await mountDashboard()
    const mem = wrapper.findAll('.dsh-col--narrow .ch-card').at(1)

    expect(mem.find('.dsh-section-title').text()).toBe('平均内存占用百分比排行')
    // HK01 mem_avg 30 → JP01 / US01 的 mem 是 null（垫底）
    expect(mem.findAll('.dsh-rank-name').map((n) => n.text())).toEqual(['HK01', 'JP01', 'US01'])
    expect(mem.findAll('.dsh-rank-value').map((n) => n.text())).toEqual(['30.0%', '—', '—'])
    expect(mem.find('.dsh-bar--mem').exists()).toBe(true)
  })

  it('流量条与资源条用不同颜色类（accent vs success）', async () => {
    const wrapper = await mountDashboard()

    expect(wrapper.find('.dsh-bar--cpu').exists()).toBe(true)
    expect(wrapper.find('.dsh-bar--mem').exists()).toBe(true)
    // 流量条不带资源类（仍是 accent）
    expect(wrapper.find('.dsh-col--wide .dsh-bar').classes()).not.toContain('dsh-bar--cpu')
  })

  it('全空窗口：三张排行卡都给「暂无采样数据」而不是 0', async () => {
    const empty = {
      totals: { up: 0, down: 0, total: 0 },
      window: { key: '24h', start: '', end: '' },
      nodes: [
        { id: 1, name: 'HK01', up: null, down: null, total: null, cpu_avg: null, cpu_max: null, mem_avg: null, mem_max: null, peak_rate: null, peak_rate_at: null, cpu_max_at: null, mem_max_at: null },
      ],
    }
    const wrapper = await mountDashboard({ monitorSummary: vi.fn(() => Promise.resolve(empty)) })

    expect(wrapper.text()).toContain('暂无采样数据：采集随每 5 分钟健康检查进行，稍后即可看到')
    // 排行里不出现 0% 冒充占用
    expect(wrapper.find('.dsh-col--narrow .dsh-rank-value').text()).toBe('—')
    // 只看机器监控那两张卡（下半部分「用户×机器流量」是另一套数据源）
    expect(wrapper.findAll('.dsh-traffic .dsh-bar b').every((n) => n.attributes('style').includes('width: 0%'))).toBe(true)
  })
})

describe('Dashboard.vue 导出 CSV', () => {
  it('CSV：表头 + 行与排行同序，缺失值是空字段（不是 0，也不是 —）', async () => {
    const wrapper = await mountDashboard()
    const csv = wrapper.vm.buildCsv()
    const lines = csv.split('\n')

    expect(lines[0]).toBe('节点,上行(字节),下行(字节),双向(字节),CPU均值(%),内存均值(%)')
    expect(lines.length).toBe(4) // 表头 + 3 台节点
    expect(lines[1]).toBe(`HK01,${1024 ** 3},${1024 ** 3},${2 * 1024 ** 3},12.5,30`)
    // US01 全 null → 除节点名外全是空字段
    expect(lines[3]).toBe('US01,,,,,')
    expect(lines[3].split(',').slice(1)).toEqual(['', '', '', '', ''])
    expect(lines[3]).not.toContain('—')
  })

  it('CSV：文件名带 window 与日期', async () => {
    const wrapper = await mountDashboard()
    expect(wrapper.vm.csvFilename()).toMatch(/^monitor-24h-\d{4}-\d{2}-\d{2}\.csv$/)
  })

  it('CSV 生成后下载里传入了 blob 与文件名', async () => {
    const wrapper = await mountDashboard()

    const createUrl = vi.fn(() => 'blob:mock')
    const revokeUrl = vi.fn()
    global.URL.createObjectURL = createUrl
    global.URL.revokeObjectURL = revokeUrl
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})

    wrapper.vm.exportCsv()
    await flushPromises()

    expect(createUrl).toHaveBeenCalledTimes(1)
    expect(click).toHaveBeenCalledTimes(1)
    expect(revokeUrl).toHaveBeenCalledTimes(1)
  })
})

/**
 * 用户 × 机器流量（issue #43，按设计稿 issue43-preview 改版为表格）。
 *
 * 五条口径：
 *  ① 用户榜是表格：排名 / 打码邮箱+#id / 套餐用量（weighted）/ 占比 / ↑原始↓原始 / 套餐类型 / 明细；
 *  ② 点用户行 → 展开该用户按机器的明细（机器 + 占该用户百分比 + 倍率标注），再点收起；
 *  ③ 机器卡三列各自独立：网卡合计（nic_total）/ 其中用户代理原始（user_raw）/ 倍率 —— 不互相换算、不互相冒充；
 *  ④ 切换窗口必须同时重取两个排行榜，展开中的明细跟着新窗口刷新；
 *  ⑤ 新表从上线当天累积，必须显示「数据自 X 起」（缺数据 ≠ 0，也不显示 0 冒充流量）。
 */
describe('Dashboard.vue 用户 × 机器流量', () => {
  const userCards = (wrapper) => wrapper.findAll('.dsh-ut-card')
  const userTable = (wrapper) => userCards(wrapper).at(0)
  const userRowsOf = (wrapper) => userTable(wrapper).findAll('tbody .dsh-ut-row')

  it('挂载时按默认窗口拉取两个排行榜，标题带窗口名', async () => {
    const wrapper = await mountDashboard()

    expect(adminApi.userTrafficRanking).toHaveBeenCalledWith('24h')
    expect(adminApi.nodeTrafficRanking).toHaveBeenCalledWith('24h')
    expect(wrapper.text()).toContain('用户流量排行 · 近 24 小时')
    // issue #47：机器流量不再单独占一张常驻卡，改由「流量消耗 Top 5」的「查看全部」弹窗承载
    expect(wrapper.text()).not.toContain('机器流量 · 近 24 小时')
    expect(userCards(wrapper).length).toBe(1)
  })

  it('用户榜表格：排名 / 用户 ID / 用量 / 占比 / 原始上下行 / 套餐类型', async () => {
    const wrapper = await mountDashboard()
    const rows = userRowsOf(wrapper)

    expect(rows.length).toBe(2)
    // 排名
    expect(rows[0].find('.dsh-ut-seq').text()).toBe('1')
    // 用户列只展示 #id（不再展示邮箱）
    expect(rows[0].find('.dsh-ut-uid').text()).toBe('#1')
    expect(rows[0].find('.dsh-ut-email').exists()).toBe(false)
    // 套餐用量（计费口径 weighted）
    expect(rows[0].find('.dsh-ut-strong').text()).toBe('3.0 GB')
    // 原始上下行（不乘倍率）
    expect(rows[0].text()).toContain('↑ 1.0 GB')
    expect(rows[0].text()).toContain('↓ 2.0 GB')
    // 占比：榜内最大值 3.0 GB → 100%；第二行 1.0/3.0 = 33.3%
    expect(rows[0].text()).toContain('100%')
    expect(rows[1].text()).toContain('33.3%')
    // 套餐类型
    expect(rows[0].find('.dsh-ut-plan').text()).toBe('周期·1 个月')
    expect(rows[1].find('.dsh-ut-plan').text()).toBe('总量')
    // 明细入口
    expect(rows[0].find('.dsh-ut-toggle').text()).toContain('明细')
  })

  it('标出数据起始日（24h 当天可用，7d/30d 要等满）', async () => {
    const wrapper = await mountDashboard()

    expect(wrapper.find('.dsh-ut-start').text()).toContain('数据自 2026-10-17 起开始累积')
    expect(wrapper.find('.dsh-ut-start').text()).toContain('需等满')
  })

  it('点用户 → 展开按机器明细（百分比 + 倍率）；再点收起', async () => {
    const wrapper = await mountDashboard()

    expect(wrapper.find('.dsh-ut-detail').exists()).toBe(false)
    expect(wrapper.vm.expandedUserId).toBeNull()

    await userRowsOf(wrapper)[0].trigger('click')
    await flushPromises()

    expect(adminApi.userTrafficNodes).toHaveBeenCalledWith(1, '24h')
    expect(wrapper.vm.expandedUserId).toBe(1)
    const detail = wrapper.find('.dsh-ut-detail')
    expect(detail.exists()).toBe(true)
    // 机器名带倍率标注（设计稿：JP1（1.5×）），加权降序
    expect(detail.findAll('.dsh-ut-node-name').map((n) => n.text())).toEqual(['HK01（1.5×）', 'JP01（2×）'])
    // 用量 + 占该用户的百分比（后端 share，不按榜内最大值算）
    expect(detail.findAll('.dsh-ut-node-value').map((n) => n.text())).toEqual(['2.0 GB', '1.0 GB'])
    expect(detail.findAll('.dsh-ut-node-share').map((n) => n.text())).toEqual(['66.7%', '33.3%'])

    // 再点同一行 → 收起
    await userRowsOf(wrapper)[0].trigger('click')
    await flushPromises()
    expect(wrapper.vm.expandedUserId).toBeNull()
    expect(wrapper.find('.dsh-ut-detail').exists()).toBe(false)
  })

  it('「查看全部」弹窗：全部机器按双向降序，倍率从 node-ranking 借（替代原机器卡）', async () => {
    const wrapper = await mountDashboard()
    wrapper.vm.allNodesOpen = true
    await flushPromises()

    // summary.nodes 是全部 enabled 节点（含窗口内无采样的那台），不是 Top 5 截断
    expect(wrapper.vm.allNodes.map((n) => n.name)).toEqual(['HK01', 'JP01', 'US01'])
    // 倍率来自 /traffic/node-ranking 的 rows（summary 本身不含倍率）
    expect(wrapper.vm.nodeMultiplierText(1)).toBe('1.5×')
    expect(wrapper.vm.nodeMultiplierText(2)).toBe('2×')
    // 没有倍率数据的机器不显示倍率标签（不是 0×）
    expect(wrapper.vm.nodeMultiplierText(3)).toBeNull()
    // 进度条分母是全部机器的最大值（2 GB），不是 Top5 的
    expect(wrapper.vm.maxAllTotal).toBe(2 * 1024 ** 3)
  })

  it('「查看全部」弹窗：窗口内没有采样的机器照样列出并标「无数据」', async () => {
    const idle = {
      window: '24h', start: '', end: '', data_start: '2026-10-17',
      rows: [{
        node_id: 9, name: 'MYKL1', weighted: null, up: null, down: null, user_raw: null,
        multiplier: 3, nic_up: 30 * 1024 ** 3, nic_down: 20 * 1024 ** 3, nic_total: 50 * 1024 ** 3,
      }],
    }
    const wrapper = await mountDashboard({ nodeTrafficRanking: vi.fn(() => Promise.resolve(idle)) })
    wrapper.vm.allNodesOpen = true
    await flushPromises()

    // 倍率仍能从 node-ranking 借到（即使这台机器在 summary 里查无数据）
    expect(wrapper.vm.nodeMultiplierText(9)).toBe('3×')
    // 无采样的那台排在最后，且 total 为 null（不是 0）
    expect(wrapper.vm.allNodes.at(-1).total).toBeNull()
  })

  it('切换窗口：两个排行榜都重取，展开中的明细按新窗口刷新', async () => {
    const wrapper = await mountDashboard()
    await userRowsOf(wrapper)[0].trigger('click')
    await flushPromises()

    wrapper.vm.userWindow = '7d'
    await flushPromises()

    expect(adminApi.userTrafficRanking).toHaveBeenLastCalledWith('7d')
    expect(adminApi.nodeTrafficRanking).toHaveBeenLastCalledWith('7d')
    // 7d 里用户 1 不在榜上 → 收起，不再留着一个空展开
    expect(wrapper.vm.expandedUserId).toBeNull()
    expect(wrapper.find('.dsh-ut-detail').exists()).toBe(false)
    // 标题跟着窗口走
    expect(wrapper.text()).toContain('用户流量排行 · 近 7 天')
  })

  it('「查看全部」弹窗：窗口内一台机器都没有采样时给空提示（不画 0）', async () => {
    const wrapper = await mountDashboard({
      monitorSummary: vi.fn(() => Promise.resolve({ totals: {}, nodes: [] })),
    })
    wrapper.vm.allNodesOpen = true
    await flushPromises()

    expect(wrapper.vm.allNodes.length).toBe(0)
  })

  it('新表没有任何数据：提示从上线当天累积，不列出 0 行', async () => {
    const empty = { window: '24h', start: '', end: '', data_start: null, rows: [] }
    const wrapper = await mountDashboard({
      userTrafficRanking: vi.fn(() => Promise.resolve(empty)),
      nodeTrafficRanking: vi.fn(() => Promise.resolve({ ...empty })),
    })

    expect(wrapper.text()).toContain('暂无用户流量数据')
    expect(wrapper.text()).toContain('用户流量从上线当天开始累积')
    expect(userRowsOf(wrapper).length).toBe(0)
  })

  it('用户流量接口挂了：横幅 + 重试，且不影响机器监控区块', async () => {
    const wrapper = await mountDashboard()
    expect(wrapper.findAll('.dsh-banner').length).toBe(0)

    adminApi.userTrafficRanking = vi.fn(() => Promise.reject({ message: 'boom' }))
    adminApi.nodeTrafficRanking = vi.fn(() => Promise.reject({ message: 'boom' }))
    await wrapper.vm.loadUserTraffic()
    await flushPromises()

    const banner = wrapper.find('.dsh-ut .dsh-banner')
    expect(banner.exists()).toBe(true)
    expect(banner.text()).toContain('用户流量暂时取不到')
    // 上一次的数据还在（不清成 0 / 空）—— 用户列现在展示 #id
    expect(wrapper.text()).toContain('#1')
    // 机器监控那块照旧（它自己有数据）
    expect(wrapper.find('.dsh-total').text()).toContain('总上传')
  })
})

/**
 * 用户流量 CSV（设计稿：导出 CSV）。
 *
 * 与界面不同：导出写**完整邮箱**（台账用途，管理员本就能在用户列表看到），
 * 打码只是展示层的选择。缺失值仍是空字段（不是 0，也不是「—」）。
 */
describe('Dashboard.vue 用户流量 CSV', () => {
  it('用户榜 CSV：表头 + 与榜单同序，完整邮箱，占比为数字', async () => {
    const wrapper = await mountDashboard()
    const lines = wrapper.vm.buildUserTrafficCsv().split('\n')

    expect(lines[0]).toBe('排名,用户ID,邮箱,套餐用量(字节),占比(%),上行原始(字节),下行原始(字节),套餐类型,套餐名,套餐月数')
    expect(lines.length).toBe(3)
    expect(lines[1]).toBe(`1,1,alice@example.com,${3 * 1024 ** 3},100,${1024 ** 3},${2 * 1024 ** 3},period,月付,1`)
    // 第二名 1/3 → 33.3%（保留 1 位）；总量套餐没有月数 → 空字段
    expect(lines[2]).toBe(`2,2,bob@example.com,${1024 ** 3},33.3,${512 * 1024 ** 2},${512 * 1024 ** 2},total,年付,`)
  })

  it('弹窗 CSV：导出全部用户（不是卡片里的 Top 5），缺数据是空字段', async () => {
    const wrapper = await mountDashboard()
    await wrapper.vm.openAllUsers()
    await flushPromises()

    // 弹窗按需拉全量（limit 500，后端 MAX_LIMIT）
    expect(adminApi.userTrafficRanking).toHaveBeenLastCalledWith('24h', 500)
    expect(wrapper.vm.allUsers.length).toBe(2)

    const lines = wrapper.vm
      .buildUserTrafficCsv(wrapper.vm.allUsers, wrapper.vm.maxAllUserWeighted)
      .split('\n')

    expect(lines[0]).toBe('排名,用户ID,邮箱,套餐用量(字节),占比(%),上行原始(字节),下行原始(字节),套餐类型,套餐名,套餐月数')
    expect(lines.length).toBe(3)
    expect(lines[1]).toContain('alice@example.com')
    expect(lines[1]).not.toContain('—')
  })

  it('CSV 文件名带窗口与日期；下载走 Blob + 触发点击', async () => {
    const wrapper = await mountDashboard()

    expect(wrapper.vm.userTrafficCsvFilename()).toMatch(/^user-traffic-24h-\d{4}-\d{2}-\d{2}\.csv$/)

    const createUrl = vi.fn(() => 'blob:mock')
    const revokeUrl = vi.fn()
    global.URL.createObjectURL = createUrl
    global.URL.revokeObjectURL = revokeUrl
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    // 上一个 CSV 用例也挂过同一个 prototype spy，计数要清零后再断言本次
    click.mockClear()

    wrapper.vm.exportUserTrafficCsv()
    await flushPromises()

    expect(createUrl).toHaveBeenCalledTimes(1)
    expect(click).toHaveBeenCalledTimes(1)
    expect(revokeUrl).toHaveBeenCalledTimes(1)
  })
})
