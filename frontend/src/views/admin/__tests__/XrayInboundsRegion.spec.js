import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 入站归属分组（#入站级分组）的前后端联动。
 *
 * 守的是四件事：
 *  1. 表单里有「归属分组」下拉，默认「跟随节点」（提交 region_id: null）；
 *  2. 选了地区 → 提交该地区的数字 id（不是字符串）；
 *  3. 编辑一个「跟随节点」的入站 → 回显「跟随节点」，保存仍提交 null；
 *  4. 编辑一个已归属地区的入站 → 回显地区名。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))
vi.mock('@/components/admin/XrayNodePicker.vue', () => ({
  default: {
    name: 'XrayNodePicker',
    props: ['modelValue'],
    emits: ['update:modelValue'],
    mounted() {
      this.$emit('update:modelValue', 1)
    },
    template: '<div class="picker-stub"></div>',
  },
}))

import XrayInbounds from '@/views/admin/XrayInbounds.vue'

const REGIONS = [
  { id: 1, name: '香港' },
  { id: 2, name: '日本' },
]

const INBOUND_HK = {
  id: 9,
  tag: 'vless-443',
  remark: '',
  protocol: 'vless',
  port: 443,
  listen: '0.0.0.0',
  network: 'tcp',
  security: 'none',
  enabled: true,
  region_id: 1,
  region_name: '香港',
}
const INBOUND_FOLLOW = {
  ...INBOUND_HK,
  id: 10,
  tag: 'vmess-80',
  port: 80,
  protocol: 'vmess',
  region_id: null,
  region_name: null,
}

const mounted = []
beforeEach(() => {
  vi.clearAllMocks()
  adminApi.listXrayInbounds = vi.fn(() =>
    Promise.resolve({ inbounds: [INBOUND_HK, INBOUND_FOLLOW], config_version: 1 }),
  )
  adminApi.listRegions = vi.fn(() => Promise.resolve({ regions: REGIONS }))
  adminApi.createXrayInbound = vi.fn(() => Promise.resolve({ inbound: { id: 11 }, config_version: 2 }))
  adminApi.updateXrayInbound = vi.fn(() => Promise.resolve({ inbound: INBOUND_FOLLOW, config_version: 3 }))
})

afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
  document.body.innerHTML = ''
  vi.unstubAllGlobals()
})

const mountPage = async () => {
  const wrapper = mount(XrayInbounds, { attachTo: document.body })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

/** 打开「新建入站」弹窗（表单默认 vless/reality/443）。 */
const openCreate = async (wrapper) => {
  await wrapper.findAll('button').find((b) => b.text().includes('新建入站')).trigger('click')
  await flushPromises()
}

/** 点表单弹窗底部按钮（创建 / 保存）。 */
const submit = async (label) => {
  const btn = [...document.body.querySelectorAll('.ch-modal-footer button')]
    .find((b) => b.textContent.trim() === label)
  expect(btn, `弹窗里应有「${label}」按钮`).toBeTruthy()
  btn.click()
  await flushPromises()
}

/** 表单里的「归属分组」下拉容器（Select 把选项渲染进 Teleport 到 body 的浮层）。 */
const regionField = () =>
  [...document.body.querySelectorAll('.an-field')].find(
    (l) => l.querySelector('span')?.textContent.trim() === '归属分组',
  )

/** 打开归属分组下拉，返回浮层里的选项文案。 */
const openRegionSelect = async () => {
  const field = regionField()
  expect(field, '表单里应有「归属分组」下拉').toBeTruthy()
  field.querySelector('.ch-select-trigger').click()
  await flushPromises()
  return [...document.body.querySelectorAll('.ch-select-option')].map((o) => o.textContent.trim())
}

const chooseRegion = async (label) => {
  await openRegionSelect()
  const opt = [...document.body.querySelectorAll('.ch-select-option')].find(
    (o) => o.textContent.trim() === label,
  )
  expect(opt, `下拉里应有「${label}」`).toBeTruthy()
  opt.click()
  await flushPromises()
}

describe('XrayInbounds.vue — 归属分组（#入站级分组）', () => {
  it('新建时不选地区 → 提交 region_id: null（跟随节点）', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)
    await submit('创建')

    expect(adminApi.createXrayInbound).toHaveBeenCalledTimes(1)
    const payload = adminApi.createXrayInbound.mock.calls[0][1]
    expect(payload.region_id).toBeNull()
  })

  it('新建时选了地区 → 提交该地区的数字 id', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)
    await chooseRegion('日本')
    await submit('创建')

    const payload = adminApi.createXrayInbound.mock.calls[0][1]
    expect(payload.region_id).toBe(2)
  })

  it('下拉里列出地区字典（跟随节点 + 各地区）', async () => {
    const wrapper = await mountPage()
    await openCreate(wrapper)

    expect(adminApi.listRegions).toHaveBeenCalled()
    const labels = await openRegionSelect()
    expect(labels).toContain('跟随节点（默认）')
    expect(labels).toContain('香港')
    expect(labels).toContain('日本')
  })

  it('编辑「跟随节点」的入站 → 回显跟随节点，保存提交 null', async () => {
    const wrapper = await mountPage()
    const row = wrapper.findAll('tbody tr').find((tr) => tr.text().includes('vmess-80'))
    expect(row).toBeTruthy()
    // 操作列：编辑按钮在删除（trash）之前
    const buttons = row.findAll('button')
    await buttons[buttons.length - 2].trigger('click')
    await flushPromises()

    const field = regionField()
    expect(field).toBeTruthy()
    expect(field.textContent).toContain('跟随节点')

    await submit('保存')
    expect(adminApi.updateXrayInbound).toHaveBeenCalledTimes(1)
    const payload = adminApi.updateXrayInbound.mock.calls[0][2]
    expect(payload.region_id).toBeNull()
  })

  it('编辑已归属地区的入站 → 回显地区名，保存提交该地区 id', async () => {
    const wrapper = await mountPage()
    const row = wrapper.findAll('tbody tr').find((tr) => tr.text().includes('vless-443'))
    const buttons = row.findAll('button')
    await buttons[buttons.length - 2].trigger('click')
    await flushPromises()

    const field = regionField()
    expect(field.textContent).toContain('香港')

    await submit('保存')
    const payload = adminApi.updateXrayInbound.mock.calls[0][2]
    expect(payload.region_id).toBe(1)
  })
})
