import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 静态 IP 出口分类页（ExitProfiles.vue）。
 *
 * 守四件事（都是只会在前端炸 / 只会在前端悄悄改口径的地方）：
 *  1. 列表列口径：账号名优先用后端 provider_name，缺了按 provider_id 现查；节点数用 node_count；
 *  2. 新建弹窗：前置节点多选来自 /nodes?all=1，勾了几台就传几个 id；
 *  3. 提交 payload：数字字段转成数字、socks 默认 p.webshare.io:1080、enabled 传布尔；
 *  4. 删除被拒（后端说「还有套餐绑着」）时，msg 必须原样弹出来，不能被吞成「删除失败」。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import ExitProfiles from '@/views/admin/ExitProfiles.vue'

const PROVIDERS = [
  { id: 11, name: '主力账号', driver: 'webshare', enabled: true, last_status: 'success' },
  { id: 12, name: '备用账号', driver: 'webshare', enabled: true, last_status: 'failed' },
]

const NODES = [
  { id: 101, name: '香港-01', status: 'online' },
  { id: 102, name: '美国-01', status: 'offline' },
]

const PROFILES = [
  {
    id: 1,
    name: '香港住宅',
    provider_id: 11,
    provider_name: '主力账号',
    traffic_gb: 5,
    duration_days: 30,
    max_thread_count: 50,
    socks_host: 'p.webshare.io',
    socks_port: 1080,
    enabled: true,
    node_ids: [101],
    node_count: 1,
  },
  {
    // provider_name 缺失（后端老版本）→ 前端按 provider_id 现查
    id: 2,
    name: '美国住宅',
    provider_id: 12,
    traffic_gb: 10,
    duration_days: 7,
    max_thread_count: 20,
    socks_host: 'p.webshare.io',
    socks_port: 1080,
    enabled: false,
    node_ids: [101, 102],
    node_count: 2,
  },
]

beforeEach(() => {
  vi.clearAllMocks()
  adminApi.listExitProfiles = vi.fn(() => Promise.resolve(PROFILES))
  adminApi.listWebshareProviders = vi.fn(() => Promise.resolve({ items: PROVIDERS }))
  adminApi.listNodes = vi.fn(() => Promise.resolve(NODES))
  adminApi.createExitProfile = vi.fn(() => Promise.resolve({ id: 3 }))
  adminApi.updateExitProfile = vi.fn(() => Promise.resolve({}))
  adminApi.deleteExitProfile = vi.fn(() => Promise.resolve({}))
  vi.stubGlobal('confirm', vi.fn(() => true))
})

const mounted = []
afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
  document.body.innerHTML = ''
  vi.unstubAllGlobals()
})

