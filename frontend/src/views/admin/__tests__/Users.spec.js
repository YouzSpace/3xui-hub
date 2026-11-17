import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 用户列表页 × 绑定节点（地区功能 #26）。
 *
 * 后端已落地三个东西，「绑定节点」这块才有意义：
 *   1. 列表每条带 node_ids / node_names（一次批量取，不是 N+1）；
 *   2. ?node_id= 按绑定节点筛用户；
 *   3. PUT /users/:id/nodes 单用户改绑。
 *
 * 这里钉的是渲染与请求两侧的对齐：
 *   - 列：有 node_names 就拼名字，字段缺失显示「—」，不把 undefined 印在页面上；
 *   - 筛选：选项来自 /users/nodes-options（含「全部节点」+ 地区名），选中后请求带 node_id；
 *   - 改绑：弹窗当前节点只读、目标节点排除已绑定，提交 {from_node_id, to_node_id}。
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

import Users from '@/views/admin/Users.vue'

const USERS = [
  { id: 1, email: 'a@example.com', plan_name: '月付', plan_type: 'period', enabled: true, node_ids: [1], node_names: ['hk-01'] },
  { id: 2, email: 'b@example.com', plan_name: '月付', plan_type: 'period', enabled: true, node_ids: [1, 2], node_names: ['hk-01', 'hk-02'] },
  // 字段缺失（没绑定 / 老后端）：显示「—」
  { id: 3, email: 'c@example.com', plan_name: '月付', plan_type: 'period', enabled: true },
]

const NODE_OPTIONS = [
  { id: 1, name: 'hk-01', region_name: '香港' },
  { id: 2, name: 'hk-02', region_name: '香港' },
]

beforeEach(() => {
  vi.clearAllMocks()
  adminApi.listUsers = vi.fn(() =>
    Promise.resolve(Object.assign(USERS.slice(), {
      pagination: { current_page: 1, last_page: 1, total: USERS.length, per_page: 50 },
    })),
  )
  adminApi.listPlans = vi.fn(() => Promise.resolve([]))
  adminApi.listUserNodeOptions = vi.fn(() => Promise.resolve(NODE_OPTIONS))
  adminApi.changeUserNodes = vi.fn(() => Promise.resolve({ ok: true }))
})

const mounted = []
afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
})

