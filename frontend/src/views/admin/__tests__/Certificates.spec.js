import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 证书管理页（Certificates.vue）。
 *
 * 接口口径见 server/internal/handler/admin/xray_certificates*.go：
 *   GET  /xray-certificates → { certificates: [...] }
 *   POST /xray-certificates/renew → dry_run 只出计划，false 真跑
 *   POST /xray-certificates/sync-local → { dir, scanned, created, updated, skipped, reasons }
 *   GET  /xray-certificates/:id/pem → { cert_pem, key_pem }
 *   DELETE /xray-certificates/:id
 *
 * 守四件事：
 *  1. 列表按后端字段渲染（来源是派生字段 local-dir/manual，自动续期责任方按来源确定）；
 *  2. 「续期检查」必须带 dry_run=true，计划条数与 checked/due 展示在弹窗；
 *  3. 「执行续期」先确认、再带 dry_run=false，成功后 toast 并刷新；
 *  4. 删除必须确认，取消时不调接口；下载 PEM 走 blob，不把私钥写进下载文件。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import Certificates from '@/views/admin/Certificates.vue'

const DAY = 86400000
const iso = (msFromNow) => new Date(Date.now() + msFromNow).toISOString()

const CERTS = [
  // 本机目录维护、40 天后到期 → 状态正常，责任方 acme.sh
  {
    id: 1, name: 'a.example.com', domain: 'a.example.com', source: 'local-dir',
    has_cert: true, has_key: true, cert_pem_chars: 1800, not_after: iso(40 * DAY),
  },
  // 手工导入、10 天后到期 → 续签窗口内，责任方节点自动续签
  {
    id: 2, name: 'b.example.com', domain: 'b.example.com', source: 'manual',
    has_cert: true, has_key: true, cert_pem_chars: 1800, not_after: iso(10 * DAY),
  },
]