const mountPage = async () => {
  const wrapper = mount(ExitProfiles, { attachTo: document.body })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

const rowOf = (wrapper, name) => wrapper.findAll('tbody tr').find((tr) => tr.text().includes(name))

/** 弹窗里按字段标题找 input（.ep-field 里的第一个 input） */
const fieldInput = (label) => {
  const field = [...document.querySelectorAll('.ep-form .ep-field')].find((f) =>
    f.querySelector('span')?.textContent.includes(label),
  )
  return field?.querySelector('input') || null
}

const setInput = async (label, value) => {
  const el = fieldInput(label)
  expect(el, `弹窗里没找到「${label}」输入框`).toBeTruthy()
  el.value = String(value)
  el.dispatchEvent(new Event('input'))
  await flushPromises()
}

const nodeBoxes = () => [...document.querySelectorAll('.ep-node-list input[type="checkbox"]')]

/** 每次点击后等一次 patch：v-model 的 checkbox 读的是上一次渲染缓存的数组 */
const toggleNode = async (idx) => {
  nodeBoxes()[idx].click()
  await flushPromises()
}

/** Select 是自定义下拉：先点触发器，再从 body 上已 Teleport 的面板里挑一项 */
const chooseProvider = async (text) => {
  const trigger = document.querySelector('.ep-form .ch-select-trigger')
  expect(trigger, '弹窗里没找到账号下拉').toBeTruthy()
  trigger.click()
  await flushPromises()
  const opt = [...document.querySelectorAll('.ch-select-option')].find((o) =>
    o.textContent.includes(text),
  )
  expect(opt, `下拉里没找到「${text}」`).toBeTruthy()
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

const openCreate = async (wrapper) => {
  const btn = wrapper.findAll('.ep-head button').find((b) => b.text().includes('新建出口分类'))
  await btn.trigger('click')
  await flushPromises()
}

describe('ExitProfiles.vue — 列表', () => {
  it('渲染名称 / 账号 / 节点数 / 流量时长 / 状态', async () => {
    const wrapper = await mountPage()

    const row = rowOf(wrapper, '香港住宅')
    const cells = row.findAll('td')
    expect(cells[0].text()).toBe('香港住宅')
    // 账号名 + 账号连通状态（last_status=success → 正常）
    expect(cells[1].text()).toContain('主力账号')
    expect(cells[1].text()).toContain('正常')
    expect(cells[2].text()).toBe('1 台')
    expect(cells[3].text()).toContain('5 GB / 30 天')
    expect(cells[3].text()).toContain('并发 50')
    expect(cells[4].text()).toBe('已启用')
  })

  it('后端没给 provider_name 时按 provider_id 现查，状态列跟着账号走', async () => {
    const wrapper = await mountPage()

    const cells = rowOf(wrapper, '美国住宅').findAll('td')
    expect(cells[1].text()).toContain('备用账号')
    // last_status=failed → 连接失败（不是笼统的「正常」）
    expect(cells[1].text()).toContain('连接失败')
    expect(cells[2].text()).toBe('2 台')
    expect(cells[4].text()).toBe('已停用')
  })

  it('接口 404 时展示加载失败 + 重试，不崩', async () => {
    adminApi.listExitProfiles = vi.fn(() => Promise.reject({ message: '404 page not found' }))
    const wrapper = await mountPage()

    expect(wrapper.text()).toContain('加载失败：404 page not found')
    expect(wrapper.text()).toContain('重试')
  })
})

describe('ExitProfiles.vue — 新建', () => {
  it('打开弹窗：账号下拉 + 前置节点多选（来自 /nodes?all=1）都在', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)

    expect(document.body.textContent).toContain('新建出口分类')
    expect(nodeBoxes()).toHaveLength(NODES.length)
    // 节点行带状态，管理员能看出这台机器还能不能用
    expect(document.body.textContent).toContain('香港-01')
    expect(document.body.textContent).toContain('offline')

    // socks 默认值：webshare 统一入口
    expect(fieldInput('SOCKS 地址').value).toBe('p.webshare.io')
    expect(fieldInput('端口').value).toBe('1080')
  })

  it('勾 2 台前置节点 + 选账号 → 提交 payload 正确', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)

    await setInput('分类名称', '日本住宅')
    await chooseProvider('主力账号')
    await setInput('每个用户流量', '8')
    await setInput('有效期', '15')
    await setInput('并发数', '30')
    await toggleNode(0)
    await toggleNode(1)

    expect(document.body.textContent).toContain('已选 2 台')

    await clickFooterButton('保存')

    expect(adminApi.createExitProfile).toHaveBeenCalledTimes(1)
    expect(adminApi.createExitProfile.mock.calls[0][0]).toEqual({
      name: '日本住宅',
      provider_id: 11,
      traffic_gb: 8,
      duration_days: 15,
      max_thread_count: 30,
      socks_host: 'p.webshare.io',
      socks_port: 1080,
      node_ids: [101, 102],
      enabled: true,
    })
  })

  it('没选账号就不提交（弹提示，不打请求）', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)

    await setInput('分类名称', '空账号')
    await clickFooterButton('保存')

    expect(toast.error).toHaveBeenCalledWith('请选择 webshare 账号')
    expect(adminApi.createExitProfile).not.toHaveBeenCalled()
  })
})

describe('ExitProfiles.vue — 编辑 / 删除', () => {
  it('编辑弹窗按列表数据回填（含已选节点），保存走 update', async () => {
    const wrapper = await mountPage()

    await rowOf(wrapper, '美国住宅').findAll('button')[0].trigger('click')
    await flushPromises()

    expect(document.body.textContent).toContain('编辑出口分类')
    expect(fieldInput('分类名称').value).toBe('美国住宅')
    expect(document.body.textContent).toContain('已选 2 台')
    // enabled=false → 勾选框不勾
    expect(document.querySelector('.ep-check input').checked).toBe(false)

    await setInput('有效期', '3')
    await clickFooterButton('保存')

    expect(adminApi.updateExitProfile).toHaveBeenCalledTimes(1)
    const [id, payload] = adminApi.updateExitProfile.mock.calls[0]
    expect(id).toBe(2)
    expect(payload.duration_days).toBe(3)
    expect(payload.node_ids).toEqual([101, 102])
    expect(payload.enabled).toBe(false)
  })

  it('删除被拒（有套餐绑定）时原样展示后端 msg', async () => {
    adminApi.deleteExitProfile = vi.fn(() =>
      Promise.reject({ message: '该分类还被 2 个套餐使用，请先解除绑定' }),
    )
    const wrapper = await mountPage()

    // 操作列第二个按钮 = 删除
    await rowOf(wrapper, '香港住宅').findAll('button')[1].trigger('click')
    await flushPromises()

    expect(adminApi.deleteExitProfile).toHaveBeenCalledWith(1)
    expect(toast.error).toHaveBeenCalledWith('该分类还被 2 个套餐使用，请先解除绑定')
  })
})
