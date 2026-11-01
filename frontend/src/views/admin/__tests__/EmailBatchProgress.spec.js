import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import { nextTick } from 'vue'

// 批量发信进度条：前端每 5 秒轮询一次 batch-progress。
// 覆盖两条最容易回归的行为 ——
//   ① done 之后必须停表（否则页面挂着就一直在打接口）；
//   ② 失败数 > 0 要按原文案提示去「日志」Tab 看原因。
const { adminApi } = vi.hoisted(() => ({ adminApi: {} }))

vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({
  default: { template: '<div />' },
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))

import Email from '@/views/admin/Email.vue'

const mountPage = async () => {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: { template: '<div/>' } }],
  })
  router.push('/?tab=settings')
  await router.isReady()
  return mount(Email, { global: { plugins: [router] } })
}

// 提交一次批量发信：返回发完后的 wrapper
const submitBatch = async (wrapper, res) => {
  adminApi.batchSendEmail.mockResolvedValueOnce(res)
  await wrapper.find('input[placeholder="例如：您的套餐即将到期"]').setValue('标题')
  await wrapper
    .find('textarea[placeholder="邮件内容，支持上方变量"]')
    .setValue('正文')
  const btn = wrapper.findAll('button').find((b) => b.text().includes('加入队列群发'))
  await btn.trigger('click')
  await flushPromises()
  await nextTick()
}

describe('Email.vue 批量发信进度', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    Object.assign(adminApi, {
      // 批量发信卡片挂在「邮箱验证」开关下，先打开才渲染得出来
      getEmail: vi.fn(() => Promise.resolve({ register_email_verify: true })),
      getEmailNotify: vi.fn(() => Promise.resolve({})),
      getEmailSchedule: vi.fn(() => Promise.resolve({ enabled: false, config: {} })),
      listPlans: vi.fn(() => Promise.resolve([])),
      getEmailLogs: vi.fn(() => Promise.resolve([])),
      listSmtpAccounts: vi.fn(() => Promise.resolve([])),
      saveEmail: vi.fn(() => Promise.resolve()),
      saveEmailNotify: vi.fn(() => Promise.resolve()),
      saveEmailSchedule: vi.fn(() => Promise.resolve()),
      batchSendEmail: vi.fn(),
      mailBatchProgress: vi.fn(),
    })
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('子测：入队后立刻拉一次进度并显示「已发 X / Y」，done 后停表', async () => {
    const wrapper = await mountPage()
    await flushPromises()

    adminApi.mailBatchProgress
      .mockResolvedValueOnce({ sent: 1, failed: 0, total: 3, done: false })
      .mockResolvedValueOnce({ sent: 3, failed: 0, total: 3, done: true })

    await submitBatch(wrapper, { batch_id: 2026102801, total: 3, estimate_seconds: 120 })

    // 入队成功后立刻拉一次，不等 5 秒
    expect(adminApi.mailBatchProgress).toHaveBeenCalledTimes(1)
    expect(adminApi.mailBatchProgress).toHaveBeenCalledWith(2026102801)
    expect(wrapper.text()).toContain('已发 1 / 3')
    expect(wrapper.text()).toContain('预计 2 分钟内发完')

    // 5 秒后第二轮：done=true → 显示完成、隐藏预计时长
    await vi.advanceTimersByTimeAsync(5000)
    await flushPromises()
    await nextTick()
    expect(adminApi.mailBatchProgress).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('已发 3 / 3')
    expect(wrapper.text()).toContain('发送完成（成功 3 / 失败 0）')
    expect(wrapper.text()).not.toContain('预计 2 分钟内发完')

    // done 之后再走多久都不再请求
    await vi.advanceTimersByTimeAsync(30000)
    await flushPromises()
    expect(adminApi.mailBatchProgress).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('失败数 > 0 时标红并提示去「日志」Tab 看原因', async () => {
    const wrapper = await mountPage()
    await flushPromises()

    adminApi.mailBatchProgress
      .mockResolvedValueOnce({ sent: 1, failed: 1, total: 3, done: false })
      .mockResolvedValueOnce({ sent: 2, failed: 1, total: 3, done: true })

    await submitBatch(wrapper, { batch_id: 2026102802, total: 3, estimate_seconds: 0 })

    // estimate_seconds=0 → 「立即发完」（未完成时才显示）
    expect(wrapper.text()).toContain('立即发完')
    expect(wrapper.text()).toContain('已发 1 / 3')
    // 失败提示在未完成时就已经出现，不用等发完
    expect(wrapper.text()).toContain('失败 1 封，去「日志」Tab 看原因')
    expect(wrapper.text()).toContain('进度可在「日志」Tab 查看。')

    await vi.advanceTimersByTimeAsync(5000)
    await flushPromises()
    await nextTick()
    expect(wrapper.text()).toContain('已发 2 / 3')
    expect(wrapper.text()).toContain('发送完成（成功 2 / 失败 1）')
    // 发完后隐藏预计时长；失败提示保留
    expect(wrapper.text()).not.toContain('立即发完')
    expect(wrapper.text()).toContain('失败 1 封，去「日志」Tab 看原因')
    // 进度条宽度按「已发+失败」算
    expect(wrapper.find('.email-progress-fill').attributes('style')).toContain('width: 100%')
    wrapper.unmount()
  })

  it('切到别的 Tab 后停止轮询，切回来若没发完才继续', async () => {
    const wrapper = await mountPage()
    await flushPromises()

    adminApi.mailBatchProgress.mockResolvedValue({ sent: 0, failed: 0, total: 5, done: false })
    await submitBatch(wrapper, { batch_id: 2026102804, total: 5, estimate_seconds: 300 })
    expect(adminApi.mailBatchProgress).toHaveBeenCalledTimes(1)

    // 切到「日志」：进度条看不见了，不该再打接口
    const logsTab = wrapper.findAll('.email-tab').find((t) => t.text() === '日志')
    await logsTab.trigger('click')
    await flushPromises()
    await vi.advanceTimersByTimeAsync(20000)
    await flushPromises()
    expect(adminApi.mailBatchProgress).toHaveBeenCalledTimes(1)

    // 切回「设置」：这批还没发完 → 续上（立刻拉一次）
    const settingsTab = wrapper.findAll('.email-tab').find((t) => t.text() === '设置')
    await settingsTab.trigger('click')
    await flushPromises()
    expect(adminApi.mailBatchProgress).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('组件卸载后不再轮询（定时器必须清掉）', async () => {
    const wrapper = await mountPage()
    await flushPromises()

    adminApi.mailBatchProgress.mockResolvedValue({ sent: 0, failed: 0, total: 5, done: false })
    await submitBatch(wrapper, { batch_id: 2026102803, total: 5, estimate_seconds: 300 })
    expect(adminApi.mailBatchProgress).toHaveBeenCalledTimes(1)

    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(20000)
    await flushPromises()
    expect(adminApi.mailBatchProgress).toHaveBeenCalledTimes(1)
  })
})
