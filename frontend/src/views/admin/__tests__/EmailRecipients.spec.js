import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import { nextTick } from 'vue'

// 批量发信「收件人筛选」提交载荷的回归（2026-11-27 产品口径）：
//   ① 发送方式必须有「全部用户（不含封禁）」= mode:all —— 以前没有这个入口，发不了全量；
//   ② 用户状态必须是后端认的 normal / depleted —— 旧代码发 over/expired，后端直接 400；
//   ③ 限定套餐改多选：「未购买过套餐」走 include_no_plan，具体套餐走 plan_ids，
//      两者可同时勾（后端按并集取人）。
// 这里只验「前端发出去的 JSON 长什么样」，取数语义由后端单测覆盖。
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

const fillAndSend = async (wrapper) => {
  await wrapper.find('input[placeholder="例如：您的套餐即将到期"]').setValue('标题')
  await wrapper.find('textarea[placeholder="邮件内容，支持上方变量"]').setValue('正文')
  const btn = wrapper.findAll('button').find((b) => b.text().includes('加入队列群发'))
  await btn.trigger('click')
  await flushPromises()
  await nextTick()
}

const lastPayload = () => adminApi.batchSendEmail.mock.calls.at(-1)[0]

describe('Email.vue 批量发信收件人筛选', () => {
  beforeEach(() => {
    Object.assign(adminApi, {
      getEmail: vi.fn(() => Promise.resolve({ register_email_verify: true })),
      getEmailNotify: vi.fn(() => Promise.resolve({})),
      getEmailSchedule: vi.fn(() => Promise.resolve({ enabled: false, config: {} })),
      listPlans: vi.fn(() =>
        Promise.resolve([
          { id: 3, name: '樱桃' },
          { id: 4, name: '芒果' },
        ]),
      ),
      getEmailLogs: vi.fn(() => Promise.resolve([])),
      listSmtpAccounts: vi.fn(() => Promise.resolve([])),
      saveEmail: vi.fn(() => Promise.resolve()),
      saveEmailNotify: vi.fn(() => Promise.resolve()),
      saveEmailSchedule: vi.fn(() => Promise.resolve()),
      batchSendEmail: vi.fn(() => Promise.resolve({ batch_id: 1, total: 1 })),
      mailBatchProgress: vi.fn(() => Promise.resolve({ sent: 1, failed: 0, total: 1, done: true })),
    })
  })

  it('默认按条件群发：plan_ids 为空、include_no_plan=false、状态是后端认的 normal', async () => {
    const wrapper = await mountPage()
    await flushPromises()
    await fillAndSend(wrapper)

    const p = lastPayload()
    expect(p.mode).toBe('filter')
    expect(p.user_status).toBe('normal')
    expect(p.plan_ids).toEqual([])
    expect(p.include_no_plan).toBe(false)
    // 旧字段不该再出现（后端虽然兼容，但新前端不该发）
    expect(p.plan_id).toBeUndefined()
    wrapper.unmount()
  })

  it('发送方式里有「全部用户」并提交 mode=all', async () => {
    const wrapper = await mountPage()
    await flushPromises()

    // 下拉选项必须包含全量入口，否则用户根本发不了全量
    const state = wrapper.vm.$.setupState
    expect(state.sendModeOptions.some((o) => o.value === 'all')).toBe(true)
    const hasAllOption = wrapper
      .findAllComponents({ name: 'Select' })
      .some((c) => (c.props('options') || []).some((o) => o.value === 'all'))
    expect(hasAllOption).toBe(true)

    // 直接改状态再提交，确认 payload 是 all
    state.batch.mode = 'all'
    await nextTick()
    await fillAndSend(wrapper)
    expect(lastPayload().mode).toBe('all')
    wrapper.unmount()
  })

  it('勾了「未购买过套餐」→ include_no_plan=true 且不进 plan_ids', async () => {
    const wrapper = await mountPage()
    await flushPromises()

    const state = wrapper.vm.$.setupState
    // 0 是「未购买过套餐」的占位值
    state.batch.plan_ids = [0, 3]
    await nextTick()
    await fillAndSend(wrapper)

    const p = lastPayload()
    expect(p.include_no_plan).toBe(true)
    expect(p.plan_ids).toEqual([3])
    wrapper.unmount()
  })

  it('只勾具体套餐 → include_no_plan=false', async () => {
    const wrapper = await mountPage()
    await flushPromises()

    wrapper.vm.$.setupState.batch.plan_ids = [3, 4]
    await nextTick()
    await fillAndSend(wrapper)

    const p = lastPayload()
    expect(p.include_no_plan).toBe(false)
    expect(p.plan_ids).toEqual([3, 4])
    wrapper.unmount()
  })

  it('旧配置里的 over/expired 读回时归一为 depleted', async () => {
    adminApi.getEmailSchedule.mockResolvedValueOnce({
      enabled: true,
      day: 1,
      time: '09:00',
      config: { mode: 'filter', user_status: 'expired', plan_id: 4, subject: 's', body: 'b' },
    })
    const wrapper = await mountPage()
    await flushPromises()

    const cfg = wrapper.vm.$.setupState.schedule.config
    expect(cfg.user_status).toBe('depleted')
    // 旧单值 plan_id 迁移到多选
    expect(cfg.plan_ids).toEqual([4])
    wrapper.unmount()
  })
})
