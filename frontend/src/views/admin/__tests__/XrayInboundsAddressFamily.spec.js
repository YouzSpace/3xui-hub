import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 入站「地址族」（issue #88）。
 *
 * listen 一直是后端字段，但以前前端固定传 0.0.0.0 —— 一台机器有 IPv6 时入站只能绑 v4。
 * 这里盯渲染层那两半：
 *   ① 新建时可选 IPv6（::），提交进 payload 的是 listen 本身（不新增字段）
 *   ② 编辑老入站（listen 是历史手工值）时给「保留原值」选项，不动的老数据提交后不变
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

const inbound = (listen) => ({
  id: 5,
  tag: 'vless-reality',
  remark: '',
  protocol: 'vless',
  port: 443,
  listen,
  network: 'tcp',
  security: 'reality',
  enabled: true,
  sort: 0,
  client_count: 1,
  dest: 'www.cloudflare.com:443',
  server_names: ['www.cloudflare.com'],
  short_ids: ['ab'],
  public_key: 'pk',
})

beforeEach(() => {
  vi.clearAllMocks()
  toast.success.mockClear()
  toast.error.mockClear()
  adminApi.createXrayInbound = vi.fn(() => Promise.resolve({ inbound: { id: 9 }, config_version: 2 }))
  adminApi.updateXrayInbound = vi.fn(() => Promise.resolve({}))
})

const mountWith = async (inbounds) => {
  adminApi.listXrayInbounds = vi.fn(() => Promise.resolve({ inbounds, config_version: 1 }))
  const wrapper = mount(XrayInbounds, { attachTo: document.body })
  await flushPromises()
  return wrapper
}

const clickButton = async (wrapper, text) => {
  const btn = wrapper.findAll('button').find((b) => b.text().includes(text))
  expect(btn, `应有按钮「${text}」`).toBeTruthy()
  await btn.trigger('click')
  await flushPromises()
}

const submit = async () => {
  const btn = [...document.body.querySelectorAll('.ch-modal-footer button')]
    .find((b) => ['创建', '保存'].includes(b.textContent.trim()))
  expect(btn, '弹窗里应有提交按钮').toBeTruthy()
  btn.click()
  await flushPromises()
}

describe('XrayInbounds.vue — 地址族（issue #88）', () => {
  it('新建默认为 IPv4，选 IPv6 后提交 listen="::"', async () => {
    const wrapper = await mountWith([])
    await clickButton(wrapper, '新建入站')

    expect(wrapper.vm.form.listen).toBe('0.0.0.0')
    // 地址族下拉的两个固定选项（IPv4 默认 / IPv6）
    expect(wrapper.vm.listenOptions.map((o) => o.value)).toEqual(['0.0.0.0', '::'])

    wrapper.vm.listenFamily = '::'
    await flushPromises()
    await submit()

    expect(adminApi.createXrayInbound).toHaveBeenCalledTimes(1)
    const [, payload] = adminApi.createXrayInbound.mock.calls[0]
    expect(payload.listen).toBe('::')
    // 不新增字段：地址族就是 listen
    expect(payload).not.toHaveProperty('address_family')

    wrapper.unmount()
  })

  it('编辑老入站（listen 是历史手工值）→ 提供「保留原值」，原样提交', async () => {
    const wrapper = await mountWith([inbound('127.0.0.1')])
    // 列表里的编辑按钮是图标按钮（没有文字），直接走页面暴露的 openEdit
    wrapper.vm.openEdit(inbound('127.0.0.1'))
    await flushPromises()

    // 编辑态回填 + 多出一个「保留原值」选项
    expect(wrapper.vm.form.listen).toBe('127.0.0.1')
    const opts = wrapper.vm.listenOptions
    expect(opts.map((o) => o.value)).toEqual(['0.0.0.0', '::', '127.0.0.1'])
    expect(opts[2].label).toContain('保留原值')

    await submit()
    // updateXrayInbound(nodeId, id, payload)
    const [nodeArg, idArg, payload] = adminApi.updateXrayInbound.mock.calls[0]
    expect(nodeArg).toBe(1)
    expect(idArg).toBe(5)
    expect(payload.listen).toBe('127.0.0.1') // 没动它就不会被改写成 0.0.0.0

    wrapper.unmount()
  })
})
