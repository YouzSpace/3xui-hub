import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 操作审计页：系统事件的二级筛选 + 漂移/写回失败的醒目标记 + error 折叠展开。
 *
 * 后端只有 type / status / search 三个参数（server/internal/handler/admin/system.go:237），
 * 所以「对账漂移 / 3x-ui 写回失败」这两个二级筛选用的是 search（后端按 action LIKE 匹配，
 * 分页与总数仍是后端算的）；「其他系统事件」是排除式，只能在本页剔除。
 * 这里盯的就是这条映射别写反、别悄悄丢掉分页。
 */

const { adminApi } = vi.hoisted(() => ({ adminApi: {} }))
vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}))

import AuditLogs from '@/views/admin/AuditLogs.vue'

// 故意写长：> 80 字，用来验默认截断与「展开」看全文
const LONG_ERR =
  '节点 totalGB=10GB 与面板 limit=50GB 不一致，expiryTime 也比面板早 12 天；' +
  '用户会被节点提前掐断，需要人工确认后修正节点侧配置（节点 hk01，用户 u@example.com）。'

const LOGS = [
  {
    id: 4, type: 'system', actor_name: 'reconcile', action: '对账漂移·会提前掐人',
    method: null, path: 'node#1/user#9', status: 500, error: LONG_ERR, ip: '10.0.0.1',
    created_at: '2026-11-07T02:00:00.000000Z',
  },
  {
    id: 3, type: 'system', actor_name: 'reconcile', action: '对账漂移·节点更宽松',
    method: null, path: 'node#1/user#10', status: 500, error: '节点限额比面板更宽', ip: '10.0.0.1',
    created_at: '2026-11-07T01:30:00.000000Z',
  },
  {
    id: 2, type: 'system', actor_name: null, action: '3x-ui 写回失败',
    method: null, path: 'node#1/user#11', status: 500, error: 'Post "http://10.0.0.9:2053/panel/api/inbounds/updateClient/1": i/o timeout', ip: '10.0.0.1',
    created_at: '2026-11-07T01:00:00.000000Z',
  },
  {
    id: 1, type: 'system', actor_name: 'panel', action: 'agent 升级',
    method: 'POST', path: '/admin-api/nodes/1/upgrade', status: 200, error: null, ip: '10.0.0.2',
    created_at: '2026-11-07T00:00:00.000000Z',
  },
]

beforeEach(() => {
  vi.clearAllMocks()
  // 模拟后端 auditLogs：search 只按 action 做 LIKE（system_sqlstore.go OperationLogPage）
  adminApi.auditLogs = vi.fn((params = {}) => {
    const list = params.search
      ? LOGS.filter((l) => String(l.action).includes(params.search))
      : LOGS.slice()
    return Promise.resolve(
      Object.assign(list, {
        pagination: { current_page: 1, last_page: 1, total: list.length, per_page: 50 },
      }),
    )
  })
})

const mountPage = async () => {
  const wrapper = mount(AuditLogs, { attachTo: document.body })
  await flushPromises()
  return wrapper
}

const lastParams = () => adminApi.auditLogs.mock.calls.at(-1)[0]

const rowOf = (wrapper, text) => wrapper.findAll('tbody tr').find((tr) => tr.text().includes(text))