beforeEach(() => {
  vi.clearAllMocks()
  adminApi.listXrayCertificates = vi.fn(() => Promise.resolve({ certificates: CERTS }))
  adminApi.renewXrayCertificates = vi.fn((dryRun) =>
    dryRun
      ? Promise.resolve({
          dry_run: true, window_days: 21, checked: 2, due: 1, skipped: 0,
          plan: [{ id: 2, name: 'b.example.com', not_after: iso(10 * DAY), ref_nodes: [3], target_node: 'hk-1' }],
          reasons: [],
        })
      : Promise.resolve({ dry_run: false, checked: 2, due: 1, renewed: 1, failed: 0, skipped: 0, bumped_nodes: 1, reasons: [] }),
  )
  adminApi.syncLocalXrayCertificates = vi.fn(() =>
    Promise.resolve({ dir: '/root/cert', scanned: 3, created: 1, updated: 2, skipped: 0, reasons: [] }),
  )
  adminApi.getXrayCertificatePem = vi.fn(() =>
    Promise.resolve({ id: 1, cert_pem: '-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----', key_pem: 'SECRET' }),
  )
  adminApi.deleteXrayCertificate = vi.fn(() => Promise.resolve({ deleted: true }))
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
  const wrapper = mount(Certificates, { attachTo: document.body })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

const rowOf = (wrapper, text) => wrapper.findAll('tbody tr').find((tr) => tr.text().includes(text))
const buttonOf = (wrapper, text) => wrapper.findAll('button').find((b) => b.text().includes(text))

describe('Certificates.vue — 列表', () => {
  it('渲染域名 / 到期时间 / 到期状态 / 来源 / 自动续期', async () => {
    const wrapper = await mountPage()

    expect(adminApi.listXrayCertificates).toHaveBeenCalledTimes(1)
    expect(wrapper.findAll('tbody tr')).toHaveLength(2)

    const local = rowOf(wrapper, 'a.example.com')
    expect(local.text()).toContain('本机目录')
    expect(local.text()).toContain('本机 acme.sh')
    expect(local.text()).toContain('正常')

    const manual = rowOf(wrapper, 'b.example.com')
    expect(manual.text()).toContain('手工导入')
    expect(manual.text()).toContain('节点自动续签')
    // 10 天后到期 → 落在 21 天续签窗口内
    expect(manual.text()).toContain('即将到期')
    expect(manual.text()).toContain('剩 10 天')
  })

  it('空态给出下一步：同步本机证书', async () => {
    adminApi.listXrayCertificates = vi.fn(() => Promise.resolve({ certificates: [] }))
    const wrapper = await mountPage()

    expect(wrapper.find('.ch-empty').text()).toContain('证书库还是空的')
    expect(wrapper.find('.ch-empty').text()).toContain('同步本机证书')
  })

  it('已过期与缺私钥都能在行内标出来', async () => {
    adminApi.listXrayCertificates = vi.fn(() =>
      Promise.resolve({
        certificates: [{
          id: 9, name: 'old.example.com', domain: 'old.example.com', source: 'manual',
          has_cert: true, has_key: false, cert_pem_chars: 100, not_after: iso(-3 * DAY),
        }],
      }),
    )
    const wrapper = await mountPage()

    const row = rowOf(wrapper, 'old.example.com')
    expect(row.text()).toContain('已过期')
    expect(row.text()).toContain('已过期 3 天')
    expect(row.text()).toContain('缺证书或私钥')
  })

  it('加载失败显示错误行，不崩', async () => {
    adminApi.listXrayCertificates = vi.fn(() => Promise.reject({ message: '服务暂不可用' }))
    const wrapper = await mountPage()

    expect(wrapper.find('.ch-empty').text()).toContain('加载失败：服务暂不可用')
  })
})

describe('Certificates.vue — 续期检查与执行', () => {
  it('续期检查带 dry_run=true，弹窗展示计划条数与 checked/due', async () => {
    const wrapper = await mountPage()

    await buttonOf(wrapper, '续期检查').trigger('click')
    await flushPromises()

    expect(adminApi.renewXrayCertificates).toHaveBeenCalledWith(true)
    expect(wrapper.vm.checkOpen).toBe(true)
    // Modal Teleport 到 body
    const body = document.body.textContent
    expect(body).toContain('续期检查')
    expect(body).toContain('检查 2 张')
    expect(body).toContain('到期 1 张')
    expect(body).toContain('hk-1')
    // 检查绝不触发真实续期
    expect(adminApi.renewXrayCertificates).not.toHaveBeenCalledWith(false)
  })

  it('检查失败弹后端原文', async () => {
    adminApi.renewXrayCertificates = vi.fn(() => Promise.reject({ message: '生成续签计划失败' }))
    const wrapper = await mountPage()

    await buttonOf(wrapper, '续期检查').trigger('click')
    await flushPromises()

    expect(toast.error).toHaveBeenCalledWith('生成续签计划失败')
    expect(wrapper.vm.checkOpen).toBe(false)
  })

  it('执行续期：确认后带 dry_run=false，toast 结果并刷新列表', async () => {
    const wrapper = await mountPage()
    const callsBefore = adminApi.listXrayCertificates.mock.calls.length

    await buttonOf(wrapper, '执行续期').trigger('click')
    await flushPromises()

    expect(globalThis.confirm).toHaveBeenCalled()
    expect(adminApi.renewXrayCertificates).toHaveBeenCalledWith(false)
    expect(toast.success).toHaveBeenCalledWith(
      expect.stringContaining('成功 1 · 失败 0'),
    )
    expect(adminApi.listXrayCertificates.mock.calls.length).toBeGreaterThan(callsBefore)
  })

  it('执行续期取消时什么都不调', async () => {
    vi.stubGlobal('confirm', vi.fn(() => false))
    const wrapper = await mountPage()

    await buttonOf(wrapper, '执行续期').trigger('click')
    await flushPromises()

    expect(adminApi.renewXrayCertificates).not.toHaveBeenCalled()
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('同步本机证书：确认后 toast scanned/created/updated', async () => {
    const wrapper = await mountPage()

    await buttonOf(wrapper, '同步本机证书').trigger('click')
    await flushPromises()

    expect(globalThis.confirm).toHaveBeenCalled()
    expect(adminApi.syncLocalXrayCertificates).toHaveBeenCalled()
    expect(toast.success).toHaveBeenCalledWith('同步完成：扫描 3 · 新增 1 · 更新 2')
  })
})

describe('Certificates.vue — 行内操作', () => {
  it('下载 PEM 取回 cert_pem 并以域名命名保存（不含私钥）', async () => {
    const blobs = []
    const originalCreate = URL.createObjectURL
    const originalRevoke = URL.revokeObjectURL
    URL.createObjectURL = vi.fn((blob) => {
      blobs.push(blob)
      return 'blob:mock'
    })
    URL.revokeObjectURL = vi.fn()
    // 拦下 <a> 点击：jsdom 没有真实下载
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})

    try {
      const wrapper = await mountPage()
      const row = rowOf(wrapper, 'a.example.com')
      await row.findAll('button').find((b) => b.text() === '下载 PEM').trigger('click')
      await flushPromises()

      expect(adminApi.getXrayCertificatePem).toHaveBeenCalledWith(1)
      expect(blobs).toHaveLength(1)
      const text = await blobs[0].text()
      expect(text).toContain('BEGIN CERTIFICATE')
      // 下载文件只含证书链，私钥不得出现在里面
      expect(text).not.toContain('SECRET')
      expect(toast.success).toHaveBeenCalledWith('已下载 a.example.com.pem')
    } finally {
      clickSpy.mockRestore()
      URL.createObjectURL = originalCreate
      URL.revokeObjectURL = originalRevoke
    }
  })

  it('删除：确认后调 DELETE 并刷新', async () => {
    const wrapper = await mountPage()
    const callsBefore = adminApi.listXrayCertificates.mock.calls.length

    const row = rowOf(wrapper, 'b.example.com')
    await row.findAll('button').find((b) => b.text() === '删除').trigger('click')
    await flushPromises()

    expect(globalThis.confirm).toHaveBeenCalled()
    expect(String(globalThis.confirm.mock.calls[0][0])).toContain('b.example.com')
    expect(adminApi.deleteXrayCertificate).toHaveBeenCalledWith(2)
    expect(toast.success).toHaveBeenCalledWith('已删除')
    expect(adminApi.listXrayCertificates.mock.calls.length).toBeGreaterThan(callsBefore)
  })

  it('删除取消时不调接口', async () => {
    vi.stubGlobal('confirm', vi.fn(() => false))
    const wrapper = await mountPage()

    const row = rowOf(wrapper, 'b.example.com')
    await row.findAll('button').find((b) => b.text() === '删除').trigger('click')
    await flushPromises()

    expect(adminApi.deleteXrayCertificate).not.toHaveBeenCalled()
  })
})
