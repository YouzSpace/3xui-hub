import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

/**
 * 系统监控页：定时任务每次运行的 status + error。
 *
 * 后端 scheduled_task_runs 里 error 最长 1000 字（internal/task/schedule.go 截断），
 * 整段铺出来会把行高撑爆，所以默认折叠、可展开；非 success 的那次运行整行标红，
 * 翻「最近运行记录」时不用逐条读状态徽章。
 */

const { adminApi, toast } = vi.hoisted(() => ({
  adminApi: {},
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}))
vi.mock('@/api/admin', () => ({ adminApi }))
vi.mock('@/components/ui/Toast.vue', () => ({ useToast: () => toast }))

import SystemStatus from '@/views/admin/SystemStatus.vue'

const LONG_ERR =
  'dial tcp 10.0.0.9:2053: i/o timeout（节点 hk01 面板接口 3 次重试均失败，' +
  '本轮流量同步跳过该节点，已用流量会滞后到下一轮，请检查节点 2053 端口与安全组策略）'

const STATUS = {
  tasks: {
    events: [
      {
        command: 'traffic-sync', expression: '*/5 * * * *',
        next_run: '2026-11-07T03:00:00.000000Z', last_run: '2026-11-07T02:55:00.000000Z',
        last_status: 'failed', last_error: LONG_ERR, last_dur_ms: 0, has_mutex: true,
      },
      {
        command: 'ban-check', expression: '*/5 * * * *',
        next_run: '2026-11-07T03:00:00.000000Z', last_run: '2026-11-07T02:55:00.000000Z',
        last_status: 'finished', last_error: null, last_dur_ms: 812, has_mutex: true,
      },
    ],
    recent: [
      { command: 'traffic-sync', status: 'failed', error: LONG_ERR, duration_ms: 0, ran_at: '2026-11-07T02:55:00.000000Z' },
      { command: 'ban-check', status: 'finished', error: null, duration_ms: 812, ran_at: '2026-11-07T02:50:00.000000Z' },
    ],
  },
  worker: { alive: true, last_seen: '2026-11-07T02:56:00.000000Z', pending: 0, running: 0, failed_count: 0, recent_failed: [] },
}

beforeEach(() => {
  vi.clearAllMocks()
  adminApi.getSystemStatus = vi.fn(() => Promise.resolve(STATUS))
  adminApi.listAsyncTasks = vi.fn(() => Promise.resolve([]))
})

let wrapper = null
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
})

const mountPage = async () => {
  wrapper = mount(SystemStatus, { attachTo: document.body })
  await flushPromises()
  return wrapper
}

describe('SystemStatus.vue — 最近运行记录的状态与失败原因', () => {
  it('每次运行都给出 status；非 success 的那次整行标红', async () => {
    const w = await mountPage()
    const items = w.findAll('.sys-recent-item')
    expect(items).toHaveLength(2)

    const failed = items[0]
    expect(failed.classes()).toContain('is-failed')
    expect(failed.text()).toContain('失败')
    // 原始 status 也要露出来（不是只翻成中文）
    expect(failed.find('.sys-recent-meta').text()).toContain('failed')

    const ok = items[1]
    expect(ok.classes()).not.toContain('is-failed')
    expect(ok.text()).toContain('正常')
    expect(ok.find('.sys-recent-meta').text()).toContain('finished')
  })

  it('失败原因默认截断到 80 字，点「展开」看全文', async () => {
    const w = await mountPage()
    const err = w.findAll('.sys-recent-item')[0].find('.sys-recent-err')

    expect(err.find('.ex-text').text()).toBe(`${LONG_ERR.slice(0, 80)}…`)
    expect(err.find('.ex-btn').text()).toBe('展开')

    await err.find('.ex-btn').trigger('click')
    expect(err.find('.ex-text').text()).toBe(LONG_ERR)
    expect(err.find('.ex-btn').text()).toBe('收起')
  })

  it('成功的那次运行不渲染失败原因', async () => {
    const w = await mountPage()
    const ok = w.findAll('.sys-recent-item')[1]
    expect(ok.find('.sys-recent-err').exists()).toBe(false)
  })
})

describe('SystemStatus.vue — 定时任务表的上次运行状态', () => {
  it('失败任务标红并展示折叠后的错误，可展开全文', async () => {
    const w = await mountPage()
    const row = w.findAll('.sys-table tbody tr').find((tr) => tr.text().includes('traffic-sync'))
    const cell = row.find('.sys-status-cell')

    expect(cell.text()).toContain('失败')
    expect(cell.find('.ex-text').text()).toBe(`${LONG_ERR.slice(0, 80)}…`)

    await cell.find('.ex-btn').trigger('click')
    expect(cell.find('.ex-text').text()).toBe(LONG_ERR)
  })

  it('成功任务显示正常且不渲染报错', async () => {
    const w = await mountPage()
    const row = w.findAll('.sys-table tbody tr').find((tr) => tr.text().includes('ban-check'))
    const cell = row.find('.sys-status-cell')

    expect(cell.text()).toContain('正常')
    expect(cell.find('.ex-text').exists()).toBe(false)
  })
})