describe('AuditLogs.vue — 系统事件的二级筛选', () => {
  it('默认不出现二级筛选；类型选「系统」后才出现，且带 type=system 请求', async () => {
    const wrapper = await mountPage()
    expect(wrapper.find('.al-sel-action').exists()).toBe(false)

    wrapper.vm.typeFilter = 'system'
    await flushPromises()

    expect(wrapper.find('.al-sel-action').exists()).toBe(true)
    expect(lastParams()).toMatchObject({ type: 'system' })
    // 没选二级筛选时不能自作主张加 search
    expect(lastParams().search).toBeUndefined()
  })

  it('只有「其他系统事件」会出排除式说明，漂移/写回失败（后端过滤）不出', async () => {
    const wrapper = await mountPage()
    wrapper.vm.typeFilter = 'system'
    await flushPromises()
    expect(wrapper.find('.al-note').exists()).toBe(false)

    wrapper.vm.actionFilter = 'drift'
    await flushPromises()
    expect(wrapper.find('.al-note').exists()).toBe(false)

    wrapper.vm.actionFilter = 'other'
    await flushPromises()
    expect(wrapper.find('.al-note').exists()).toBe(true)
  })

  it('选「对账漂移」→ 用 search 传 action 字面量（后端 LIKE 命中 action），搜索框同时停用', async () => {
    const wrapper = await mountPage()
    wrapper.vm.typeFilter = 'system'
    await flushPromises()

    wrapper.vm.actionFilter = 'drift'
    await flushPromises()

    expect(lastParams()).toMatchObject({ type: 'system', search: '对账漂移' })
    expect(wrapper.find('.al-search input').attributes('disabled')).toBeDefined()
    // 结果只剩漂移行
    expect(wrapper.findAll('tbody tr')).toHaveLength(2)

    // 换成写回失败：字面量跟着换
    wrapper.vm.actionFilter = 'writeback'
    await flushPromises()
    expect(lastParams()).toMatchObject({ type: 'system', search: '3x-ui 写回失败' })
    expect(wrapper.findAll('tbody tr')).toHaveLength(1)
    expect(wrapper.find('tbody tr').text()).toContain('3x-ui 写回失败')
  })

  it('选「其他系统事件」→ 不带 search，在本页剔掉漂移与写回失败', async () => {
    const wrapper = await mountPage()
    wrapper.vm.typeFilter = 'system'
    await flushPromises()

    wrapper.vm.actionFilter = 'other'
    await flushPromises()

    expect(lastParams().search).toBeUndefined()
    expect(wrapper.find('.al-search input').attributes('disabled')).toBeUndefined()
    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(1)
    expect(rows[0].text()).toContain('agent 升级')
    // 排除式只能本页剔除：必须讲清楚，别让人把「共 N 条」当过滤后的总数
    expect(wrapper.find('.al-note').text()).toContain('只从当前页里剔掉')
  })

  it('离开「系统」类型时二级筛选自动清空，不把 action 带进别的类型', async () => {
    const wrapper = await mountPage()
    wrapper.vm.typeFilter = 'system'
    await flushPromises()
    wrapper.vm.actionFilter = 'drift'
    await flushPromises()

    wrapper.vm.typeFilter = 'admin'
    await flushPromises()

    expect(wrapper.vm.actionFilter).toBe('')
    expect(wrapper.find('.al-sel-action').exists()).toBe(false)
    expect(lastParams()).toMatchObject({ type: 'admin' })
    expect(lastParams().search).toBeUndefined()
  })
})

describe('AuditLogs.vue — 漂移行标记与 error 折叠', () => {
  it('会提前掐人=红、节点更宽松=黄，并带方向角标；普通系统事件不标记', async () => {
    const wrapper = await mountPage()

    const early = rowOf(wrapper, '对账漂移·会提前掐人')
    expect(early.classes()).toContain('al-row-danger')
    expect(early.text()).toContain('会提前掐人')

    const loose = rowOf(wrapper, '对账漂移·节点更宽松')
    expect(loose.classes()).toContain('al-row-warning')

    const writeback = rowOf(wrapper, '3x-ui 写回失败')
    expect(writeback.classes()).toContain('al-row-danger')

    const plain = rowOf(wrapper, 'agent 升级')
    expect(plain.classes()).not.toContain('al-row-danger')
    expect(plain.classes()).not.toContain('al-row-warning')
  })

  it('error 默认截断到 80 字，点「展开」看全文，再点「收起」', async () => {
    const wrapper = await mountPage()
    const cell = rowOf(wrapper, '对账漂移·会提前掐人').find('.al-err-cell')

    expect(cell.find('.ex-text').text()).toBe(`${LONG_ERR.slice(0, 80)}…`)
    expect(cell.find('.ex-btn').text()).toBe('展开')

    await cell.find('.ex-btn').trigger('click')
    expect(cell.find('.ex-text').text()).toBe(LONG_ERR)
    expect(cell.find('.ex-btn').text()).toBe('收起')

    await cell.find('.ex-btn').trigger('click')
    expect(cell.find('.ex-text').text()).toBe(`${LONG_ERR.slice(0, 80)}…`)
  })

  it('短报错不截断、不给展开按钮；没有报错显示占位符', async () => {
    const wrapper = await mountPage()

    const shortCell = rowOf(wrapper, '对账漂移·节点更宽松').find('.al-err-cell')
    expect(shortCell.find('.ex-text').text()).toBe('节点限额比面板更宽')
    expect(shortCell.find('.ex-btn').exists()).toBe(false)

    const emptyCell = rowOf(wrapper, 'agent 升级').find('.al-err-cell')
    expect(emptyCell.text()).toBe('—')
  })
})
