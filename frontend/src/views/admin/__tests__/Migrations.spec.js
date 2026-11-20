import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 迁移历史页（Migrations.vue）。
 *
 * 后端接口 GET /admin-api/migrations 的 data 是 { items, total, page, per_page }
 * （server/internal/handler/admin/migration_domain.go:348），不是 usePagedList 认的
 * `res.pagination` —— 这里盯三件事：
 *  1. 列表字段按后端原始口径渲染（from_node_id 为 null = 首次分配）；
 *  2. 筛选条件确实进了请求参数（user_id / node_id / reason），不是只改了输入框；
 *  3. 撤销只对「batch_id 非空 + 24 小时内」开放，确认后调 undo 接口并刷新，失败弹错误。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}))

import Migrations from '@/views/admin/Migrations.vue'

// 后端 dateTimeStr 口径：UTC，无时区标记
const utc = (msAgo) =>
  new Date(Date.now() - msAgo).toISOString().replace('T', ' ').slice(0, 19)

const ROWS = [
  // 1 小时前、有批次 → 可撤销
  {
    id: 4, user_id: 12, from_node_id: 2, to_node_id: 5,
    reason: 'balance', operator: 'admin', batch_id: 'batch-fresh-001',
    created_at: utc(1 * 60 * 60 * 1000),
  },
  // 30 小时前、有批次 → 已超窗口
  {
    id: 3, user_id: 13, from_node_id: 1, to_node_id: 2,
    reason: 'failover', operator: 'admin', batch_id: 'batch-old-002',
    created_at: utc(30 * 60 * 60 * 1000),
  },
  // 无批次（非接口产生，例如手动改绑）→ 不可撤销
  {
    id: 2, user_id: 14, from_node_id: null, to_node_id: 3,
    reason: 'manual', operator: 'ops', batch_id: null,
    created_at: utc(60 * 1000),
  },
]

const NODES = [
  { id: 1, name: 'hk-1' },
  { id: 2, name: 'jp-1' },
  { id: 3, name: 'us-1' },
  { id: 5, name: 'sg-1' },
]

const pagedResponse = (items, total = items.length) => ({
  items,
  total,
  page: 1,
  per_page: 20,
})

beforeEach(() => {
  vi.clearAllMocks()
  adminApi.listMigrationsPage = vi.fn(() => Promise.resolve(pagedResponse(ROWS)))
  adminApi.listNodes = vi.fn(() => Promise.resolve(NODES))
  adminApi.undoMigration = vi.fn(() => Promise.resolve({ batch_id: 'batch-fresh-001', undone: 1, rebound: 1, unbound: 0 }))
  // useConfirm 在没有宿主时退回 window.confirm
  vi.stubGlobal('confirm', vi.fn(() => true))
})

const mounted = []
afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
  vi.unstubAllGlobals()
})

