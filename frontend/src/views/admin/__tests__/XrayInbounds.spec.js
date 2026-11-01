import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 入站页 × REALITY serverNames 拦截的前后端联动（issue #57）。
 *
 * 后端（XrayInboundController::guardServerNames）对「与 dest 不同站点」的 serverNames 返回
 * HTTP 200 + {code:422, msg}（bootstrap/app.php 统一包成业务码），前端 client.js 会把这个
 * 业务码透传成 reject 的 code。这里盯的是渲染层那一半：
 *   ① code=422 且 msg 提到 serverNames → 弹确认，确认后带 confirm_server_names 重发
 *   ② 用户点取消 → 不重发，按普通错误提示
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))
// 节点选择器：挂载即选中 1 号节点，省去驱动它内部列表的麻烦
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

const CONFLICT = {
  code: 422,
  message: '这些 serverNames 与 dest（www.cloudflare.com）不是同一站点的域名：a.example.com。',
}

beforeEach(() => {
  adminApi.listXrayInbounds = vi.fn(() => Promise.resolve({ inbounds: [], config_version: 1 }))
  adminApi.createXrayInbound = vi.fn(() => Promise.resolve({ inbound: { id: 9 }, config_version: 2 }))
  adminApi.updateXrayInbound = vi.fn(() => Promise.resolve({}))
  vi.clearAllMocks()
  toast.success.mockClear()
  toast.error.mockClear()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

const mountPage = async () => {
  const wrapper = mount(XrayInbounds, { attachTo: document.body })
  await flushPromises()
  return wrapper
}

/** 打开「新建入站」弹窗并点提交（默认表单就是一条 reality/vless/443）。 */
const submitCreate = async (wrapper) => {
  await wrapper.findAll('button').find((b) => b.text().includes('新建入站')).trigger('click')
  await flushPromises()

  const submit = [...document.body.querySelectorAll('.ch-modal-footer button')]
    .find((b) => b.textContent.trim() === '创建')
  expect(submit, '弹窗里应有创建按钮').toBeTruthy()
  submit.click()
  await flushPromises()
}

describe('XrayInbounds.vue — serverNames 拦截联动（issue #57）', () => {
  it('后端 422 时弹确认，确认后带 confirm_server_names 重发', async () => {
    const wrapper = await mountPage()
    const confirmSpy = vi.fn(() => true)
    vi.stubGlobal('confirm', confirmSpy)

    adminApi.createXrayInbound = vi.fn()
      .mockRejectedValueOnce(CONFLICT)
      .mockResolvedValueOnce({ inbound: { id: 9 }, config_version: 2 })

    await submitCreate(wrapper)

    expect(confirmSpy).toHaveBeenCalled()
    expect(adminApi.createXrayInbound).toHaveBeenCalledTimes(2)

    const [firstNode, firstPayload] = adminApi.createXrayInbound.mock.calls[0]
    const [secondNode, secondPayload] = adminApi.createXrayInbound.mock.calls[1]

    expect(firstNode).toBe(1)
    expect(secondNode).toBe(1)
    expect(firstPayload.confirm_server_names).toBeUndefined()
    expect(secondPayload.confirm_server_names).toBe(true)
    expect(secondPayload.server_names).toEqual(firstPayload.server_names)

    wrapper.unmount()
  })

  it('用户取消确认时不重发，按普通错误提示', async () => {
    const wrapper = await mountPage()
    vi.stubGlobal('confirm', vi.fn(() => false))

    adminApi.createXrayInbound = vi.fn().mockRejectedValueOnce(CONFLICT)

    await submitCreate(wrapper)

    expect(adminApi.createXrayInbound).toHaveBeenCalledTimes(1)
    expect(toast.error).toHaveBeenCalledWith(expect.stringContaining('serverNames'))

    wrapper.unmount()
  })

  it('非 serverNames 的 422 不弹确认，直接报错', async () => {
    const wrapper = await mountPage()
    const confirmSpy = vi.fn(() => true)
    vi.stubGlobal('confirm', confirmSpy)

    adminApi.createXrayInbound = vi.fn().mockRejectedValueOnce({
      code: 422,
      message: 'Reality 仅支持 VLESS 协议',
    })

    await submitCreate(wrapper)

    expect(confirmSpy).not.toHaveBeenCalled()
    expect(adminApi.createXrayInbound).toHaveBeenCalledTimes(1)
    expect(toast.error).toHaveBeenCalledWith('Reality 仅支持 VLESS 协议')

    wrapper.unmount()
  })
})
