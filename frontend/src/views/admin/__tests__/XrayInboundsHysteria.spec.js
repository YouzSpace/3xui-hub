import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 入站协议白名单 + HY2（hysteria2）表单（issue #94 面板侧前端）。
 *
 * 盯三件事：
 *   ① 协议下拉与后端白名单**完全一致**（多一项提交被 422 打回，少一项建不出来）
 *   ② 选 HY2 后传输/安全层自动收敛成 hysteria + tls，跳跃/伪装/证书字段出现
 *   ③ 提交的 payload 用后端实际的字段名（hy_hop_ports / tls_cert_id …），
 *      且跳跃间隔是字符串区间（"5-10"），不是被 Number() 吃掉
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {
    listXrayInbounds: vi.fn(),
    createXrayInbound: vi.fn(),
    updateXrayInbound: vi.fn(),
    listXrayCertificates: vi.fn(),
    getXrayCertificatePem: vi.fn(),
    issueXrayCertificate: vi.fn(),
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

// 用例开头都会把 body 清空，但上一实例还挂着（attachTo body）；它若还有待处理的渲染，
// 就会 patch 到已移除的节点上报 insertBefore(null)。统一在每个用例结束时卸载。
const mountedAll = []
const track = (wrapper) => {
  mountedAll.push(wrapper)
  return wrapper
}
afterEach(() => {
  mountedAll.forEach((w) => w.unmount())
  mountedAll.length = 0
})

const mountInbounds = async () => {
  adminApi.listXrayInbounds.mockResolvedValue({ inbounds: [] })
  adminApi.listXrayCertificates.mockResolvedValue({ certificates: [{ id: 9, name: 'wild', domain: 'a.example.com' }] })
  const wrapper = track(mount(XrayInbounds, { attachTo: document.body }))
  await flushPromises()
  return wrapper
}

describe('XrayInbounds — 协议白名单与 HY2 表单', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('协议下拉与后端白名单完全一致', async () => {
    const wrapper = await mountInbounds()
    // 后端 handler/admin/xray_inbound.go:395：in:vless,vmess,trojan,shadowsocks,hysteria,socks,http,tunnel,wireguard
    expect(wrapper.vm.protocolOptions.map((o) => o.value)).toEqual([
      'vless',
      'vmess',
      'trojan',
      'shadowsocks',
      'hysteria',
      'socks',
      'http',
      'tunnel',
      'wireguard',
    ])
  })

  it('选中 HY2 后传输/安全层收敛，并出现跳跃与证书字段', async () => {
    const wrapper = await mountInbounds()
    wrapper.vm.openCreate()
    await flushPromises()
    wrapper.vm.form.protocol = 'hysteria'
    await flushPromises()

    expect(wrapper.vm.form.network).toBe('hysteria')
    expect(wrapper.vm.form.security).toBe('tls')
    expect(wrapper.vm.networkOptions.map((o) => o.value)).toEqual(['hysteria'])
    expect(wrapper.vm.securityOptions.map((o) => o.value)).toEqual(['tls'])

    // 证书库已加载，下拉里能选到真实条目
    expect(wrapper.vm.certOptions.map((o) => o.value)).toContain(9)
    // Modal 走 Teleport 到 body，断言要看 document.body 而不是组件子树
    expect(document.body.textContent).toContain('跳跃区间')
    expect(document.body.textContent).toContain('证书库条目')
  })

  it('提交的 payload 用后端字段名，跳跃间隔保持字符串区间', async () => {
    const wrapper = await mountInbounds()
    adminApi.createXrayInbound.mockResolvedValue({ id: 1 })

    wrapper.vm.openCreate()
    await flushPromises()
    Object.assign(wrapper.vm.form, {
      protocol: 'hysteria',
      tag: 'hy2',
      port: 443,
      // 跳跃区间在表单里已拆成起点/终点两个框（提交时才拼回 "a-b"），
      // 直接写 hy_hop_ports 已经不会进 payload 了。
      hy_hop_start: '20000',
      hy_hop_end: '20199',
      hy_hop_interval: '5-10',
      hy_congestion: 'bbr',
      hy_masquerade_type: 'string',
      hy_masquerade_content: 'ok',
      tls_cert_id: 9,
    })
    await flushPromises()
    await wrapper.vm.save()
    await flushPromises()

    expect(adminApi.createXrayInbound).toHaveBeenCalledTimes(1)
    const payload = adminApi.createXrayInbound.mock.calls[0][1]
    expect(payload.protocol).toBe('hysteria')
    expect(payload.network).toBe('hysteria')
    expect(payload.security).toBe('tls')
    expect(payload.hy_hop_ports).toBe('20000-20199')
    // 关键：Number("5-10") 会变 NaN → 0，区间丢失 = 用户配了没效果
    expect(payload.hy_hop_interval).toBe('5-10')
    expect(payload.hy_congestion).toBe('bbr')
    expect(payload.hy_masquerade_type).toBe('string')
    expect(payload.hy_masquerade_content).toBe('ok')
    expect(payload.tls_cert_id).toBe(9)
  })

  it('编辑 HY2 入站时回显跳跃区间与证书引用', async () => {
    const wrapper = await mountInbounds()
    wrapper.vm.openEdit({
      id: 7,
      tag: 'hy2',
      protocol: 'hysteria',
      port: 443,
      listen: '0.0.0.0',
      network: 'hysteria',
      security: 'tls',
      hy_hop_ports: '20000-20199',
      hy_hop_interval: '5-10',
      hy_congestion: 'bbr',
      hy_masquerade: { type: 'string', content: 'ok' },
      tls_cert_id: 9,
      enabled: true,
    })
    await flushPromises()

    expect(wrapper.vm.form.hy_hop_ports).toBe('20000-20199')
    expect(wrapper.vm.form.hy_hop_interval).toBe('5-10')
    expect(wrapper.vm.form.tls_cert_id).toBe(9)
    expect(wrapper.vm.form.hy_masquerade_content).toBe('ok')
  })
})