const mountPage = async () => {
  const wrapper = mount(Users, { attachTo: document.body })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

const rowOf = (wrapper, email) => wrapper.findAll('tbody tr').find((tr) => tr.text().includes(email))

// Select 的选项面板 Teleport 到 body，选项只能从 document 里找。
// 参数必须是真 DOM 元素（DOMWrapper 走 .trigger，原生元素走 dispatchEvent）。
const openPanel = async (triggerEl) => {
  triggerEl.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await flushPromises()
  return [...document.querySelectorAll('.ch-select-panel .ch-select-option')]
}

const pickOption = async (triggerEl, label) => {
  const opts = await openPanel(triggerEl)
  const hit = opts.find((b) => b.textContent.trim().startsWith(label))
  expect(hit, `下拉里没有「${label}」：${opts.map((o) => o.textContent.trim())}`).toBeTruthy()
  hit.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await flushPromises()
}

const lastListParams = () => adminApi.listUsers.mock.calls.at(-1)[0]

describe('Users.vue — 绑定节点列', () => {
  it('表头有「绑定节点」列；有 node_names 时按名字拼接', async () => {
    const wrapper = await mountPage()

    const heads = wrapper.findAll('thead th').map((th) => th.text())
    expect(heads).toContain('绑定节点')

    expect(rowOf(wrapper, 'a@example.com').find('.au-bound-nodes').text()).toBe('hk-01')
    expect(rowOf(wrapper, 'b@example.com').find('.au-bound-nodes').text()).toBe('hk-01、hk-02')
  })

  it('后端字段缺失时显示「—」，不显示 undefined / 空串', async () => {
    const wrapper = await mountPage()
    const cell = rowOf(wrapper, 'c@example.com').find('.au-bound-nodes')
    expect(cell.text()).toBe('—')
    expect(cell.text()).not.toContain('undefined')
  })
})

describe('Users.vue — 绑定节点筛选', () => {
  it('下拉候选来自 /users/nodes-options：含「全部节点」和「节点名（地区）」', async () => {
    const wrapper = await mountPage()
    expect(adminApi.listUserNodeOptions).toHaveBeenCalled()

    const filter = wrapper.find('.au-node-filter')
    expect(filter.exists()).toBe(true)
    // 接口就绪了，不能再是 disabled 占位
    expect(filter.find('button').attributes('disabled')).toBeUndefined()

    const labels = (await openPanel(filter.find('button').element)).map((o) => o.textContent.trim())
    expect(labels).toEqual(['全部节点', 'hk-01（香港）', 'hk-02（香港）'])
  })

  it('选中节点后列表请求带 node_id；换回「全部节点」不再带', async () => {
    const wrapper = await mountPage()
    // 首屏：不带 node_id
    expect(lastListParams()).not.toHaveProperty('node_id')

    const filterTrigger = () => wrapper.find('.au-node-filter button').element
    await pickOption(filterTrigger(), 'hk-02')
    expect(lastListParams()).toMatchObject({ node_id: 2 })
    expect(adminApi.listUsers).toHaveBeenCalledTimes(2)

    await pickOption(filterTrigger(), '全部节点')
    expect(lastListParams()).not.toHaveProperty('node_id')
  })
})

describe('Users.vue — 改绑弹窗', () => {
  const openRebindFor = async (wrapper, email) => {
    const btn = rowOf(wrapper, email).find('button[title="改绑节点"]')
    expect(btn.exists(), `${email} 那行没有改绑按钮`).toBe(true)
    await btn.trigger('click')
    await flushPromises()
  }

  // 弹窗 Teleport 到 body：标题/字段都不在 wrapper 里
  const modalText = () => document.querySelector('.ch-modal-card')?.textContent || ''
  const confirmButton = () => [...document.querySelectorAll('.ch-modal-footer button')]
    .find((b) => b.textContent.trim() === '确认改绑')

  it('点「改绑」打开弹窗：当前节点只读展示，目标节点排除已绑定的', async () => {
    const wrapper = await mountPage()
    await openRebindFor(wrapper, 'a@example.com')

    expect(wrapper.vm.rebindOpen).toBe(true)
    expect(modalText()).toContain('改绑节点')
    expect(modalText()).toContain('a@example.com')
    expect(modalText()).toContain('hk-01')

    const trigger = document.querySelector('.ch-modal-card .ch-select-trigger')
    expect(trigger).toBeTruthy()
    const labels = (await openPanel(trigger)).map((o) => o.textContent.trim())
    // 已经绑在 hk-01 上，目标节点里只剩 hk-02
    expect(labels).toEqual(['hk-02（香港）'])
  })

  it('提交 payload 正确：绑 1 台的用户走 from=当前节点 → to=目标节点', async () => {
    const wrapper = await mountPage()
    await openRebindFor(wrapper, 'a@example.com')

    await pickOption(document.querySelector('.ch-modal-card .ch-select-trigger'), 'hk-02')

    const confirm = confirmButton()
    expect(confirm).toBeTruthy()
    confirm.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await flushPromises()

    expect(adminApi.changeUserNodes).toHaveBeenCalledWith(1, { from_node_id: 1, to_node_id: 2 })
    expect(toast.success).toHaveBeenCalledWith('已改绑')
    // 改完重新拉列表（绑定列要跟着变）
    expect(adminApi.listUsers).toHaveBeenCalledTimes(2)
  })

  it('没绑定节点的用户提交时 from_node_id 为 null（只加不减）', async () => {
    const wrapper = await mountPage()
    await openRebindFor(wrapper, 'c@example.com')

    await pickOption(document.querySelector('.ch-modal-card .ch-select-trigger'), 'hk-01')

    const confirm = confirmButton()
    expect(confirm).toBeTruthy()
    confirm.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await flushPromises()

    expect(adminApi.changeUserNodes).toHaveBeenCalledWith(3, { from_node_id: null, to_node_id: 1 })
  })
})