const mountPage = async () => {
  const wrapper = mount(Migrations, { attachTo: document.body })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

const lastParams = () => adminApi.listMigrationsPage.mock.calls.at(-1)[0]
const rowOf = (wrapper, text) => wrapper.findAll('tbody tr').find((tr) => tr.text().includes(text))

describe('Migrations.vue — 列表渲染', () => {
  it('按后端字段渲染时间 / 用户 / 从→到节点 / 原因 / 批次', async () => {
    const wrapper = await mountPage()

    expect(adminApi.listMigrationsPage).toHaveBeenCalled()
    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(3)

    const fresh = rowOf(wrapper, 'batch-fresh-001')
    expect(fresh).toBeTruthy()
    expect(fresh.text()).toContain('#12')
    // 节点 id → 名字；from ≠ to 中间有箭头
    expect(fresh.text()).toContain('jp-1')
    expect(fresh.text()).toContain('sg-1')
    expect(fresh.text()).toContain('→')
    expect(fresh.text()).toContain('批量均衡')
    expect(fresh.text()).not.toContain('balance')
  })

  it('from_node_id 为空显示「首次分配」；batch_id 为空提示非接口产生', async () => {
    const wrapper = await mountPage()

    const manual = rowOf(wrapper, '#14')
    expect(manual.text()).toContain('首次分配')
    expect(manual.text()).toContain('手动改绑')
    expect(manual.text()).toContain('非接口产生')
    // 没有批次号的记录不能撤销
    expect(manual.find('button').exists()).toBe(false)
  })

  it('节点接口不可用时退回 #id，不弹错误也不崩', async () => {
    adminApi.listNodes = vi.fn(() => Promise.reject({ message: '404 page not found' }))
    const wrapper = await mountPage()

    const fresh = rowOf(wrapper, 'batch-fresh-001')
    expect(fresh.text()).toContain('#2')
    expect(fresh.text()).toContain('#5')
    expect(toast.error).not.toHaveBeenCalled()
  })

  it('分页元信息从 data 里取：total 渲染到分页条', async () => {
    adminApi.listMigrationsPage = vi.fn(() =>
      Promise.resolve(pagedResponse([ROWS[0]], 41)),
    )
    const wrapper = await mountPage()

    // 41 条 / 每页 20 → 3 页
    expect(wrapper.find('.ch-pagination-info').text()).toContain('共 41 条')
    expect(wrapper.find('.ch-pagination-info').text()).toContain('第 1 / 3 页')
  })
})

describe('Migrations.vue — 筛选参数', () => {
  it('user_id / node_id / reason 进了请求参数，空值不带', async () => {
    const wrapper = await mountPage()
    // 初始请求：不带任何筛选
    expect(lastParams()).toEqual({ page: 1 })

    wrapper.vm.userFilter = '12'
    wrapper.vm.nodeFilter = '5'
    wrapper.vm.reasonFilter = 'balance'
    await flushPromises()
    // 两个输入框有 350ms 防抖，等它落地再断言
    await new Promise((r) => setTimeout(r, 400))
    await flushPromises()

    expect(lastParams()).toMatchObject({ user_id: '12', node_id: '5', reason: 'balance' })
  })

  it('只选 reason 时也立即带进请求', async () => {
    const wrapper = await mountPage()

    wrapper.vm.reasonFilter = 'failover'
    await flushPromises()

    expect(lastParams()).toMatchObject({ reason: 'failover' })
    expect(lastParams().user_id).toBeUndefined()
    expect(lastParams().node_id).toBeUndefined()
  })
})

describe('Migrations.vue — 撤销', () => {
  it('只有 24 小时内 + 有批次号的行出现撤销按钮', async () => {
    const wrapper = await mountPage()

    const fresh = rowOf(wrapper, 'batch-fresh-001')
    expect(fresh.find('button').text()).toBe('撤销')

    const old = rowOf(wrapper, 'batch-old-002')
    expect(old.find('button').exists()).toBe(false)
    expect(old.text()).toContain('已超 24 小时')
  })

  it('确认后调用 undo 接口（传批次号），成功后刷新并 toast', async () => {
    const wrapper = await mountPage()
    const callsBefore = adminApi.listMigrationsPage.mock.calls.length

    const fresh = rowOf(wrapper, 'batch-fresh-001')
    await fresh.find('button').trigger('click')
    await flushPromises()

    // 确认文案必须说清是「整批回滚」
    expect(globalThis.confirm).toHaveBeenCalled()
    expect(String(globalThis.confirm.mock.calls[0][0])).toContain('batch-fresh-001')
    expect(adminApi.undoMigration).toHaveBeenCalledWith('batch-fresh-001')
    // 成功后重拉列表
    expect(adminApi.listMigrationsPage.mock.calls.length).toBeGreaterThan(callsBefore)
    expect(toast.success).toHaveBeenCalled()
  })

  it('取消确认时什么都不调', async () => {
    vi.stubGlobal('confirm', vi.fn(() => false))
    const wrapper = await mountPage()

    await rowOf(wrapper, 'batch-fresh-001').find('button').trigger('click')
    await flushPromises()

    expect(adminApi.undoMigration).not.toHaveBeenCalled()
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('撤销失败：后端的拒绝原因原样弹 toast.error，不谎报成功', async () => {
    adminApi.undoMigration = vi.fn(() =>
      Promise.reject({ message: '已超过 24 小时撤销窗口，已拒绝。', code: 422 }),
    )
    const wrapper = await mountPage()

    await rowOf(wrapper, 'batch-fresh-001').find('button').trigger('click')
    await flushPromises()

    expect(toast.error).toHaveBeenCalledWith('已超过 24 小时撤销窗口，已拒绝。')
    expect(toast.success).not.toHaveBeenCalled()
  })
})
