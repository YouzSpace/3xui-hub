import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 配置模板面板（#地区功能-20）。
 *
 * 盯三件容易回归的事：
 *  ① 三类模板各自成表，数据从一次 GET /admin-api/xray-templates 拆开渲染；
 *  ② 编辑弹窗里的 JSON 列提交前先校验 —— 格式错只提示、不发请求；
 *  ③ 套用只列 xray 节点（3x-ui 节点没有模板的落表），逐节点结果要显示「跳过 + 原因」。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import TemplatePanel from '@/views/admin/xray/TemplatePanel.vue'

const TEMPLATES = {
  inbounds: [
    { id: 11, name: '标准 vless-reality 443', tag: 'vless-reality', protocol: 'vless', port: 443, listen: '0.0.0.0', settings: { decryption: 'none' }, stream_settings: { network: 'tcp' }, enabled: true },
  ],
  outbounds: [
    { id: 21, name: '直连出站', tag: 'direct', protocol: 'freedom', settings: { domainStrategy: 'AsIs' }, stream_settings: null, enabled: true },
  ],
  routing: [
    { id: 31, name: '国内直连', domains: ['geosite:cn'], ips: [], port: null, network: null, outbound_tag: 'direct', enabled: true },
  ],
}

const NODES = [
  { id: 1, name: 'HK-1', driver_type: 'xray' },
  { id: 2, name: 'JP-面板机', driver_type: '3x-ui' },
]

beforeEach(() => {
  adminApi.listXrayTemplates = vi.fn(() => Promise.resolve(TEMPLATES))
  adminApi.listNodes = vi.fn(() => Promise.resolve(NODES))
  adminApi.createXrayTemplate = vi.fn(() => Promise.resolve({ id: 99 }))
  adminApi.applyXrayTemplates = vi.fn(() => Promise.resolve({ results: [], applied: 0, skipped: 0, failed: 0 }))
  toast.success.mockClear()
  toast.error.mockClear()
})

afterEach(() => {
  vi.clearAllMocks()
})

const mountPanel = async () => {
  const wrapper = mount(TemplatePanel, { attachTo: document.body })
  await flushPromises()
  return wrapper
}

/** 按 label 文案取弹窗里的控件（弹窗是 Teleport 到 body 的，只能用 document 查）。 */
const fieldControl = (labelText) => {
  const field = [...document.body.querySelectorAll('.xt-field')].find((l) =>
    l.textContent.includes(labelText),
  )
  return field ? field.querySelector('input, textarea') : null
}

/** 给原生控件赋值并触发 Vue 的 v-model 监听。 */
const typeInto = async (el, value, eventName = 'input') => {
  el.value = value
  el.dispatchEvent(new Event(eventName))
  await flushPromises()
}

const clickButton = async (scope, text) => {
  const btn = [...scope.querySelectorAll('button')].find((b) => b.textContent.trim().includes(text))
  expect(btn, `应存在按钮「${text}」`).toBeTruthy()
  btn.click()
  await flushPromises()
}

describe('TemplatePanel.vue — 配置模板', () => {
  it('三类模板各自渲染成表（入站 / 出站 / 路由）', async () => {
    const wrapper = await mountPanel()

    expect(adminApi.listXrayTemplates).toHaveBeenCalledTimes(1)
    const text = wrapper.text()
    expect(text).toContain('入站模板')
    expect(text).toContain('出站模板')
    expect(text).toContain('路由模板')

    // 每类各自那一行
    expect(text).toContain('标准 vless-reality 443')
    expect(text).toContain('vless-reality')
    expect(text).toContain('直连出站')
    expect(text).toContain('国内直连')
    expect(text).toContain('geosite:cn')

    // 三类都各自有新建入口
    expect(text).toContain('新建入站模板')
    expect(text).toContain('新建出站模板')
    expect(text).toContain('新建路由模板')

    wrapper.unmount()
  })

  it('新建入站模板：JSON 格式错时不提交并提示', async () => {
    const wrapper = await mountPanel()

    await clickButton(wrapper.element, '新建入站模板')

    await typeInto(fieldControl('名称'), '标准模板')
    await typeInto(fieldControl('tag（套用时'), 'vless-reality')
    await typeInto(fieldControl('端口'), '8443')
    // settings 少一个右括号：JSON.parse 必挂
    await typeInto(fieldControl('settings（JSON'), '{ "clients": [ ]')

    await clickButton(document.body, '创建')

    expect(toast.error).toHaveBeenCalledTimes(1)
    expect(String(toast.error.mock.calls[0][0])).toContain('settings')
    expect(String(toast.error.mock.calls[0][0])).toContain('JSON')
    // 关键：格式错不发请求，弹窗留着让用户改
    expect(adminApi.createXrayTemplate).not.toHaveBeenCalled()

    // 改对之后同一个弹窗能提交，且 JSON 列以对象形式提交
    await typeInto(fieldControl('settings（JSON'), '{ "clients": [], "decryption": "none" }')
    await clickButton(document.body, '创建')

    expect(adminApi.createXrayTemplate).toHaveBeenCalledTimes(1)
    const [kind, payload] = adminApi.createXrayTemplate.mock.calls[0]
    expect(kind).toBe('inbound')
    expect(payload).toMatchObject({
      name: '标准模板',
      tag: 'vless-reality',
      protocol: 'vless',
      port: 8443,
      listen: '0.0.0.0',
      settings: { clients: [], decryption: 'none' },
      stream_settings: null,
      enabled: true,
    })

    wrapper.unmount()
  })

  it('套用：只列 xray 节点，逐节点结果展示「跳过 + 原因」', async () => {
    adminApi.applyXrayTemplates = vi.fn(() =>
      Promise.resolve({
        results: [
          { node_id: 1, node_name: 'HK-1', status: 'skipped', message: '节点单独配置过，未勾选覆盖，已跳过' },
        ],
        applied: 0,
        skipped: 1,
        failed: 0,
      }),
    )
    const wrapper = await mountPanel()

    // 节点多选只列 driver_type=xray 的
    const nodeBoxes = wrapper.findAll('.xt-node')
    expect(nodeBoxes).toHaveLength(1)
    expect(nodeBoxes[0].text()).toContain('HK-1')
    expect(wrapper.text()).not.toContain('JP-面板机')

    const box = nodeBoxes[0].find('input[type="checkbox"]')
    box.element.checked = true
    await box.trigger('change')

    await clickButton(wrapper.element, '套用')

    expect(adminApi.applyXrayTemplates).toHaveBeenCalledTimes(1)
    expect(adminApi.applyXrayTemplates.mock.calls[0][0]).toEqual({
      node_ids: [1],
      overwrite_custom: false,
    })

    // 结果区：跳过 + 后端给的原因
    const result = wrapper.find('.xt-result')
    expect(result.exists()).toBe(true)
    expect(result.text()).toContain('HK-1')
    expect(result.text()).toContain('跳过')
    expect(result.text()).toContain('节点单独配置过，未勾选覆盖，已跳过')
    expect(result.text()).toContain('跳过 1')

    wrapper.unmount()
  })
})
