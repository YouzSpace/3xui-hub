import { describe, it, expect, beforeEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import { nextTick } from 'vue'

// 「改完自动保存」的竞态回归：
// 本页 keep-alive，加载响应可能晚于用户操作到达。修复前，迟到的响应会把用户刚打开的
// 开关覆盖回服务器旧值，随后自动保存又把这个旧值提交回去 —— 表现为「开关状态没保存」、
// 「限制发送没生效」（mail_rate_limit_enabled 同样被盖回关闭）。
// 修复：加载响应若赶上「本地有未保存修改」或「已晚于之后的成功保存」→ 丢弃。
const { adminApi, scheduleLoads } = vi.hoisted(() => ({ adminApi: {}, scheduleLoads: [] }))

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
  router.push('/?tab=scheduled')
  await router.isReady()
  return mount(Email, { global: { plugins: [router] } })
}

const wait = (ms) => new Promise((r) => setTimeout(r, ms))

describe('Email.vue 自动保存竞态', () => {
  beforeEach(() => {
    scheduleLoads.length = 0
    Object.assign(adminApi, {
      getEmail: vi.fn(() => Promise.resolve({})),
      getEmailNotify: vi.fn(() => Promise.resolve({})),
      // 定时发信配置的加载：挂起，由用例手动决定「什么时候返回、返回什么旧值」
      getEmailSchedule: vi.fn(() => new Promise((resolve) => scheduleLoads.push(resolve))),
      listPlans: vi.fn(() => Promise.resolve([])),
      getEmailLogs: vi.fn(() => Promise.resolve([])),
      saveEmail: vi.fn(() => Promise.resolve()),
      saveEmailNotify: vi.fn(() => Promise.resolve()),
      saveEmailSchedule: vi.fn(() => Promise.resolve()),
    })
  })

  it('用户打开的开关，不被迟到的加载响应覆盖，且按「开」提交', async () => {
    const wrapper = await mountPage()
    await flushPromises()

    // 用户打开「启用按月定时发信」
    const cb = wrapper.find('input[type="checkbox"]')
    await cb.setValue(true)
    await nextTick()

    // 加载响应此刻才回来，带的是服务器旧值（关闭）
    scheduleLoads.forEach((resolve) => resolve({ enabled: false, day: 1, time: '09:00', config: {} }))
    await flushPromises()
    await nextTick()

    expect(wrapper.find('input[type="checkbox"]').element.checked).toBe(true)

    // 防抖 800ms 后提交，提交的是「开」
    await wait(900)
    expect(adminApi.saveEmailSchedule).toHaveBeenCalled()
    expect(adminApi.saveEmailSchedule.mock.calls.at(-1)[0].enabled).toBe(true)
  })

  it('保存完成之后才到达的旧加载响应，不覆盖也不再回写旧值', async () => {
    const wrapper = await mountPage()
    await flushPromises()

    const cb = wrapper.find('input[type="checkbox"]')
    await cb.setValue(true)
    await nextTick()

    // 先让防抖保存跑完
    await wait(900)
    expect(adminApi.saveEmailSchedule).toHaveBeenCalled()
    const saveCallsAfterCommit = adminApi.saveEmailSchedule.mock.calls.length

    // 之前那次加载的响应此刻才回来（旧值：关）
    scheduleLoads.forEach((resolve) => resolve({ enabled: false, day: 1, time: '09:00', config: {} }))
    await flushPromises()
    await nextTick()
    await wait(900)

    expect(wrapper.find('input[type="checkbox"]').element.checked).toBe(true)
    // 没有被旧数据带回一次「关」的提交
    expect(adminApi.saveEmailSchedule.mock.calls.length).toBe(saveCallsAfterCommit)
  })
})
