import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 入站页 × HY2 端口跳跃区间（起点/终点两个数字框 ↔ 后端口径 "a-b"）。
 *
 * 存进去的永远是半角 "20000-20199"（后端只认这个格式），界面上拆成两个数字框，
 * 中间的「-」由代码写死 —— 用户手敲连字符时中文输入法常打成全角「－」或 en-dash「–」，
 * 历史数据里也存过这种串，所以回显时要把三种横线都归一成半角再拆。
 *
 * 这里盯四件事：
 *   ① splitHopRange / joinHopRange 在组件上确实定义（模板两处调用，缺一个就崩）
 *   ② 拆分：半角/全角/en-dash 都能拆开，拆不出的两个框都留空（不能让残串原样回填）
 *   ③ 拼接：两边都填才拼；只填一边视为没填全 → 空串（= 不跳跃）
 *   ④ 提交的 payload.hy_hop_ports 是拼回来的半角串，编辑回显落到两个输入框
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {
    listXrayInbounds: vi.fn(),
    listXrayCertificates: vi.fn(),
    createXrayInbound: vi.fn(),
    updateXrayInbound: vi.fn(),
  },
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

const mountedAll = []
afterEach(() => {
  mountedAll.forEach((w) => w.unmount())
  mountedAll.length = 0
  document.body.innerHTML = ''
})

const hyInbound = {
  id: 7, tag: 'hy2', protocol: 'hysteria', port: 56123, listen: '0.0.0.0',
  network: 'hysteria', security: 'tls',
  hy_hop_ports: '20000-20199', hy_hop_interval: '5-10',
  enabled: true,
}