/**
 * 端口跳跃区块（issue #94）：探测 → 应用 → 读回节点真实状态。
 * 版本不足时按钮禁用并提示「需升级 agent」，且一次指令都不能发。
 */
describe('XrayInbounds — 端口跳跃区块', () => {
  const hyInbound = {
    id: 7, tag: 'hy2', protocol: 'hysteria', port: 56123, listen: '0.0.0.0',
    network: 'hysteria', security: 'tls', hy_hop_ports: '20000-20199',
    hy_hop_interval: '5-10', enabled: true,
  }

  const mountWith = async (extra = {}) => {
    adminApi.listXrayInbounds.mockResolvedValue({ inbounds: [hyInbound], ...extra })
    adminApi.listXrayCertificates.mockResolvedValue({ certificates: [] })
    const wrapper = mount(XrayInbounds, { attachTo: document.body })
    await flushPromises()
    return wrapper
  }

  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
    adminApi.hopStatus = vi.fn().mockResolvedValue({
      result: {
        backend: 'iptables-nft',
        persist_available: true,
        chains: [{
          name: 'HY2HOP-7',
          rules: ['-p udp --dport 56123 -j REDIRECT --to-ports 56123'],
          jumps: ['-p udp --dport 20000:20199 -j HY2HOP-7'],
          persisted: true,
        }],
      },
    })
    adminApi.hopProbe = vi.fn().mockResolvedValue({
      result: { backend: 'iptables-nft', picked: '20000-20199', free_ranges: ['20000-20199'] },
    })
    adminApi.hopApply = vi.fn().mockResolvedValue({
      result: { backend: 'iptables-nft', chain: 'HY2HOP-7', verified: true, persisted: true },
    })
    adminApi.hopRemove = vi.fn().mockResolvedValue({ result: { verified: true } })
  })

  it('agent 版本不足：区块提示需升级，探测/应用/删除一律不发指令', async () => {
    const wrapper = await mountWith({ hop_supported: false, hop_min_agent_version: '1.0.7' })
    expect(wrapper.vm.hopSupported).toBe(false)

    await wrapper.vm.openHop(hyInbound)
    await flushPromises()

    expect(document.body.textContent).toContain('需升级 agent')
    expect(document.body.textContent).toContain('1.0.7')
    // 版本不足时连 status 都不发（后端会 412）；界面只留升级提示。
    expect(adminApi.hopStatus).not.toHaveBeenCalled()

    await wrapper.vm.runHopProbe()
    await wrapper.vm.runHopApply()
    await wrapper.vm.runHopRemove()
    await flushPromises()
    expect(adminApi.hopProbe).not.toHaveBeenCalled()
    expect(adminApi.hopApply).not.toHaveBeenCalled()
    expect(adminApi.hopRemove).not.toHaveBeenCalled()
  })

  it('agent 够新：探测 → 应用 → 展示节点真实链状态', async () => {
    const wrapper = await mountWith({ hop_supported: true })
    expect(wrapper.vm.hopSupported).toBe(true)

    await wrapper.vm.openHop(hyInbound)
    await flushPromises()
    expect(adminApi.hopStatus).toHaveBeenCalledWith(1)
    expect(wrapper.vm.hopChain?.name).toBe('HY2HOP-7')
    expect(wrapper.vm.hopChain?.persisted).toBe(true)

    await wrapper.vm.runHopProbe()
    await flushPromises()
    expect(adminApi.hopProbe).toHaveBeenCalledWith(1, expect.objectContaining({ inbound_id: 7 }))
    // 推荐区间自动填进「待应用」
    expect(wrapper.vm.hopPorts).toBe('20000-20199')

    await wrapper.vm.runHopApply()
    await flushPromises()
    expect(adminApi.hopApply).toHaveBeenCalledWith(1, { inbound_id: 7, ports: '20000-20199' })
    expect(wrapper.vm.hopApplyResult?.verified).toBe(true)
    // 应用后会再读一次真实状态
    expect(adminApi.hopStatus).toHaveBeenCalledTimes(2)
  })
})

/**
 * 入站表单证书三项（#103）：
 *   ① HY2 隐藏「证书 PEM / 私钥 PEM」两个框（后端完全不读），TLS 入站仍保留
 *   ② 「自动填充证书」按 serverName / 节点域名挑一张匹配且未过期的证书
 *   ③ 证书下拉标注来源与到期；域名覆盖不到当前目标域名的条目置灰
 */
describe('XrayInbounds — 证书三项（#103）', () => {
  const mountWithCerts = async (certs) => {
    adminApi.listXrayInbounds.mockResolvedValue({ inbounds: [] })
    adminApi.listXrayCertificates.mockResolvedValue({ certificates: certs })
    adminApi.getNode = vi.fn().mockResolvedValue({ id: 1, host: 'https://hy1.example.com:2053/panel' })
    const wrapper = track(mount(XrayInbounds, { attachTo: document.body }))
    await flushPromises()
    return wrapper
  }

  /** 打开新建弹窗并切到 HY2（证书区只在 HY2 分支出现） */
  const openHy2 = async (wrapper) => {
    wrapper.vm.openCreate()
    await flushPromises()
    wrapper.vm.form.protocol = 'hysteria'
    await flushPromises()
  }

  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('HY2 不渲染证书/私钥 PEM 框，切到 TLS 入站再回来', async () => {
    const wrapper = await mountWithCerts([
      { id: 1, name: 'hy1', domain: 'hy1.example.com', source: 'local-dir', has_cert: true, has_key: true },
    ])
    await openHy2(wrapper)

    expect(document.body.textContent).not.toContain('证书 PEM')
    expect(document.body.textContent).not.toContain('私钥 PEM')
    // 换成一句「HY2 走证书库引用」的说明，别让用户以为漏了必填项
    expect(document.body.textContent).toContain('HY2 不读内联 PEM')

    // TLS 入站（vless/trojan）才是真必填：两个框必须还在
    wrapper.vm.form.protocol = 'trojan'
    await flushPromises()
    expect(wrapper.vm.form.security).toBe('tls')
    expect(document.body.textContent).toContain('证书 PEM')
    expect(document.body.textContent).toContain('私钥 PEM')
  })

  it('证书下拉标注来源/到期，域名不匹配置灰、过期单独标注', async () => {
    const wrapper = await mountWithCerts([
      { id: 1, name: 'hy1', domain: 'hy1.example.com', source: 'local-dir', not_after: '2099-01-01T00:00:00Z', has_cert: true, has_key: true },
      { id: 2, name: 'other', domain: 'other.example.com', source: 'manual', not_after: '2099-01-01T00:00:00Z', has_cert: true, has_key: true },
      { id: 3, name: 'old', domain: 'hy1.example.com', source: 'manual', not_after: '2000-01-01T00:00:00Z', has_cert: true, has_key: true },
    ])
    await openHy2(wrapper)

    // 节点 host 带 scheme/端口/路径 → 剥成裸主机名后按它匹配
    expect(wrapper.vm.nodeHost).toBe('hy1.example.com')
    expect(wrapper.vm.certTargetHost).toBe('hy1.example.com')

    const opts = wrapper.vm.certOptions
    const byName = (name) => opts.find((o) => String(o.label).startsWith(name))
    expect(byName('hy1').disabled).toBe(false)
    expect(byName('hy1').label).toContain('本机目录')
    expect(byName('hy1').label).toContain('到期')
    expect(byName('other').disabled).toBe(true)
    expect(byName('other').label).toContain('域名不匹配')
    expect(byName('old').label).toContain('已过期')
  })

  it('自动填充证书：预填 SNI 并选中同域名里到期最晚的那张', async () => {
    const wrapper = await mountWithCerts([
      { id: 3, name: 'old', domain: 'hy1.example.com', not_after: '2000-01-01T00:00:00Z', has_cert: true, has_key: true },
      { id: 4, name: 'new', domain: 'hy1.example.com', not_after: '2099-01-01T00:00:00Z', has_cert: true, has_key: true },
      { id: 5, name: 'other', domain: 'other.example.com', not_after: '2099-01-01T00:00:00Z', has_cert: true, has_key: true },
    ])
    await openHy2(wrapper)

    wrapper.vm.autoFillCert()
    await flushPromises()

    expect(wrapper.vm.form.tls_server_name).toBe('hy1.example.com')
    expect(wrapper.vm.form.tls_cert_id).toBe(4)
    // 提交时引用证书 id（后端据此写 tlsCertId，私钥不出库）
    expect(adminApi.createXrayInbound).not.toHaveBeenCalled()
  })
})