const mountPage = async () => {
  adminApi.listXrayInbounds.mockResolvedValue({ inbounds: [] })
  adminApi.listXrayCertificates.mockResolvedValue({ certificates: [] })
  const wrapper = mount(XrayInbounds, { attachTo: document.body })
  mountedAll.push(wrapper)
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('XrayInbounds.vue — 跳跃区间拆分/拼接', () => {
  it('splitHopRange / joinHopRange 在组件上确实定义且可调用', async () => {
    const wrapper = await mountPage()

    // openEdit 里 ...splitHopRange(...) 展开、save 里 joinHopRange(...) 调用，
    // 任一缺失都会在打开弹窗/提交时抛 "is not a function"
    expect(typeof wrapper.vm.splitHopRange).toBe('function')
    expect(typeof wrapper.vm.joinHopRange).toBe('function')
  })

  it('splitHopRange：半角/全角/en-dash 都归一后拆开，非法串两个框留空', async () => {
    const wrapper = await mountPage()
    const split = wrapper.vm.splitHopRange

    expect(split('20000-20199')).toEqual({ hy_hop_start: '20000', hy_hop_end: '20199' })
    // 历史数据里存过的错字符（中文输入法全角－ / en-dash –）
    expect(split('20000－20199')).toEqual({ hy_hop_start: '20000', hy_hop_end: '20199' })
    expect(split('20000–20199')).toEqual({ hy_hop_start: '20000', hy_hop_end: '20199' })
    // 首尾空格不影响
    expect(split(' 20000 - 20199 ')).toEqual({ hy_hop_start: '20000', hy_hop_end: '20199' })

    // 拆不出来就两个框都留空，不能把残串塞进起点框（后端会报格式错）
    const empty = { hy_hop_start: '', hy_hop_end: '' }
    expect(split('')).toEqual(empty)
    expect(split(null)).toEqual(empty)
    expect(split(undefined)).toEqual(empty)
    expect(split('20000')).toEqual(empty)
    expect(split('20000-20199-30000')).toEqual(empty)
    expect(split('abc-def')).toEqual(empty)
    expect(split('20000_20199')).toEqual(empty)
  })

  it('joinHopRange：两边都填才拼半角串，只填一边视为不跳跃', async () => {
    const wrapper = await mountPage()
    const join = wrapper.vm.joinHopRange

    expect(join('20000', '20199')).toBe('20000-20199')
    // 数字类型（type=number 的输入框可能给数字）也要能拼
    expect(join(20000, 20199)).toBe('20000-20199')
    expect(join(' 20000 ', ' 20199 ')).toBe('20000-20199')

    // 残缺 = 没填全：给后端一个空串，走「不跳跃」，而不是发一个半截区间让它报错
    expect(join('', '')).toBe('')
    expect(join('20000', '')).toBe('')
    expect(join('', '20199')).toBe('')
    expect(join(null, null)).toBe('')
    expect(join(undefined, undefined)).toBe('')
  })
})

describe('XrayInbounds.vue — 跳跃区间提交与回显', () => {
  /** 打开新建弹窗并切到 HY2（跳跃区间只在 HY2 分支出现） */
  const openHy2Create = async (wrapper) => {
    wrapper.vm.openCreate()
    await flushPromises()
    wrapper.vm.form.protocol = 'hysteria'
    await flushPromises()
  }

  it('新建 HY2 入站：两个框拼成 hy_hop_ports 提交', async () => {
    const wrapper = await mountPage()
    adminApi.createXrayInbound.mockResolvedValue({ id: 1 })

    await openHy2Create(wrapper)
    Object.assign(wrapper.vm.form, {
      tag: 'hy2',
      port: 56123,
      hy_hop_start: '20000',
      hy_hop_end: '20199',
    })
    await flushPromises()
    await wrapper.vm.save()
    await flushPromises()

    expect(adminApi.createXrayInbound).toHaveBeenCalledTimes(1)
    const payload = adminApi.createXrayInbound.mock.calls[0][1]
    expect(payload.hy_hop_ports).toBe('20000-20199')
  })

  it('只填起点不填终点：当作不跳跃，hy_hop_ports 是空串', async () => {
    const wrapper = await mountPage()
    adminApi.createXrayInbound.mockResolvedValue({ id: 1 })

    await openHy2Create(wrapper)
    Object.assign(wrapper.vm.form, { tag: 'hy2', port: 56123, hy_hop_start: '20000', hy_hop_end: '' })
    await flushPromises()
    await wrapper.vm.save()
    await flushPromises()

    const payload = adminApi.createXrayInbound.mock.calls[0][1]
    expect(payload.hy_hop_ports).toBe('')
  })

  it('编辑回显：a-b 拆进两个数字框（全角破折号的历史数据也拆得开）', async () => {
    const wrapper = await mountPage()

    wrapper.vm.openEdit(hyInbound)
    await flushPromises()
    expect(wrapper.vm.form.hy_hop_start).toBe('20000')
    expect(wrapper.vm.form.hy_hop_end).toBe('20199')

    // 弹窗走 Teleport 到 body，断言要看 document.body
    const boxes = [...document.body.querySelectorAll('.an-hop-range input')]
    expect(boxes, 'HY2 表单里应有起点/终点两个输入框').toHaveLength(2)
    expect(boxes[0].value).toBe('20000')
    expect(boxes[1].value).toBe('20199')

    // 历史数据：全角－ 存的串，回显也要拆得开（否则用户看到的是两个空框，改一下就丢配置）
    wrapper.vm.openEdit({ ...hyInbound, hy_hop_ports: '20000－20199' })
    await flushPromises()
    expect(wrapper.vm.form.hy_hop_start).toBe('20000')
    expect(wrapper.vm.form.hy_hop_end).toBe('20199')

    // 没有跳跃区间：两个框都空，不是 "0" 也不是残留旧值
    wrapper.vm.openEdit({ ...hyInbound, hy_hop_ports: '' })
    await flushPromises()
    expect(wrapper.vm.form.hy_hop_start).toBe('')
    expect(wrapper.vm.form.hy_hop_end).toBe('')
  })

  it('新建表单字段初始化：跳跃区间两框空、默认端口/协议对上后端默认', async () => {
    const wrapper = await mountPage()

    wrapper.vm.openCreate()
    await flushPromises()

    const form = wrapper.vm.form
    expect(form.protocol).toBe('vless')
    expect(form.port).toBe(443)
    expect(form.listen).toBe('0.0.0.0')
    // 跳跃区间默认不填 = 不跳跃
    expect(form.hy_hop_start).toBe('')
    expect(form.hy_hop_end).toBe('')
    // 内核默认值，留 0 会被后端 min 校验拦下
    expect(form.hy_udp_idle_timeout).toBe(60)

    // 表单被复用后新建：上一次编辑的跳跃值不能残留
    wrapper.vm.openEdit(hyInbound)
    await flushPromises()
    expect(wrapper.vm.form.hy_hop_start).toBe('20000')
    wrapper.vm.openCreate()
    await flushPromises()
    expect(wrapper.vm.form.hy_hop_start).toBe('')
    expect(wrapper.vm.form.hy_hop_end).toBe('')
  })
})