/**
 * 证书库回填 PEM + 远程签发：
 *   ① TLS 入站（vless/trojan）多一个「从证书库填充 PEM」，选中条目 → 取解密 PEM → 填满两个框
 *   ② HY2 分支不出现这个按钮（它没有 PEM 框，走证书 id 引用）
 *   ③ 「签发证书」按 TLS serverName 下发 cert_issue（留空才回落节点域名）；serverName 是 IP 时禁用并说清原因
 */
describe('XrayInbounds — 证书库回填 PEM 与远程签发', () => {
  const mountWithCerts = async (certs, host = 'https://tls1.example.com:2053/panel') => {
    adminApi.listXrayInbounds.mockResolvedValue({ inbounds: [] })
    adminApi.listXrayCertificates.mockResolvedValue({ certificates: certs })
    adminApi.getNode = vi.fn().mockResolvedValue({ id: 1, host })
    const wrapper = track(mount(XrayInbounds, { attachTo: document.body }))
    await flushPromises()
    return wrapper
  }

  /** 打开新建弹窗并切成 TLS 入站（trojan + 安全层 tls） */
  const openTls = async (wrapper) => {
    wrapper.vm.openCreate()
    await flushPromises()
    wrapper.vm.form.protocol = 'trojan'
    wrapper.vm.form.security = 'tls'
    await flushPromises()
  }

  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  const tlsCert = {
    id: 7, name: 'tls1', domain: 'tls1.example.com', source: 'local-dir',
    has_cert: true, has_key: true, not_after: '2099-01-01T00:00:00Z',
  }

  it('TLS 入站：按钮在，点一下把证书与私钥两个框填满', async () => {
    const wrapper = await mountWithCerts([tlsCert])
    await openTls(wrapper)

    expect(document.body.textContent).toContain('从证书库填充 PEM')
    // 节点域名当作匹配目标 → 自动挑中同域名那张
    expect(wrapper.vm.pemCertId).toBe(7)

    adminApi.getXrayCertificatePem.mockResolvedValue({
      cert_pem: '-----BEGIN CERTIFICATE-----\nAAA',
      key_pem: '-----BEGIN PRIVATE KEY-----\nBBB',
    })
    wrapper.vm.fillTlsPemFromLibrary()
    await flushPromises()

    expect(adminApi.getXrayCertificatePem).toHaveBeenCalledWith(7)
    expect(wrapper.vm.form.tls_certificate).toContain('BEGIN CERTIFICATE')
    expect(wrapper.vm.form.tls_key).toContain('BEGIN PRIVATE KEY')
  })

  it('HY2 分支没有回填按钮（PEM 框本身也不该有）', async () => {
    const wrapper = await mountWithCerts([{ ...tlsCert, name: 'hy1', domain: 'tls1.example.com' }])
    wrapper.vm.openCreate()
    await flushPromises()
    wrapper.vm.form.protocol = 'hysteria'
    await flushPromises()

    expect(document.body.textContent).not.toContain('从证书库填充 PEM')
    expect(document.body.textContent).not.toContain('证书 PEM')
    expect(document.body.textContent).toContain('节点证书签发')
  })

  it('签发证书：域名节点调 issue，新证书回填两个框', async () => {
    const wrapper = await mountWithCerts([tlsCert])
    await openTls(wrapper)

    const issued = {
      id: 8, name: 'tls1-acme', domain: 'tls1.example.com', source: 'manual',
      has_cert: true, has_key: true, not_after: '2099-04-01T00:00:00Z',
    }
    adminApi.issueXrayCertificate.mockResolvedValue({ certificate: issued })
    adminApi.listXrayCertificates.mockResolvedValue({ certificates: [tlsCert, issued] })
    adminApi.getXrayCertificatePem.mockResolvedValue({ cert_pem: 'CERT-PEM', key_pem: 'KEY-PEM' })

    wrapper.vm.issueNodeCert()
    await flushPromises()

    expect(adminApi.issueXrayCertificate).toHaveBeenCalledWith({
      node_id: 1,
      domain: 'tls1.example.com',
      method: 'http-01',
    })
    // 列表刷新后按回执 id 取 PEM 回填
    expect(adminApi.getXrayCertificatePem).toHaveBeenCalledWith(8)
    expect(wrapper.vm.form.tls_certificate).toBe('CERT-PEM')
    expect(wrapper.vm.form.tls_key).toBe('KEY-PEM')
  })

  it('签发域名是 IP：按钮禁用，点了也不发请求', async () => {
    const wrapper = await mountWithCerts([], 'http://203.0.113.9:2053')
    await openTls(wrapper)

    // serverName 留空 → 回落节点 host（IP），不该能签
    expect(wrapper.vm.certSignDomain).toBe('203.0.113.9')
    expect(wrapper.vm.certSignDomainIsIp).toBe(true)
    wrapper.vm.issueNodeCert()
    await flushPromises()

    expect(adminApi.issueXrayCertificate).not.toHaveBeenCalled()
    expect(toast.error).toHaveBeenCalled()
  })

  it('serverName 与节点域名不同：按 serverName 签发并回填 PEM', async () => {
    const wrapper = await mountWithCerts([tlsCert])
    await openTls(wrapper)

    wrapper.vm.form.tls_server_name = 'v6.efu.top'
    await flushPromises()
    expect(wrapper.vm.certSignDomain).toBe('v6.efu.top')

    const issued = {
      id: 11, name: 'v6.efu.top', domain: 'v6.efu.top', source: 'manual',
      has_cert: true, has_key: true, not_after: '2099-05-01T00:00:00Z',
    }
    adminApi.issueXrayCertificate.mockResolvedValue({ certificate: issued })
    adminApi.listXrayCertificates.mockResolvedValue({ certificates: [tlsCert, issued] })
    adminApi.getXrayCertificatePem.mockResolvedValue({ cert_pem: 'CERT-PEM', key_pem: 'KEY-PEM' })

    wrapper.vm.issueNodeCert()
    await flushPromises()

    expect(adminApi.issueXrayCertificate).toHaveBeenCalledWith({
      node_id: 1,
      domain: 'v6.efu.top',
      method: 'http-01',
    })
    expect(wrapper.vm.form.tls_certificate).toBe('CERT-PEM')
    expect(wrapper.vm.form.tls_key).toBe('KEY-PEM')
  })

  it('改 serverName 后旧证书不再匹配：清空选择并提示先签发', async () => {
    const wrapper = await mountWithCerts([tlsCert])
    await openTls(wrapper)

    // 打开时按节点域名自动选中 tls1.example.com 那张
    expect(wrapper.vm.pemCertId).toBe(7)

    wrapper.vm.form.tls_server_name = 'wu.top'
    await flushPromises()

    expect(wrapper.vm.pemCertId).toBeNull()
    expect(wrapper.vm.pemCertPlaceholder).toContain('先点上方')
  })

  it('HY2 改 serverName 后，不匹配的已选证书同样被清空', async () => {
    const wrapper = await mountWithCerts([tlsCert])
    wrapper.vm.openCreate()
    await flushPromises()
    wrapper.vm.form.protocol = 'hysteria'
    await flushPromises()

    wrapper.vm.form.tls_cert_id = 7
    await flushPromises()
    wrapper.vm.form.tls_server_name = 'wu.top'
    await flushPromises()

    expect(wrapper.vm.form.tls_cert_id).toBeNull()
  })

  it('TLS 入站可以只引用证书库（PEM 留空），保存时带 tls_cert_id', async () => {
    const wrapper = await mountWithCerts([tlsCert])
    await openTls(wrapper)
    wrapper.vm.form.port = 8443
    adminApi.createXrayInbound.mockResolvedValue({ id: 1 })

    await wrapper.vm.save()
    await flushPromises()

    expect(adminApi.createXrayInbound).toHaveBeenCalled()
    const payload = adminApi.createXrayInbound.mock.calls[0][1]
    expect(payload.tls_cert_id).toBe(7)
    expect(payload.tls_certificate).toBeUndefined()
    expect(payload.tls_key).toBeUndefined()
  })

  it('TLS 入站既没引用也没 PEM：拦下不发请求', async () => {
    const wrapper = await mountWithCerts([])
    await openTls(wrapper)
    wrapper.vm.form.port = 8443

    await wrapper.vm.save()

    expect(adminApi.createXrayInbound).not.toHaveBeenCalled()
    expect(toast.error).toHaveBeenCalled()
  })
})
